package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ErrRateLimited means proton-cal reported a 429 or a rate limit; Apply stopped
// at once and a rerun resumes where it left off.
var ErrRateLimited = errors.New("rate limited by Proton")

// Runner runs proton-cal with args (no binary name) and returns its stdout.
type Runner interface {
	Run(ctx context.Context, args []string) ([]byte, error)
}

// ExecRunner runs the proton-cal binary in an isolated config dir. Its
// environment is an allowlist (PATH, HOME, XDG_*, TZ, LANG, LC_*, SSL_CERT_*,
// proxy variables) plus PROTON_CAL_CONFIG_DIR; apply never logs in, so no
// credential variable is passed at all.
type ExecRunner struct {
	Bin       string   // proton-cal binary
	ConfigDir string   // PROTON_CAL_CONFIG_DIR
	Env       []string // extra KEY=VALUE entries (tests)
}

// childEnvAllowed reports whether proton-cal may inherit the variable. It is an
// allowlist: nothing else (credentials, SSH agent, tokens...) reaches the child.
func childEnvAllowed(k string) bool {
	u := strings.ToUpper(k)
	switch u {
	case "PATH", "HOME", "TZ", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
		return true
	}
	return strings.HasPrefix(k, "XDG_") || strings.HasPrefix(k, "LC_")
}

func (r *ExecRunner) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); childEnvAllowed(k) {
			env = append(env, kv)
		}
	}
	env = append(env, "PROTON_CAL_CONFIG_DIR="+r.ConfigDir)
	return append(env, r.Env...)
}

// RunError is a failed proton-cal run; Stderr is what the tool printed.
type RunError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *RunError) Error() string {
	return fmt.Sprintf("proton-cal %s: %v: %s", strings.Join(e.Args[:min(len(e.Args), 2)], " "), e.Err, strings.TrimSpace(e.Stderr))
}

func (e *RunError) Unwrap() error { return e.Err }

// Run implements Runner.
func (r *ExecRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = r.environ()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, &RunError{Args: args, Stderr: errb.String(), Err: err}
	}
	return out.Bytes(), nil
}

// rateLimitRE matches proton-cal's own 429 error ("proton api error:
// status=429 code=N: ...", printed after its 3 retries) and a plain "too many
// requests" or "rate limit" message; a bare 429 inside an id or a title is not one.
var rateLimitRE = regexp.MustCompile(`(?i)\bstatus=429\b|\btoo many requests\b|\brate[ -]?limit`)

func rateLimited(err error) bool {
	var re *RunError
	return errors.As(err, &re) && rateLimitRE.MatchString(re.Stderr)
}

// ApplyOptions tunes Apply.
type ApplyOptions struct {
	Wait  time.Duration                              // pause before every write but the first (2 s in real runs)
	Sleep func(context.Context, time.Duration) error // default: a timer that honours ctx
	Log   io.Writer                                  // one line per write; default discard
}

// Result counts what Apply did.
type Result struct {
	Written int // writes that succeeded (calendar creations included)
	Planned int // commands in the plan after skipping what exists
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type calendarRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

// Apply seeds the account through proton-cal: it resolves the data set's
// calendars (the default one by the account's default flag, the others by name,
// creating the missing ones), lists what the data set already created, plans the
// rest and runs it in order, one write per Wait. It stops on the first rate limit
// with an error wrapping ErrRateLimited; rerunning resumes.
func Apply(ctx context.Context, ds Dataset, r Runner, o ApplyOptions) (Result, error) {
	var res Result
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	first := true
	write := func(args []string) ([]byte, error) {
		if !first {
			if err := o.Sleep(ctx, o.Wait); err != nil {
				return nil, err
			}
		}
		first = false
		out, err := r.Run(ctx, args)
		if rateLimited(err) {
			return nil, fmt.Errorf("%w: %w", ErrRateLimited, err)
		}
		if err == nil {
			res.Written++
		}
		return out, err
	}

	// read runs a listing; a 429 there stops the run like one on a write.
	read := func(args []string) ([]byte, error) {
		out, err := r.Run(ctx, args)
		if rateLimited(err) {
			return nil, fmt.Errorf("%w: %w", ErrRateLimited, err)
		}
		return out, err
	}
	listCalendars := func() ([]calendarRow, error) {
		out, err := read([]string{"calendars", "-o", "json"})
		if err != nil {
			return nil, err
		}
		var rows []calendarRow
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, fmt.Errorf("calendars listing: %w", err)
		}
		return rows, nil
	}
	rows, err := listCalendars()
	if err != nil {
		return res, err
	}
	calIDs := map[string]string{}
	for _, c := range ds.Calendars {
		id := ""
		for _, row := range rows {
			if (c.Default && row.IsDefault) || (!c.Default && row.Name == c.Name) {
				id = row.ID
				break
			}
		}
		switch {
		case id != "":
		case c.Default:
			return res, errors.New("the account has no calendar flagged default (is_default) in `proton-cal calendars -o json`")
		default:
			args := []string{"create", "calendar", c.Name}
			if c.Color != "" {
				args = append(args, "--color", c.Color)
			}
			out, err := write(append(args, "-o", "json"))
			if err != nil {
				return res, err
			}
			fmt.Fprintf(o.Log, "created calendar %s\n", c.Name)
			var made calendarRow
			if err := json.Unmarshal(out, &made); err != nil {
				return res, fmt.Errorf("created calendar %q but its id is unreadable: %w", c.Name, err)
			}
			if made.ID == "" {
				return res, fmt.Errorf("created calendar %q but proton-cal reported no id", c.Name)
			}
			id = made.ID
		}
		calIDs[c.Key] = id
	}

	from, days := ListingWindow(ds)
	out, err := read([]string{"events", "--all-calendars", "--from", from, "--days", fmt.Sprint(days), "-o", "json"})
	if err != nil {
		return res, err
	}
	existing, err := ParseExisting(bytes.NewReader(out))
	if err != nil {
		return res, err
	}
	eventIDs := map[string]string{}
	for _, e := range existing {
		eventIDs[e.ID] = e.EventID
	}

	cmds := Plan(ds, existing)
	res.Planned = len(cmds)
	for _, c := range cmds {
		args := make([]string, len(c.Args))
		for i, a := range c.Args {
			switch a {
			case PlaceholderCal:
				a = calIDs[c.Calendar]
			case PlaceholderID:
				if eventIDs[c.Ref] == "" {
					return res, fmt.Errorf("%s needs the event id of %s, which proton-cal did not report", c.ID, c.Ref)
				}
				a = eventIDs[c.Ref]
			}
			args[i] = a
		}
		out, err := write(args)
		if err != nil {
			return res, fmt.Errorf("%s: %w", c.ID, err)
		}
		fmt.Fprintf(o.Log, "%s %s\n", c.Op, c.ID)
		if c.Op == OpCreate {
			var made struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(out, &made) == nil && made.ID != "" {
				eventIDs[c.ID] = made.ID
			}
		}
	}
	return res, nil
}

// ListingWindow is the --from and --days of the events listing that finds
// every seeded event. proton-cal filters by the window in its own time zone,
// so the data set's range is widened by a day on each side: an event at
// 2021-01-01 07:00 in Tokyo is still 2020-12-31 in Paris.
func ListingWindow(ds Dataset) (from string, days int) {
	f, _ := time.Parse(dateLayout, ds.Range.From)
	t, _ := time.Parse(dateLayout, ds.Range.To)
	return f.AddDate(0, 0, -1).Format(dateLayout), int(t.Sub(f).Hours()/24) + 3
}
