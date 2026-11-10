// Command pcb-lab is the experiment harness: it logs into the dedicated
// Proton test account so experiments can run. It is never shipped.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

func main() {
	os.Exit(int(execute(os.Args[1:], os.Stdout, os.Stderr)))
}

type reported struct{ error }

func (e *reported) Unwrap() error { return e.error }

func usage(err error) error { return &exitcode.Error{Code: exitcode.Usage, Err: err} }

func execute(args []string, stdout, stderr io.Writer) exitcode.Code {
	root := &cobra.Command{
		Use:           "pcb-lab",
		Short:         "Experiment harness for the Proton test account (never shipped)",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usage(fmt.Errorf("unknown command %q", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().Bool("json", false, "print one JSON envelope instead of text")
	root.PersistentFlags().String("state-dir", "", "lab state directory (default $XDG_STATE_HOME/pcb-lab)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usage(err) })
	root.AddCommand(loginCmd(), whoamiCmd(), probeCmd())
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	cmd, err := root.ExecuteC()
	if err == nil {
		return exitcode.OK
	}
	var r *reported
	if !errors.As(err, &r) {
		if jsonWanted(args) {
			_ = output.Write(stdout, cmd.Name(), nil, err)
		} else {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			var e *exitcode.Error
			if errors.As(err, &e) && e.Hint != "" {
				fmt.Fprintf(stderr, "Hint: %s\n", e.Hint)
			}
		}
	}
	return exitcode.From(err)
}

func jsonWanted(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	return false
}

// stateDir is --state-dir, else $XDG_STATE_HOME/pcb-lab (~/.local/state/pcb-lab).
func stateDir(cmd *cobra.Command) (string, error) {
	if d, _ := cmd.Flags().GetString("state-dir"); d != "" {
		return d, nil
	}
	dirs, err := paths.Resolve(os.Getenv)
	if err != nil {
		return "", &exitcode.Error{Code: exitcode.Blocked, Err: err}
	}
	return filepath.Join(filepath.Dir(dirs.State), "pcb-lab"), nil
}

func options(cmd *cobra.Command) (session.Options, error) {
	dir, err := stateDir(cmd)
	if err != nil {
		return session.Options{}, err
	}
	// PCB_LAB_BASEURL points the lab at a fake Proton in tests.
	return session.Options{BaseURL: os.Getenv("PCB_LAB_BASEURL"), AppVersion: os.Getenv("PCB_LAB_APPVERSION"), StateDir: dir}, nil
}

// finish prints data (or the failure) the way the command's mode wants.
func finish(cmd *cobra.Command, name string, data any, text string, err error) error {
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		if werr := output.Write(cmd.OutOrStdout(), name, data, err); werr != nil {
			return werr
		}
		if err != nil {
			return &reported{err}
		}
		return nil
	}
	if err == nil {
		fmt.Fprintln(cmd.OutOrStdout(), text)
	}
	return err
}

type loginData struct {
	UID    string   `json:"uid"`
	Scopes []string `json:"scopes"`
}

func loginCmd() *cobra.Command {
	var captcha string
	c := &cobra.Command{
		Use:   "login",
		Short: "Log into the test account (credentials from PCB_LAB_USERNAME, PCB_LAB_PASSWORD, PCB_LAB_TOTP)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return finish(cmd, "login", nil, "", err)
			}
			o.CaptchaToken = captcha
			o.Creds.Username = os.Getenv("PCB_LAB_USERNAME")
			o.Creds.Password = os.Getenv("PCB_LAB_PASSWORD")
			if raw := os.Getenv("PCB_LAB_TOTP"); raw != "" {
				t, perr := otp.Parse(raw)
				if perr != nil {
					return finish(cmd, "login", nil, "", &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("PCB_LAB_TOTP: %w", perr)})
				}
				o.Creds.TOTP = t
			}
			s, err := session.Login(cmd.Context(), o)
			if err != nil {
				return finish(cmd, "login", nil, "", err)
			}
			defer s.Close()
			return finish(cmd, "login", loginData{session.RedactID(s.UID), s.Scopes},
				fmt.Sprintf("logged in; session %s; %d scopes: %v", session.RedactID(s.UID), len(s.Scopes), s.Scopes), nil)
		},
	}
	c.Flags().StringVar(&captcha, "captcha-token", "", "token of a solved human verification (from hv.json)")
	return c
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the saved session's scopes and a redacted ID",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return finish(cmd, "whoami", nil, "", err)
			}
			s, err := session.Resume(cmd.Context(), o)
			if err != nil {
				return finish(cmd, "whoami", nil, "", err)
			}
			defer s.Close()
			return finish(cmd, "whoami", loginData{session.RedactID(s.UID), s.Scopes},
				fmt.Sprintf("session %s; %d scopes: %v", session.RedactID(s.UID), len(s.Scopes), s.Scopes), nil)
		},
	}
}
