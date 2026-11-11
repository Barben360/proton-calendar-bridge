package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

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
	c.Flags().StringVar(&f.username, "username", "", "Proton username (default: $PCB_USERNAME, else asked)")
	c.Flags().IntVar(&f.passwordFD, "password-fd", -1, "read the password from this file descriptor until end of input (default: $PCB_PASSWORD, else asked)")
	c.Flags().BoolVar(&f.unattended, "unattended-relogin", false, "store the credentials so pcb can log in again without you (needs PCB_TOTP as an otpauth:// URI with 2FA)")
}

func (a *app) newLogin() *cobra.Command {
	var f loginFlags
	c := &cobra.Command{
		Use:   "login",
		Short: "Log in to Proton (password from the prompt, --password-fd or PCB_PASSWORD; 2FA code from PCB_TOTP or the prompt)",
		Args:  NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.login(cmd, f, "") },
	}
	addLoginFlags(c, &f)

	var rf loginFlags
	var token string
	resume := &cobra.Command{
		Use:   "resume --captcha-token TOKEN",
		Short: "Log in again with the token of a CAPTCHA you solved",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "" {
				return UsageError(fmt.Errorf("--captcha-token is required"))
			}
			return a.login(cmd, rf, token)
		},
	}
	addLoginFlags(resume, &rf)
	resume.Flags().StringVar(&token, "captcha-token", "", "token of the CAPTCHA, as printed by pcb login")
	c.AddCommand(resume)
	return c
}

// interactive reports whether cmd may prompt: the terminal can answer and
// --non-interactive is not set.
func (a *app) interactive(cmd *cobra.Command) bool {
	off, _ := cmd.Flags().GetBool("non-interactive")
	return a.Term.Interactive() && !off
}

func (a *app) login(cmd *cobra.Command, f loginFlags, captchaToken string) error {
	ctx := cmd.Context()
	interactive := a.interactive(cmd)
	// The service first: a bad config.toml fails before any prompt.
	svc, err := a.service(cmd)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	defer a.closeService(svc)
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
	return Finish(cmd, res, fmt.Sprintf("Logged in as %s (account %s)", res.Username, res.AccountID), nil)
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
func waitingForTOTP(cmd *cobra.Command, svc service.API) bool {
	st, err := svc.Status(cmd.Context())
	return err == nil && st.Phase == string(service.StateNeedsTOTP)
}

// promptTOTP asks for the 2FA code, again while the service keeps the login
// waiting, at most maxTOTPPrompts times. failure is what stopped the last try.
func (a *app) promptTOTP(cmd *cobra.Command, svc service.API, failure error) (service.LoginResult, error) {
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
