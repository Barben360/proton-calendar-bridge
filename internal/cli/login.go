package cli

import (
	"context"
	"errors"
	"fmt"
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
)

// loginFlags are the flags of login and login resume.
type loginFlags struct {
	username   string
	passwordFD int
	unattended bool
}

func addLoginFlags(c *cobra.Command, f *loginFlags) {
	c.Flags().StringVar(&f.username, "username", "", "log in as the Proton user `NAME` (without it: $PCB_USERNAME, else a prompt)")
	// The default is never read (passwordFDSet comes from Changed): 0 keeps
	// the help free of a sentinel default (ux Minor).
	c.Flags().IntVar(&f.passwordFD, "password-fd", 0, "read the password from file descriptor `N` until end of input (without it: $PCB_PASSWORD, else a prompt)")
	c.Flags().BoolVar(&f.unattended, "unattended-relogin", false, "store the credentials so pcb can log in again without you (needs PCB_TOTP as an otpauth:// URI with 2FA)")
}

func (a *app) newLogin() *cobra.Command {
	var f loginFlags
	c := &cobra.Command{
		Use:   "login",
		Short: "Log in to Proton (password from the prompt, --password-fd or PCB_PASSWORD; 2FA code from PCB_TOTP or the prompt)",
		Long: "Log in to Proton. When pcb serve runs, the login goes through it, and a login waiting for its 2FA code " +
			"stays in the daemon for 5 minutes (pcb login resume --totp finishes it); otherwise it runs on its own.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.login(cmd, f, "") },
	}
	addLoginFlags(c, &f)

	var rf loginFlags
	var token string
	var totp bool
	resume := &cobra.Command{
		Use:   "resume (--captcha-token TOKEN | --totp)",
		Short: "Log in again with the token of a CAPTCHA you solved, or send the 2FA code of the login pcb serve holds",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (token != "") == totp {
				return UsageError(fmt.Errorf("pass exactly one of --captcha-token TOKEN and --totp"))
			}
			if totp {
				// The daemon holds the login these flags made (ruling M5-R12).
				for _, name := range []string{"username", "password-fd", "unattended-relogin"} {
					if cmd.Flags().Changed(name) {
						return UsageError(fmt.Errorf("--%s does not apply to --totp: the daemon holds the login it made", name))
					}
				}
				return a.resumeTOTP(cmd)
			}
			return a.login(cmd, rf, token)
		},
	}
	addLoginFlags(resume, &rf)
	resume.Flags().StringVar(&token, "captcha-token", "", "token of the CAPTCHA, as printed by pcb login")
	resume.Flags().BoolVar(&totp, "totp", false, "send the 2FA code (from $PCB_TOTP, else asked) of the login pcb serve holds")
	c.AddCommand(resume)
	return c
}

// interactive reports whether cmd may prompt: the terminal can answer and
// --non-interactive is not set.
func (a *app) interactive(cmd *cobra.Command) bool {
	off, _ := cmd.Flags().GetBool("non-interactive")
	return a.Term.Interactive() && !off
}

// loginAPI is what pcb login needs of the service: the in-process one, or
// the daemon's over its control socket.
type loginAPI interface {
	Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error)
	SubmitTOTP(ctx context.Context, code string) (service.LoginResult, error)
	Status(ctx context.Context) (service.Status, error)
}

// daemonLogin is the daemon's loginAPI.
type daemonLogin struct{ c *ctl.Client }

func (d daemonLogin) Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error) {
	return d.c.Login(ctx, req)
}

func (d daemonLogin) SubmitTOTP(ctx context.Context, code string) (service.LoginResult, error) {
	return d.c.SubmitTOTP(ctx, code)
}

func (d daemonLogin) Status(ctx context.Context) (service.Status, error) {
	st, err := d.c.Status(ctx)
	return st.Status, err
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
		svc = daemonLogin{c}
	} else {
		// The service first: a bad config.toml fails before any prompt.
		local, err := a.service(cmd)
		if err != nil {
			return Finish(cmd, nil, "", err)
		}
		defer a.closeService(local)
		a.noDaemonNote(cmd)
		svc = local
	}
	creds, err := a.credentials(ctx, credentialSource{
		username: f.username, passwordFD: f.passwordFD, passwordFDSet: cmd.Flags().Changed("password-fd"), interactive: interactive,
	})
	if err != nil {
		return Finish(cmd, nil, "", err)
	}

	req := service.LoginRequest{
		Username: creds.username, Password: creds.password, TOTP: creds.totp,
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
	switch {
	case err == nil && res.State == service.StateNeedsCaptcha:
		return Finish(cmd, res, "", captchaError(res))
	case err == nil && res.State == service.StateNeedsTOTP && !interactive && c != nil:
		return Finish(cmd, res, "", heldTOTPError(res))
	case err == nil && res.State == service.StateNeedsTOTP && !interactive:
		return Finish(cmd, nil, "", mustAct("the account needs a 2FA code: set PCB_TOTP (a code, or the otpauth:// URI)",
			"run pcb login again with PCB_TOTP set; the login was cancelled"))
	case err == nil && res.State == service.StateNeedsTOTP:
		res, err = a.promptTOTP(cmd, svc, nil)
	case err != nil && interactive && waitingForTOTP(cmd, svc):
		res, err = a.promptTOTP(cmd, svc, err)
	}
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	return loggedIn(cmd, res)
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
	hint := "PCB_TOTP=<code> pcb login resume --totp"
	if res.LoginExpires != nil {
		hint += " before " + res.LoginExpires.Local().Format(time.TimeOnly)
	}
	return mustAct("the account needs a 2FA code", hint+" (the daemon holds the login)")
}

// resumeTOTP is pcb login resume --totp: the 2FA code, from PCB_TOTP or
// the prompt, for the login the daemon holds (ruling M5-P11: never argv).
func (a *app) resumeTOTP(cmd *cobra.Command) error {
	ctx := cmd.Context()
	c, err := a.daemon(cmd)
	switch {
	case err != nil:
		return Finish(cmd, nil, "", err)
	case c == nil:
		return Finish(cmd, nil, "", mustAct("no pcb daemon holds a login waiting for its code", "run pcb login with PCB_TOTP set"))
	}
	defer func() { _ = c.Close() }()
	svc := daemonLogin{c}
	var res service.LoginResult
	switch code := a.Env("PCB_TOTP"); {
	case code != "":
		res, err = svc.SubmitTOTP(ctx, code)
	case a.interactive(cmd):
		res, err = a.promptTOTP(cmd, svc, nil)
	default:
		err = mustAct("no 2FA code: set PCB_TOTP (a code, or the otpauth:// URI)",
			"PCB_TOTP=<code> pcb login resume --totp, or run it in a terminal to be asked")
	}
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	return loggedIn(cmd, res)
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

// waitForCaptcha shows the CAPTCHA URL and waits for Enter.
func (a *app) waitForCaptcha(ctx context.Context, res service.LoginResult) error {
	fmt.Fprintf(a.Stderr, "Proton asks for a CAPTCHA. Solve it in a browser:\n  %s\n", res.CaptchaURL)
	if _, err := a.Term.ReadLine(ctx, "Press Enter once solved: "); err != nil {
		return noAnswer(ctx, "answer to the CAPTCHA prompt", err)
	}
	return nil
}

// waitingForTOTP reports whether the service still holds a login waiting for
// its 2FA code (a rejected code leaves it so, up to its tries).
func waitingForTOTP(cmd *cobra.Command, svc loginAPI) bool {
	st, err := svc.Status(cmd.Context())
	return err == nil && st.Phase == string(service.StateNeedsTOTP)
}

// promptTOTP asks for the 2FA code, again while the service keeps the login
// waiting, at most maxTOTPPrompts times. failure is what stopped the last try.
func (a *app) promptTOTP(cmd *cobra.Command, svc loginAPI, failure error) (service.LoginResult, error) {
	for range maxTOTPPrompts {
		if failure != nil {
			a.reportFailure(failure)
		}
		code, err := a.Term.ReadSecret(cmd.Context(), "2FA code (or otpauth:// URI): ")
		if err != nil {
			return service.LoginResult{}, noAnswer(cmd.Context(), "2FA code", err)
		}
		var res service.LoginResult
		if res, failure = svc.SubmitTOTP(cmd.Context(), code); failure == nil {
			return res, nil
		}
		if !waitingForTOTP(cmd, svc) {
			break
		}
	}
	return service.LoginResult{}, failure
}

// reportFailure prints err and its hint on stderr, as Run does for a failure.
func (a *app) reportFailure(err error) {
	fmt.Fprintf(a.Stderr, "Error: %s\n", err)
	if hint := exitcode.HintOf(err); hint != "" {
		fmt.Fprintf(a.Stderr, "Hint: %s\n", hint)
	}
}
