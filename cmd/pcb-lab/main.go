// Command pcb-lab is the experiment harness: it logs into the dedicated
// Proton test account so experiments can run. It is never shipped.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

func main() {
	os.Exit(int(execute(os.Args[1:], os.Stdout, os.Stderr)))
}

func newRoot() *cobra.Command {
	root := cli.Root("pcb-lab", "Experiment harness for the Proton test account (never shipped)",
		loginCmd(), whoamiCmd(), probeCmd(), seedCmd())
	root.PersistentFlags().String("state-dir", "", "lab state directory (default $XDG_STATE_HOME/pcb-lab)")
	return root
}

func execute(args []string, stdout, stderr io.Writer) exitcode.Code {
	return cli.Run(newRoot(), args, stdout, stderr)
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

type loginData struct {
	UID    string   `json:"uid"`
	Scopes []string `json:"scopes"`
}

func loginCmd() *cobra.Command {
	var captcha string
	c := &cobra.Command{
		Use:   "login",
		Short: "Log into the test account (credentials from PCB_LAB_USERNAME, PCB_LAB_PASSWORD, PCB_LAB_TOTP)",
		Args:  cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			o.CaptchaToken = captcha
			o.Creds.Username = os.Getenv("PCB_LAB_USERNAME")
			o.Creds.Password = os.Getenv("PCB_LAB_PASSWORD")
			if raw := os.Getenv("PCB_LAB_TOTP"); raw != "" {
				t, perr := otp.Parse(raw)
				if perr != nil {
					return cli.Finish(cmd, nil, "", cli.UsageError(fmt.Errorf("PCB_LAB_TOTP: %w", perr)))
				}
				o.Creds.TOTP = t
			}
			s, err := session.Login(cmd.Context(), o)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			defer s.Close()
			return cli.Finish(cmd, loginData{session.RedactID(s.UID), s.Scopes},
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
		Args:  cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			s, err := session.Resume(cmd.Context(), o)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			defer s.Close()
			return cli.Finish(cmd, loginData{session.RedactID(s.UID), s.Scopes},
				fmt.Sprintf("session %s; %d scopes: %v", session.RedactID(s.UID), len(s.Scopes), s.Scopes), nil)
		},
	}
}
