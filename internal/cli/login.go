package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

const (
	// maxCaptchaRounds is how many times an interactive login waits for a
	// solved CAPTCHA before giving up.
	maxCaptchaRounds = 3
	// maxTOTPPrompts is how many 2FA codes an interactive login asks for: the
	// tries the service allows.
	maxTOTPPrompts = 3
	// maxMailboxPrompts is how many mailbox passwords an interactive login
	// asks for: the tries the service allows.
	maxMailboxPrompts = 3
)

// loginFlags are the flags of login and login resume.
type loginFlags struct {
	username   string
	passwordFD int
	mailboxFD  int
	unattended bool
}

func addLoginFlags(c *cobra.Command, f *loginFlags) {
	c.Flags().StringVar(&f.username, "username", "", "log in as the Proton user `NAME` (without it: $PCB_USERNAME, else a prompt)")
	// The default is never read (passwordFDSet comes from Changed): 0 keeps
	// the help free of a sentinel default (ux Minor).
	c.Flags().IntVar(&f.passwordFD, "password-fd", 0, "read the password from file descriptor `N` until end of input (without it: $PCB_PASSWORD, else a prompt)")
	c.Flags().IntVar(&f.mailboxFD, "mailbox-password-fd", 0, "read the mailbox password of a two-password account from file descriptor `N` "+
		"until end of input (without it: $PCB_MAILBOX_PASSWORD, else a prompt when Proton asks)")
	c.Flags().BoolVar(&f.unattended, "unattended-relogin", false, "store the credentials so pcb can log in again without you (needs PCB_TOTP as an otpauth:// URI with 2FA)")
}

func (a *app) newLogin() *cobra.Command {
	var f loginFlags
	c := &cobra.Command{
		Use: "login",
		Short: "Log in to Proton (password from the prompt, --password-fd or PCB_PASSWORD; 2FA code from PCB_TOTP or the prompt; " +
			"mailbox password from --mailbox-password-fd, PCB_MAILBOX_PASSWORD or the prompt)",
		Long: "Log in to Proton. The username decides the account: a username pcb knows logs that account in again, a " +
			"new one adds an account (so pcb login takes no --account). An account in two-password mode also needs " +
			"its mailbox password, after the 2FA code; pcb never stores it. When pcb serve runs, the login goes " +
			"through it, and a login waiting for its 2FA code or its mailbox password stays in the daemon for 5 " +
			"minutes (pcb login resume --totp or --mailbox-password finishes it); otherwise it runs on its own.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.login(cmd, f, "") },
	}
	addLoginFlags(c, &f)

	var rf loginFlags
	var token string
	var totp, mailbox bool
	resume := selecting(&cobra.Command{
		Use:   "resume (--captcha-token TOKEN | --totp | --mailbox-password)",
		Short: "Log in again with the token of a CAPTCHA you solved, or send the 2FA code or the mailbox password of the login pcb serve holds",
		Long: "Finish a login: with --captcha-token, log in again with the token of the CAPTCHA you solved; with " +
			"--totp, send the 2FA code of the login pcb serve holds; with --mailbox-password, send its mailbox " +
			"password (from --mailbox-password-fd, else $PCB_MAILBOX_PASSWORD, else asked; never from the command " +
			"line). --account (or PCB_ACCOUNT) names the waiting login; without it, the only one. With " +
			"--captcha-token and no --username nor PCB_USERNAME, the username is the waiting account's address.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			modes := 0
			for _, on := range []bool{token != "", totp, mailbox} {
				if on {
					modes++
				}
			}
			if modes != 1 {
				return UsageError(fmt.Errorf("pass exactly one of --captcha-token TOKEN, --totp and --mailbox-password"))
			}
			if token != "" {
				return a.login(cmd, rf, token)
			}
			step, mode := totpStep, "--totp"
			if mailbox {
				step, mode = mailboxStep, "--mailbox-password"
			}
			// The daemon holds the login these flags made (ruling M5-R12).
			for _, name := range []string{"username", "password-fd", "unattended-relogin"} {
				if cmd.Flags().Changed(name) {
					return UsageError(fmt.Errorf("--%s does not apply to %s: the daemon holds the login it made", name, mode))
				}
			}
			return a.resumeHeld(cmd, step, rf)
		},
	})
	addLoginFlags(resume, &rf)
	resume.Flags().StringVar(&token, "captcha-token", "", "token of the CAPTCHA, as printed by pcb login")
	resume.Flags().BoolVar(&totp, "totp", false, "send the 2FA code (from $PCB_TOTP, else asked) of the login pcb serve holds")
	resume.Flags().BoolVar(&mailbox, "mailbox-password", false, "send the mailbox password (from --mailbox-password-fd, else "+
		"$PCB_MAILBOX_PASSWORD, else asked) of the login pcb serve holds")
	c.AddCommand(resume)
	return c
}

// interactive reports whether cmd may prompt: the terminal can answer and
// --non-interactive is not set.
func (a *app) interactive(cmd *cobra.Command) bool {
	off, _ := cmd.Flags().GetBool("non-interactive")
	return a.Term.Interactive() && !off
}

// loginAPI is what pcb login needs of the accounts: the in-process Hub, or
// the daemon's over its control socket. SubmitTOTP, SubmitMailboxPassword
// and Status act on the account of the last Login (its local ID once Login
// named it, else its username), or on the selector the command was given.
type loginAPI interface {
	Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error)
	SubmitTOTP(ctx context.Context, code string) (service.LoginResult, error)
	SubmitMailboxPassword(ctx context.Context, password string) (service.LoginResult, error)
	Status(ctx context.Context) (service.Status, error)
	Accounts(ctx context.Context) ([]service.AccountInfo, error)
}

// hubLogin is the in-process loginAPI.
type hubLogin struct {
	h   Hub
	sel string
}

func (l *hubLogin) Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error) {
	l.sel = req.Username
	res, err := l.h.Login(ctx, req)
	if res.Account != "" {
		l.sel = res.Account
	}
	return res, err
}

func (l *hubLogin) SubmitTOTP(ctx context.Context, code string) (service.LoginResult, error) {
	return l.h.SubmitTOTP(ctx, l.sel, code)
}

func (l *hubLogin) SubmitMailboxPassword(ctx context.Context, password string) (service.LoginResult, error) {
	return l.h.SubmitMailboxPassword(ctx, l.sel, password)
}

func (l *hubLogin) Status(ctx context.Context) (service.Status, error) {
	api, _, err := l.h.Account(l.sel)
	if err != nil {
		return service.Status{}, err
	}
	return api.Status(ctx)
}

func (l *hubLogin) Accounts(context.Context) ([]service.AccountInfo, error) {
	return l.h.Accounts(), nil
}

// daemonLogin is the daemon's loginAPI.
type daemonLogin struct {
	c   *ctl.Client
	sel string
}

func (d *daemonLogin) Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error) {
	d.sel = req.Username
	res, err := d.c.Login(ctx, req)
	if res.Account != "" {
		d.sel = res.Account
	}
	return res, err
}

func (d *daemonLogin) SubmitTOTP(ctx context.Context, code string) (service.LoginResult, error) {
	if err := d.pick(ctx, totpStep); err != nil {
		return service.LoginResult{}, err
	}
	return d.c.SubmitTOTP(ctx, d.sel, code)
}

func (d *daemonLogin) SubmitMailboxPassword(ctx context.Context, password string) (service.LoginResult, error) {
	if err := d.pick(ctx, mailboxStep); err != nil {
		return service.LoginResult{}, err
	}
	return d.c.SubmitMailboxPassword(ctx, d.sel, password)
}

// pick sets the selector, when none is set, to the daemon's only login
// waiting at step.
func (d *daemonLogin) pick(ctx context.Context, step heldStep) error {
	if d.sel != "" {
		return nil
	}
	id, err := waitingAt(ctx, d.c, step)
	if err != nil {
		return err
	}
	d.sel = id
	return nil
}

func (d *daemonLogin) Status(ctx context.Context) (service.Status, error) {
	st, err := d.c.Status(ctx)
	if err != nil {
		return service.Status{}, err
	}
	infos := make([]service.AccountInfo, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		infos = append(infos, a.AccountInfo)
	}
	info, err := service.SelectAccount(infos, d.sel)
	if err != nil {
		return service.Status{}, err
	}
	for _, a := range st.Accounts {
		if a.ID == info.ID {
			return a.Login, nil
		}
	}
	return service.Status{}, nil
}

func (d *daemonLogin) Accounts(ctx context.Context) ([]service.AccountInfo, error) {
	return d.c.Accounts(ctx)
}

// waitingAt is the local ID of the daemon's only login waiting at step (as
// the Hub's submits choose with no selector). None there, but one at the
// other step: the refusal names the resume of that step.
func waitingAt(ctx context.Context, c *ctl.Client, step heldStep) (string, error) {
	list, err := c.Accounts(ctx)
	if err != nil {
		return "", err
	}
	var waiting []service.AccountInfo
	var elsewhere []heldStep
	for _, info := range list {
		other, held := stepAt(info.Phase)
		switch {
		case !held:
		case other == step:
			waiting = append(waiting, info)
		default:
			elsewhere = append(elsewhere, other)
		}
	}
	switch {
	case len(waiting) == 1:
		return waiting[0].ID, nil
	case len(waiting) > 1:
		return "", service.SeveralAccounts(waiting)
	case len(elsewhere) == 1:
		return "", wrongStep(step, elsewhere[0])
	}
	return "", mustAct("no login is waiting for a "+step.noun, "run pcb login first")
}

// wrongStep is the refusal of a resume for step while the login waits at
// other: it names the resume of other.
func wrongStep(step, other heldStep) error {
	return mustAct("no login is waiting for a "+step.noun, "the login waits for its "+other.noun+": run "+other.resume)
}

// heldStep is a step at which a login waits for a secret: its 2FA code, or
// the mailbox password of a two-password account.
type heldStep struct {
	state  service.LoginState
	noun   string // what the step waits for, in messages
	prompt string
	tries  int    // how many an interactive login asks for
	resume string // the command that sends it to the login the daemon holds
}

var (
	totpStep = heldStep{state: service.StateNeedsTOTP, noun: "2FA code", prompt: "2FA code (or otpauth:// URI): ",
		tries: maxTOTPPrompts, resume: "pcb login resume --totp"}
	mailboxStep = heldStep{state: service.StateNeedsMailboxPassword, noun: "mailbox password", prompt: "Mailbox password: ",
		tries: maxMailboxPrompts, resume: "pcb login resume --mailbox-password"}
)

// stepAt is the step a login in phase waits at; false if none.
func stepAt(phase string) (heldStep, bool) {
	switch service.LoginState(phase) {
	case service.StateNeedsTOTP:
		return totpStep, true
	case service.StateNeedsMailboxPassword:
		return mailboxStep, true
	}
	return heldStep{}, false
}

// submit sends secret to the login waiting at s.
func (s heldStep) submit(ctx context.Context, svc loginAPI, secret string) (service.LoginResult, error) {
	if s == mailboxStep {
		return svc.SubmitMailboxPassword(ctx, secret)
	}
	return svc.SubmitTOTP(ctx, secret)
}

func (a *app) login(cmd *cobra.Command, f loginFlags, captchaToken string) error {
	ctx := cmd.Context()
	interactive := a.interactive(cmd)
	c, err := a.daemon(cmd)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	var svc loginAPI
	if c != nil {
		defer func() { _ = c.Close() }()
		svc = &daemonLogin{c: c}
	} else {
		// The accounts first: a bad config.toml fails before any prompt.
		h, err := a.hub(cmd)
		if err != nil {
			return Finish(cmd, nil, "", err)
		}
		defer a.closeHub(h)
		a.noDaemonNote(cmd)
		svc = &hubLogin{h: h}
	}
	username := f.username
	if captchaToken != "" {
		if username, err = a.resumeUsername(cmd, svc, f.username); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	creds, err := a.credentials(ctx, credentialSource{
		username: username, passwordFD: f.passwordFD, passwordFDSet: cmd.Flags().Changed("password-fd"),
		mailboxFD: f.mailboxFD, mailboxFDSet: cmd.Flags().Changed("mailbox-password-fd"), interactive: interactive,
	})
	if err != nil {
		return Finish(cmd, nil, "", err)
	}

	req := service.LoginRequest{
		Username: creds.username, Password: creds.password, TOTP: creds.totp, MailboxPassword: creds.mailbox,
		CaptchaToken: captchaToken, UnattendedRelogin: f.unattended,
	}
	res, err := svc.Login(ctx, req)
	for round := 0; err == nil && res.State == service.StateNeedsCaptcha && interactive && round < maxCaptchaRounds; round++ {
		if werr := a.waitForCaptcha(ctx, res); werr != nil {
			// Keep the way back: the solved token is still good for pcb login resume.
			return Finish(cmd, res, "", withHint(werr, resumeHint(res)))
		}
		req.CaptchaToken = res.CaptchaToken
		res, err = svc.Login(ctx, req)
	}
	if err == nil && res.State == service.StateNeedsCaptcha {
		return Finish(cmd, res, "", captchaError(res))
	}
	// An upfront mailbox password went with the request: the service keeps
	// it across the 2FA step.
	return a.finishLogin(cmd, svc, heldOptions{interactive: interactive, daemon: c != nil}, res, err)
}

// finishLogin takes the login through the steps it is held at, then
// reports it: logged in, or the failure (with the held login as its data).
func (a *app) finishLogin(cmd *cobra.Command, svc loginAPI, o heldOptions, res service.LoginResult, err error) error {
	res, err = a.finishHeld(cmd, svc, o, res, err)
	if err != nil {
		var data any
		if res.State != "" {
			data = res
		}
		return Finish(cmd, data, "", err)
	}
	return loggedIn(cmd, res)
}

// heldOptions say how finishHeld may carry a login through its steps.
type heldOptions struct {
	interactive bool   // it may prompt
	daemon      bool   // the daemon holds the login: a resume can finish it
	mailbox     string // a mailbox password given ahead, not yet sent
}

// finishHeld carries a login through the steps it is held at, from res and
// err (the outcome of the last call): its 2FA code, then the mailbox
// password of a two-password account. Interactive, it asks for each, again
// while the service keeps the login waiting at that step after a rejected
// one, at most the step's tries; else the step is an exit 10 naming how to
// finish it. On a failure the result, when set, is the held login (the
// --json data).
func (a *app) finishHeld(cmd *cobra.Command, svc loginAPI, o heldOptions, res service.LoginResult, err error) (service.LoginResult, error) {
	ctx := cmd.Context()
	asked := map[service.LoginState]int{}
	for {
		h, ok := heldAt(cmd, svc, res, err)
		if !ok {
			return res, err
		}
		step, failure := h.step, h.failure
		switch {
		case step == mailboxStep && failure == nil && o.mailbox != "":
			pw := o.mailbox
			o.mailbox = ""
			res, err = svc.SubmitMailboxPassword(ctx, pw)
			continue
		case !o.interactive:
			return notAsked(h, o.daemon)
		case asked[step.state] == step.tries:
			if failure == nil {
				failure = mustAct("the login still waits for its "+step.noun, "run pcb login again")
			}
			return service.LoginResult{}, failure
		case failure != nil:
			a.reportFailure(failure)
		}
		asked[step.state]++
		secret, perr := a.Term.ReadSecret(ctx, step.prompt)
		if perr != nil {
			return service.LoginResult{}, noAnswer(ctx, step.noun, perr)
		}
		res, err = step.submit(ctx, svc, secret)
	}
}

// heldLogin is a login held at step: its result, and failure, what
// stopped its last try (nil on a fresh step).
type heldLogin struct {
	step    heldStep
	failure error
	result  service.LoginResult
}

// heldAt is the login held after res and err: at the state of res, or,
// after a failure (a rejected 2FA code, a mailbox password that does not
// unlock the keys), at the phase the service keeps it in, until when it
// ends. false when it waits for nothing more.
func heldAt(cmd *cobra.Command, svc loginAPI, res service.LoginResult, err error) (heldLogin, bool) {
	if err == nil {
		step, ok := stepAt(string(res.State))
		return heldLogin{step: step, result: res}, ok
	}
	st, serr := svc.Status(cmd.Context())
	if serr != nil {
		return heldLogin{}, false
	}
	step, ok := stepAt(st.Phase)
	return heldLogin{step: step, failure: err, result: service.LoginResult{State: step.state, LoginExpires: st.LoginExpires}}, ok
}

// notAsked is the exit 10 of the held login h that cannot prompt: with a
// daemon, it holds the login (h's result, the --json data) and the hint
// names the resume and until when; without one, the login ends with the
// command. A rejected 2FA code is reported as it is.
func notAsked(h heldLogin, daemon bool) (service.LoginResult, error) {
	step, failure, held, expires := h.step, h.failure, h.result, h.result.LoginExpires
	switch {
	case step == totpStep && failure != nil:
		return service.LoginResult{}, failure
	case step == totpStep && daemon:
		return held, heldTOTPError(held)
	case step == totpStep:
		return service.LoginResult{}, mustAct("the account needs a 2FA code: set PCB_TOTP (a code, or the otpauth:// URI)",
			"run pcb login again with PCB_TOTP set; the login was cancelled")
	case failure != nil && daemon:
		hint := heldHint(mailboxStep.resume, expires)
		if h := exitcode.HintOf(failure); h != "" {
			hint = h + ": " + hint
		}
		return held, withHint(failure, hint)
	case daemon:
		return held, mustAct("the account needs its mailbox password", heldHint(mailboxStep.resume, expires))
	case failure != nil:
		return service.LoginResult{}, withHint(failure,
			"run pcb login again with the right mailbox password in PCB_MAILBOX_PASSWORD; the login was cancelled")
	}
	return service.LoginResult{}, mustAct("the account uses two-password mode: set PCB_MAILBOX_PASSWORD or pass --mailbox-password-fd",
		"run pcb login again with PCB_MAILBOX_PASSWORD set; the login was cancelled")
}

// heldHint names cmd, the command that finishes the login the daemon
// holds, and until when.
func heldHint(cmd string, expires *time.Time) string {
	if expires != nil {
		cmd += " before " + expires.Local().Format(time.TimeOnly)
	}
	return cmd + " (the daemon holds the login)"
}

// resumeUsername is the username pcb login resume --captcha-token logs in
// when the selector names an account, or when no username is given
// (--username, PCB_USERNAME): the address of the login waiting for a
// CAPTCHA, the selected one or else the only one. "" leaves the username
// to the usual sources.
func (a *app) resumeUsername(cmd *cobra.Command, svc loginAPI, flagUser string) (string, error) {
	sel := a.selector(cmd)
	if sel == "" && (flagUser != "" || a.Env("PCB_USERNAME") != "") {
		return flagUser, nil
	}
	list, err := svc.Accounts(cmd.Context())
	if err != nil {
		return "", err
	}
	captcha := func(info service.AccountInfo) bool { return info.Phase == string(service.StateNeedsCaptcha) }
	var info service.AccountInfo
	if sel != "" {
		if info, err = service.SelectAccount(list, sel); err != nil {
			return "", err
		}
		if !captcha(info) {
			return "", mustAct("no login of "+accountName(info)+" waits for a CAPTCHA", "run pcb login")
		}
	} else {
		var waiting []service.AccountInfo
		for _, i := range list {
			if captcha(i) {
				waiting = append(waiting, i)
			}
		}
		switch len(waiting) {
		case 0:
			return "", nil
		case 1:
			info = waiting[0]
		default:
			return "", service.SeveralAccounts(waiting)
		}
	}
	if flagUser != "" && info.Address != "" && !strings.EqualFold(flagUser, info.Address) {
		return "", UsageError(fmt.Errorf("--username %s is not the address of the account %s", flagUser, accountName(info)))
	}
	if info.Address == "" {
		return flagUser, nil
	}
	return info.Address, nil
}

func loggedIn(cmd *cobra.Command, res service.LoginResult) error {
	text := fmt.Sprintf("Logged in as %s (account %s)", res.Username, res.AccountID)
	if res.DefaultClientCreated {
		text += "\nCalDAV credentials: pcb client show"
	}
	return Finish(cmd, res, text, nil)
}

// heldTOTPError is the exit 10 of a login the daemon holds for its 2FA
// code: it names the command that sends it, and until when.
func heldTOTPError(res service.LoginResult) error {
	return mustAct("the account needs a 2FA code", heldHint("PCB_TOTP=<code> pcb login resume --totp", res.LoginExpires))
}

// resumeHeld is pcb login resume --totp and --mailbox-password: the 2FA
// code (PCB_TOTP, else the prompt) or the mailbox password
// (--mailbox-password-fd, else PCB_MAILBOX_PASSWORD, else the prompt) of
// the login the daemon holds (ruling M5-P11: never argv). A login that
// waits at the other step is refused before anything is asked or sent;
// after the 2FA code, the mailbox password follows (the one given ahead,
// else asked).
func (a *app) resumeHeld(cmd *cobra.Command, step heldStep, f loginFlags) error {
	ctx := cmd.Context()
	c, err := a.daemon(cmd)
	switch {
	case err != nil:
		return Finish(cmd, nil, "", err)
	case c == nil && step == totpStep:
		return Finish(cmd, nil, "", mustAct("no pcb daemon holds a login waiting for its code", "run pcb login with PCB_TOTP set"))
	case c == nil:
		return Finish(cmd, nil, "", mustAct("no pcb daemon holds a login waiting for its mailbox password",
			"run pcb login with PCB_MAILBOX_PASSWORD set"))
	}
	defer func() { _ = c.Close() }()
	svc := &daemonLogin{c: c, sel: a.selector(cmd)}
	if err := resumable(ctx, svc, step); err != nil {
		return Finish(cmd, nil, "", err)
	}
	mailbox, err := a.mailboxPassword(f.mailboxFD, cmd.Flags().Changed("mailbox-password-fd"))
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	interactive := a.interactive(cmd)
	var res service.LoginResult
	switch code := a.Env("PCB_TOTP"); {
	case step == totpStep && code != "":
		res, err = svc.SubmitTOTP(ctx, code)
	case step == mailboxStep && mailbox != "":
		res, err = svc.SubmitMailboxPassword(ctx, mailbox)
		mailbox = ""
	case interactive:
		res = service.LoginResult{State: step.state} // finishHeld asks for it
	case step == totpStep:
		return Finish(cmd, nil, "", mustAct("no 2FA code: set PCB_TOTP (a code, or the otpauth:// URI)",
			"PCB_TOTP=<code> pcb login resume --totp, or run it in a terminal to be asked"))
	default:
		return Finish(cmd, nil, "", mustAct("no mailbox password: set PCB_MAILBOX_PASSWORD or pass --mailbox-password-fd",
			"PCB_MAILBOX_PASSWORD=<password> pcb login resume --mailbox-password, or run it in a terminal to be asked"))
	}
	return a.finishLogin(cmd, svc, heldOptions{interactive: interactive, daemon: true, mailbox: mailbox}, res, err)
}

// resumable checks that the login the resume names waits at step: one at
// the other step is refused with the resume of that step.
func resumable(ctx context.Context, svc *daemonLogin, step heldStep) error {
	if svc.sel == "" {
		return svc.pick(ctx, step)
	}
	st, err := svc.Status(ctx)
	if err != nil {
		return err
	}
	if other, held := stepAt(st.Phase); held && other != step {
		return wrongStep(step, other)
	}
	return nil
}

// captchaError is the exit 10 of a CAPTCHA nobody solved: it names the URL
// and the command that resumes with the token.
func captchaError(res service.LoginResult) error {
	return mustAct("Proton asks for a CAPTCHA: solve it at "+res.CaptchaURL, resumeHint(res))
}

func resumeHint(res service.LoginResult) string {
	return "pcb login resume --captcha-token " + shellquote.Quote(res.CaptchaToken)
}

// withHint returns err with hint as its hint, keeping its exit code.
func withHint(err error, hint string) error {
	var ee *exitcode.Error
	if errors.As(err, &ee) {
		return &exitcode.Error{Code: ee.Code, Err: ee.Err, Hint: hint}
	}
	return &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: hint}
}

// waitForCaptcha shows the CAPTCHA URL and waits for Enter, then says the
// check is under way: Proton can take seconds to answer the retried login
// (observation O1).
func (a *app) waitForCaptcha(ctx context.Context, res service.LoginResult) error {
	fmt.Fprintf(a.Stderr, "Proton asks for a CAPTCHA. Solve it in a browser:\n  %s\n", res.CaptchaURL)
	if _, err := a.Term.ReadLine(ctx, "Press Enter once solved: "); err != nil {
		return noAnswer(ctx, "answer to the CAPTCHA prompt", err)
	}
	fmt.Fprintln(a.Stderr, "Checking the CAPTCHA with Proton…")
	return nil
}

// reportFailure prints err and its hint on stderr, as Run does for a failure.
func (a *app) reportFailure(err error) {
	fmt.Fprintf(a.Stderr, "Error: %s\n", err)
	if hint := exitcode.HintOf(err); hint != "" {
		fmt.Fprintf(a.Stderr, "Hint: %s\n", hint)
	}
}
