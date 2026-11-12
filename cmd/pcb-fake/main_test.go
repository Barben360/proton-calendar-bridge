package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/seed"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

const (
	testSecret   = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	testPassword = "pcb-fake-test-password-1"
)

// accountFile writes an account file with mode and returns its path.
func accountFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "account.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func goodAccount(t *testing.T) string {
	return accountFile(t, "username = \"rehearsal\"\npassword = \""+testPassword+"\"\ntotp_secret = \""+testSecret+"\"\n", 0o600)
}

func TestListenAcceptsOnlyLoopbackIPs(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "localhost:0", "192.168.1.10:0", "example.org:80", "127.0.0.1", "[fe80::1%lo]:0"} {
		if l, err := listenLoopback("listen", addr); err == nil {
			_ = l.Close()
			t.Errorf("%q: listening, want a refusal", addr)
		}
	}
	l, err := listenLoopback("listen", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("127.0.0.1:0: %v", err)
	}
	_ = l.Close()
}

// run executes pcb-fake with args until it returns, at most 10 s: a serve
// that should have been refused then ends with exit 0 and its intro.
func run(t *testing.T, args ...string) (exitcode.Code, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	code := execute(ctx, args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestServeRefusesWhatItMustNotRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		code exitcode.Code
		want string // in stderr
	}{
		{"any interface", []string{"--listen", "0.0.0.0:0"}, exitcode.Usage, "loopback IP only"},
		{"a host name", []string{"--listen", "localhost:0"}, exitcode.Usage, "loopback IP only"},
		{"control on any interface", []string{"--control", "0.0.0.0:0"}, exitcode.Usage, "--control"},
		{"no account", []string{"--account", ""}, exitcode.Usage, "--account is required"},
		{"json", []string{"--json"}, exitcode.Usage, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"serve", "--account", goodAccount(t)}, c.args...)
			code, out, errb := run(t, args...)
			if code != c.code || !strings.Contains(errb+out, c.want) {
				t.Fatalf("exit %d, want %d with %q\nstdout: %s\nstderr: %s", code, c.code, c.want, out, errb)
			}
			if strings.Contains(out, "Base URL") {
				t.Fatalf("it served: %s", out)
			}
		})
	}
}

func TestAccountFileIsRefusedUnlessPrivateAndComplete(t *testing.T) {
	t.Parallel()
	const secretish = "s3cret-value-never-echoed"
	cases := []struct {
		name, content string
		mode          os.FileMode
		want          string
	}{
		{"group readable", "password = \"" + secretish + "\"\n", 0o640, "chmod 600"},
		{"world readable", "password = \"" + secretish + "\"\n", 0o644, "chmod 600"},
		{"no password", "username = \"u\"\n", 0o600, "no password"},
		{"unknown key", "pasword = \"" + secretish + "\"\n", 0o600, "unknown key"},
		{"not TOML", "password " + secretish + "\n", 0o600, "not valid TOML"},
		{"bad TOTP secret", "password = \"p\"\ntotp_secret = \"" + secretish + "!\"\n", 0o600, "not a base32 secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			code, out, errb := run(t, "serve", "--account", accountFile(t, c.content, c.mode))
			if code != exitcode.MustAct || !strings.Contains(errb, c.want) {
				t.Fatalf("exit %d, want %d with %q\nstdout: %s\nstderr: %s", code, exitcode.MustAct, c.want, out, errb)
			}
			if strings.Contains(out+errb, secretish) {
				t.Fatalf("a value of the account file was echoed: %s%s", out, errb)
			}
		})
	}
	a, err := readAccount(accountFile(t, "password = \"p\"\n", 0o600))
	if err != nil || a.Username != defaultUsername || a.Password != "p" || a.TOTPSecret != "" {
		t.Fatalf("minimal file: %+v, %v", a, err)
	}
}

// lineWriter hands serve's output to the test through a pipe.
type lineWriter struct{ w *io.PipeWriter }

func (l lineWriter) Write(p []byte) (int, error) { return l.w.Write(p) }

// TestServeSeedsAndItsControlRevokes runs serve as the rehearsal does: it
// prints its URLs and counts, its account logs in with the TOTP of the
// file, a revoke through the control endpoint ends the session, and the
// end of the context stops it with exit 0.
func TestServeSeedsAndItsControlRevokes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	var errb bytes.Buffer
	done := make(chan exitcode.Code, 1)
	go func() {
		done <- execute(ctx, []string{"serve", "--account", goodAccount(t), "--seed", "../../testdata/seed/dataset.yaml"}, lineWriter{pw}, &errb)
		_ = pw.Close()
	}()
	lines := map[string]string{}
	var calendars []string
	sc := bufio.NewScanner(pr)
	for sc.Scan() && sc.Text() != readyLine {
		k, v, _ := strings.Cut(sc.Text(), ": ")
		if strings.HasPrefix(k, "Calendar ") {
			calendars = append(calendars, sc.Text())
		}
		lines[k] = v
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	base, ctl := lines["Base URL"], lines["Control URL"]
	if !strings.HasPrefix(base, "http://127.0.0.1:") || !strings.HasPrefix(ctl, "http://127.0.0.1:") || lines["Username"] != "rehearsal" {
		t.Fatalf("intro %v (stderr %s)", lines, errb.String())
	}
	if lines["Seeded"] != "4 calendars, 364 events" {
		t.Fatalf("Seeded: %q, want 4 calendars, 364 events", lines["Seeded"])
	}
	if !slices.Contains(calendars, `Calendar "My calendar" (default): 190 objects, 194 rows`) {
		t.Fatalf("calendar lines %q lack the default calendar's counts", calendars)
	}

	m := gpa.New(gpa.WithHostURL(base), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	defer m.Close()
	c, auth, err := m.NewClientWithLogin(ctx, "rehearsal", []byte(testPassword))
	if err != nil {
		t.Fatalf("login with the file's password: %v", err)
	}
	defer c.Close()
	o, err := otp.Parse("otpauth://totp/x?secret=" + testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Auth2FA(ctx, gpa.Auth2FAReq{TwoFactorCode: o.Code(time.Now())}); err != nil {
		t.Fatalf("2FA with the file's secret: %v", err)
	}
	if status := userStatus(t, base, auth); status != http.StatusOK {
		t.Fatalf("GET /core/v4/users before the revoke: %d", status)
	}
	resp, err := http.Post(ctl+"/revoke-sessions", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /revoke-sessions: %d", resp.StatusCode)
	}
	if status := userStatus(t, base, auth); status != http.StatusUnauthorized {
		t.Fatalf("GET /core/v4/users after the revoke: %d, want 401", status)
	}
	if got := get(t, ctl+"/calls?method=GET&path=/core/v4/users"); got != "2\n" {
		t.Fatalf("calls: %q, want 2", got)
	}

	cancel()
	if code := <-done; code != exitcode.OK {
		t.Fatalf("exit %d after the context ended, want 0 (stderr %s)", code, errb.String())
	}
	if resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(base + "/tests/ping"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the fake still answers after it stopped")
	}
}

func userStatus(t *testing.T, base string, a gpa.Auth) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/core/v4/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", a.UID)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recorder is a seeder that keeps what it is given.
type recorder struct {
	cals     []protontest.Calendar
	def      string
	byCal    map[string][]protontest.Event
	username string
}

func (r *recorder) AddCalendar(username string, c protontest.Calendar) string {
	r.username = username
	r.cals = append(r.cals, c)
	return c.Name
}

func (r *recorder) SetDefaultCalendar(_, id string) { r.def = id }

func (r *recorder) AddEvent(calID string, e protontest.Event) string {
	r.byCal[calID] = append(r.byCal[calID], e)
	return e.UID
}

// rows returns the rows of uid, the master first.
func (r *recorder) rows(t *testing.T, uid string) []protontest.Event {
	t.Helper()
	var out []protontest.Event
	for _, evs := range r.byCal {
		for _, e := range evs {
			if e.UID == uid {
				out = append(out, e)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no row of %s", uid)
	}
	return out
}

// unfolded is the card's lines, continuations joined.
func unfolded(card string) []string {
	return strings.Split(strings.ReplaceAll(card, "\r\n ", ""), "\r\n")
}

func hasLines(t *testing.T, what string, e protontest.Event, want ...string) {
	t.Helper()
	lines := unfolded(e.Card)
	for _, w := range want {
		if !slices.Contains(lines, w) {
			t.Errorf("%s: no line %q in\n%s", what, w, strings.Join(lines, "\n"))
		}
	}
}

func parisUnix(t *testing.T, s string) int64 {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return at.Unix()
}

func TestSeedTurnsTheDataSetIntoProtonRows(t *testing.T) {
	t.Parallel()
	ds, err := seed.Load("../../testdata/seed/dataset.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{byCal: map[string][]protontest.Event{}}
	n, err := seedDataset(r, "rehearsal", ds)
	if err != nil {
		t.Fatal(err)
	}
	if n.calendars != 4 || n.events != 364 || len(r.cals) != 4 || r.def != "My calendar" {
		t.Fatalf("seeded %+v, calendars %d, default %q; want 4 calendars, 364 rows (352 entries, 8 exceptions, a meeting, 3 shared), default My calendar",
			n, len(r.cals), r.def)
	}
	if sh := r.cals[3]; sh.Permissions != 119 || !sh.ForeignMemberFirst || len(r.byCal[sh.Name]) != 3 {
		t.Errorf("shared calendar %+v with %d rows: want Permissions 119 (no Write bit), another member first, 3 rows", sh, len(r.byCal[sh.Name]))
	}

	// A timed series with an edit and a delete (s341, Europe/Paris).
	rows := r.rows(t, "pcb-seed-s341@pcb-fake.invalid")
	if len(rows) != 2 || rows[0].RecurrenceID != nil || rows[1].RecurrenceID == nil {
		t.Fatalf("s341: %d rows, want a master then its exception", len(rows))
	}
	master, exc := rows[0], rows[1]
	hasLines(t, "s341 master", master, "DTSTART;TZID=Europe/Paris:20240304T083000", "DTEND;TZID=Europe/Paris:20240304T090000",
		"RRULE:FREQ=DAILY;COUNT=10", "EXDATE;TZID=Europe/Paris:20240309T083000", "SUMMARY:pcb-seed s341 Daily standup (count 10)")
	if master.Start != parisUnix(t, "2024-03-04 08:30") || master.RRule != "FREQ=DAILY;COUNT=10" {
		t.Errorf("s341 master columns: start %d, rrule %q", master.Start, master.RRule)
	}
	hasLines(t, "s341 exception", exc, "RECURRENCE-ID;TZID=Europe/Paris:20240307T083000",
		"DTSTART;TZID=Europe/Paris:20240307T100000", "DTEND;TZID=Europe/Paris:20240307T103000",
		"SUMMARY:pcb-seed s341.e Daily standup (count 10) (edited)")
	if *exc.RecurrenceID != parisUnix(t, "2024-03-07 08:30") || exc.Start != parisUnix(t, "2024-03-07 10:00") {
		t.Errorf("s341 exception columns: recurrence %d, start %d", *exc.RecurrenceID, exc.Start)
	}

	// UNTIL of a timed series: the end of that day where it runs, in UTC.
	weekly := r.rows(t, "pcb-seed-s342@pcb-fake.invalid")[0]
	hasLines(t, "s342", weekly, "RRULE:FREQ=WEEKLY;UNTIL=20270102T045959Z", "DTSTART;TZID=America/New_York:20260105T180000")
	if !slices.Equal(weekly.Notifications, []protontest.Notification{{Type: 1, Trigger: "-PT15M"}}) {
		t.Errorf("s342 notifications %v, want one -PT15M", weekly.Notifications)
	}
	hasLines(t, "s343", r.rows(t, "pcb-seed-s343@pcb-fake.invalid")[0], "RRULE:FREQ=WEEKLY;INTERVAL=2;COUNT=20")

	// An all-day yearly series: DATE forms, the end exclusive.
	ann := r.rows(t, "pcb-seed-s346@pcb-fake.invalid")
	if len(ann) != 2 || !ann[0].FullDay {
		t.Fatalf("s346: %d rows, full day %v", len(ann), ann[0].FullDay)
	}
	hasLines(t, "s346 master", ann[0], "DTSTART;VALUE=DATE:20210612", "DTEND;VALUE=DATE:20210613", "RRULE:FREQ=YEARLY;COUNT=7",
		"EXDATE;VALUE=DATE:20240612")
	hasLines(t, "s346 exception", ann[1], "RECURRENCE-ID;VALUE=DATE:20230612", "DTSTART;VALUE=DATE:20230613", "DTEND;VALUE=DATE:20230614")

	// Text escaping, reminders, a UTC entry.
	hasLines(t, "s351", r.rows(t, "pcb-seed-s351@pcb-fake.invalid")[0],
		`LOCATION:Salle 4\, Bât. A\; étage 2 \\ aile gauche\nentrée côté jardin\n(2e porte)`,
		`DESCRIPTION:Line one\nLine two\, with a comma\; and a semicolon\\ and a backslash`)
	two := r.rows(t, "pcb-seed-s352@pcb-fake.invalid")[0]
	if want := []protontest.Notification{{Type: 1, Trigger: "-PT15M"}, {Type: 1, Trigger: "-P1D"}, {Type: 0, Trigger: "-PT1H"}}; !slices.Equal(two.Notifications, want) {
		t.Errorf("s352 notifications %v, want %v", two.Notifications, want)
	}
	if two.Color != "#415DF0" {
		t.Errorf("s352 colour %q, want cobalt's", two.Color)
	}
	hasLines(t, "s350", r.rows(t, "pcb-seed-s350@pcb-fake.invalid")[0], "DTSTART:20230608T100000Z", "DTEND:20230608T110000Z")

	// The meeting the data set lacks: an organiser and attendees with tokens.
	hasLines(t, "meeting", r.rows(t, "pcb-fake-meeting@pcb-fake.invalid")[0],
		"ORGANIZER;CN=Fake Organizer:mailto:rehearsal@proton.local",
		"ATTENDEE;CN=Lea;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;X-PM-TOKEN=pcbfaketoken1:mailto:lea@example.org")
}

func TestFoldKeepsLinesShortAndRunesWhole(t *testing.T) {
	t.Parallel()
	long := "SUMMARY:" + strings.Repeat("Zażółć gęślą jaźń 👩‍👩‍👧‍👦 ", 12)
	folded := fold(long)
	for i, l := range strings.Split(folded, "\r\n") {
		if len(l) > 75 || !utf8.ValidString(l) || (i > 0 && !strings.HasPrefix(l, " ")) {
			t.Fatalf("line %d %q: %d octets, valid %v", i, l, len(l), utf8.ValidString(l))
		}
	}
	if got := strings.ReplaceAll(folded, "\r\n ", ""); got != long {
		t.Fatalf("unfolded %q, want the original", got)
	}
	if fold("UID:short") != "UID:short" {
		t.Fatal("a short line was folded")
	}
}

// pcb itself never links the fake: the rehearsal tooling stays out of the
// shipped binary (plan R2, "developer only").
func TestPCBNeverLinksTheFake(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "../pcb").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "github.com/Barben360/proton-calendar-bridge/internal/service") {
		t.Fatalf("go list listed %d packages without internal/service: the check would pass vacuously", len(deps))
	}
	for _, d := range deps {
		for _, banned := range []string{"/internal/proton/protontest", "/internal/lab", "/cmd/pcb-fake", "net/http/httptest", "testing"} {
			if strings.HasSuffix(d, banned) || strings.Contains(d, banned+"/") || d == banned {
				t.Errorf("cmd/pcb links %s", d)
			}
		}
	}
}
