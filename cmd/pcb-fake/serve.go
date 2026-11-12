package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/seed"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// readyLine is the last line serve prints once the fake answers.
const readyLine = "Serving until SIGINT or SIGTERM"

func serveCmd() *cobra.Command {
	var listen, control, seedFile, accountFile string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the fake Proton on a loopback address until SIGINT or SIGTERM",
		Long: "Serve the fake Proton over plain HTTP on a loopback address, with one account read from --account " +
			"(a 0600 TOML file: username, password, totp_secret; 2FA when totp_secret is set) and, with --seed, the " +
			"calendars and events of the data set file (testdata/seed/dataset.yaml), plus an event with attendees " +
			"and a calendar shared read-only. It prints the base URL (pcb's [proton] base_url), the username and the " +
			"control URL, whose POST /revoke-sessions ends every session (as a revoke in the web app does) and POST " +
			"/expire-access-tokens makes every access token answer 401 until refreshed.",
		Args: cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cli.JSONMode(cmd) {
				return cli.UsageError(errors.New("--json cannot be combined with serve: it runs until stopped"))
			}
			return serve(cmd, listen, control, seedFile, accountFile)
		},
	}
	c.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "loopback IP:PORT of the fake Proton API (port 0: any free one)")
	c.Flags().StringVar(&control, "control", "127.0.0.1:0", "loopback IP:PORT of the control endpoint (port 0: any free one)")
	c.Flags().StringVar(&seedFile, "seed", "", "data set file to seed the account with (testdata/seed/dataset.yaml); empty: no events")
	c.Flags().StringVar(&accountFile, "account", "", "the account's TOML file, mode 0600: username, password, totp_secret (required)")
	return cli.NoJSON(c)
}

func serve(cmd *cobra.Command, listen, control, seedFile, accountFile string) (err error) {
	if accountFile == "" {
		return &exitcode.Error{Code: exitcode.Usage, Err: errors.New("--account is required"), Hint: "pass a 0600 TOML file with username, password and totp_secret"}
	}
	acct, err := readAccount(accountFile)
	if err != nil {
		return err
	}
	var ds seed.Dataset
	if seedFile != "" {
		if ds, err = seed.Load(seedFile); err != nil {
			return &exitcode.Error{Code: exitcode.MustAct, Err: err, Hint: "fix the data set file"}
		}
	}
	api, err := listenLoopback("listen", listen)
	if err != nil {
		return cli.UsageError(err)
	}
	ctl, err := listenLoopback("control", control)
	if err != nil {
		_ = api.Close()
		return cli.UsageError(err)
	}
	var p protontest.Process
	defer p.Close()
	p.Cleanup(func() { _ = ctl.Close() })
	defer protontest.Recover(&err)
	f := protontest.New(&p, protontest.Options{
		Username: acct.Username, Password: acct.Password,
		TwoFA: acct.TOTPSecret != "", TOTPSecret: acct.TOTPSecret, Listener: api,
	})
	var n seeded
	if seedFile != "" {
		if n, err = seedDataset(f, f.Username, ds); err != nil {
			return &exitcode.Error{Code: exitcode.Blocked, Err: err}
		}
	}
	srv := &http.Server{Handler: controlHandler(f), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctl) }()
	p.Cleanup(func() { _ = srv.Close() })

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Base URL: %s\n", f.URL)
	fmt.Fprintf(out, "Username: %s\n", f.Username)
	fmt.Fprintf(out, "Control URL: http://%s\n", ctl.Addr())
	fmt.Fprintf(out, "Seeded: %d calendars, %d events\n", n.calendars, n.events)
	for _, c := range n.perCalendar {
		mark := ""
		if c.isDefault {
			mark = " (default)"
		}
		fmt.Fprintf(out, "Calendar %q%s: %d objects, %d rows\n", c.name, mark, c.objects, c.rows)
	}
	fmt.Fprintln(out, readyLine)
	select {
	case <-cmd.Context().Done():
		return nil
	case err := <-served:
		return &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("control endpoint: %w", err)}
	}
}

// fake is what the control endpoint drives (a *protontest.Fake).
type fake interface {
	RevokeSessions()
	ExpireAccessTokens()
	Calls(method, pathPrefix string) int
}

// controlHandler is the control endpoint: POST /revoke-sessions, POST
// /expire-access-tokens, and GET /calls?method=M&path=P (the count of
// requests the fake received, as Fake.Calls counts them).
func controlHandler(f fake) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /revoke-sessions", func(w http.ResponseWriter, _ *http.Request) {
		f.RevokeSessions()
		fmt.Fprintln(w, "every session revoked")
	})
	mux.HandleFunc("POST /expire-access-tokens", func(w http.ResponseWriter, _ *http.Request) {
		f.ExpireAccessTokens()
		fmt.Fprintln(w, "every access token expired")
	})
	mux.HandleFunc("GET /calls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fmt.Fprintln(w, strconv.Itoa(f.Calls(q.Get("method"), q.Get("path"))))
	})
	return mux
}
