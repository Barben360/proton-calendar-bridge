package cli_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// clientRig is a rig on the real service, logged out, with no Proton.
func clientRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, nil)
	r.useLocal()
	return r
}

func addClient(t *testing.T, r *rig, name string) service.ClientSecret {
	t.Helper()
	r.want(exitcode.OK, "client", "add", name, "--json")
	var sec service.ClientSecret
	e := r.envelope()
	mustUnmarshal(t, e.Data, &sec)
	return sec
}

func TestClientAddPrintsThePasswordOnceInTheEnvelope(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	r.want(exitcode.OK, "client", "add", "Thunderbird", "--json")
	e := r.envelope()
	var sec service.ClientSecret
	mustUnmarshal(t, e.Data, &sec)
	if !e.OK || e.Command != "client add" || sec.Name != "Thunderbird" || sec.ID == "" || len(sec.Password) != 32 {
		t.Fatalf("envelope = %+v, data = id %q name %q, %d password chars", e, sec.ID, sec.Name, len(sec.Password))
	}
	if n := strings.Count(r.out.String(), sec.Password); n != 1 {
		t.Errorf("the password appears %d times on stdout, want once", n)
	}
	if r.errb.Len() != 0 {
		t.Errorf("stderr = %q, want empty in JSON mode", r.errb.String())
	}
}

func TestClientAddTextShowsThePasswordAndHowToUseIt(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	r.want(exitcode.OK, "client", "add", "Phone")
	out := r.out.String()
	i := strings.Index(out, "Password: ")
	if i < 0 {
		t.Fatalf("no password line:\n%s", out)
	}
	pw := strings.Fields(out[i+len("Password: "):])[0]
	if len(pw) != 32 || strings.Count(out, pw) != 1 {
		t.Errorf("password %q (%d chars) appears %d times, want 32 chars, once", pw, len(pw), strings.Count(out, pw))
	}
	for _, want := range []string{"Thunderbird", "Username  Phone", "pcb client show Phone"} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions miss %q:\n%s", want, out)
		}
	}
	// The printed password is the real one.
	r.want(exitcode.OK, "client", "list", "--json")
	if strings.Contains(r.out.String(), pw) {
		t.Error("client list prints a password")
	}
}

func TestClientListNeverShowsPasswords(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	a := addClient(t, r, "Phone")
	b := addClient(t, r, "Laptop")
	r.want(exitcode.OK, "client", "list", "--json")
	out := r.out.String()
	e := r.envelope()
	var data struct {
		Clients []service.Client `json:"clients"`
	}
	mustUnmarshal(t, e.Data, &data)
	if e.Command != "client list" || len(data.Clients) != 2 || data.Clients[0].Name != "Phone" || data.Clients[1].Name != "Laptop" {
		t.Fatalf("envelope = %+v", e)
	}
	for _, s := range []string{a.Password, b.Password, "password"} {
		if strings.Contains(out, s) {
			t.Errorf("client list output contains %q", s)
		}
	}
	r.want(exitcode.OK, "client", "list")
	for _, s := range []string{"NAME", "Phone", "Laptop", a.ID, "never"} {
		if !strings.Contains(r.out.String(), s) {
			t.Errorf("text table misses %q:\n%s", s, r.out.String())
		}
	}
}

func TestClientListEmptyIsAnEmptyArray(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	r.want(exitcode.OK, "client", "list", "--json")
	var raw struct {
		Clients []service.Client `json:"clients"`
	}
	mustUnmarshal(t, r.envelope().Data, &raw)
	if raw.Clients == nil || len(raw.Clients) != 0 {
		t.Errorf("stdout = %s, want an empty clients array, not null", r.out.String())
	}
}

func TestClientRemoveByNameAndByID(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	a := addClient(t, r, "Phone")
	addClient(t, r, "Laptop")
	r.want(exitcode.OK, "client", "remove", "phone", "--json")
	if e := r.envelope(); !e.OK || e.Command != "client remove" || !strings.Contains(string(e.Data), a.ID) {
		t.Fatalf("remove by name: %+v", e)
	}
	b := func() service.Client {
		r.want(exitcode.OK, "client", "list", "--json")
		var d struct{ Clients []service.Client }
		mustUnmarshal(t, r.envelope().Data, &d)
		if len(d.Clients) != 1 || d.Clients[0].Name != "Laptop" {
			t.Fatalf("clients after removing Phone = %+v", d.Clients)
		}
		return d.Clients[0]
	}()
	r.want(exitcode.OK, "client", "remove", b.ID)
	r.want(exitcode.OK, "client", "list", "--json")
	var left struct {
		Clients []service.Client `json:"clients"`
	}
	mustUnmarshal(t, r.envelope().Data, &left)
	if len(left.Clients) != 0 {
		t.Errorf("clients left: %s", r.out.String())
	}
}

func TestClientRefusalsAreUsageErrorsWithEnvelopes(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	addClient(t, r, "Phone")
	for _, args := range [][]string{
		{"client", "add", "PHONE"},   // taken, case-insensitively
		{"client", "add", " padded"}, // leading space
		{"client", "add", ""},        // empty
		{"client", "add", "a\tb"},    // control character
		{"client", "add", strings.Repeat("x", 65)},
		{"client", "remove", "nobody"},
	} {
		r.want(exitcode.Usage, append(args, "--json")...)
		e := r.envelope()
		if e.OK || e.Error == nil || e.Error.Code != 2 || e.Error.Hint == "" || e.Command != strings.Join(args[:2], " ") {
			t.Errorf("pcb %v: envelope = %+v", args, e)
		}
	}
	r.want(exitcode.OK, "client", "list", "--json")
	if strings.Count(r.out.String(), `"name":`) != 1 {
		t.Errorf("a refused add left a client: %s", r.out.String())
	}
}

func TestClientCommandsWantExactlyOneOperand(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	for _, args := range [][]string{
		{"client", "add"}, {"client", "add", "a", "b"}, {"client", "remove"}, {"client", "remove", "a", "b"}, {"client", "list", "x"},
	} {
		r.want(exitcode.Usage, append(args, "--json")...)
	}
}

func TestClientErrorsNeverCarryThePassword(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	sec := addClient(t, r, "Phone")
	r.want(exitcode.Usage, "client", "add", "Phone", "--json")
	if strings.Contains(r.out.String()+r.errb.String(), sec.Password) {
		t.Error("an error output contains a password")
	}
}

// showText runs pcb client show args and returns its text lines by label.
func showText(t *testing.T, r *rig, args ...string) map[string]string {
	t.Helper()
	r.want(exitcode.OK, append([]string{"client", "show"}, args...)...)
	lines := map[string]string{}
	for _, l := range strings.Split(r.out.String(), "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			lines[k] = v
		}
	}
	return lines
}

const fpLabel = "TLS certificate SHA-256 fingerprint"

func TestClientShowPrintsWhatThunderbirdNeeds(t *testing.T) {
	t.Parallel()
	t.Run("offline, loopback", func(t *testing.T) {
		t.Parallel()
		r := clientRig(t)
		sec := addClient(t, r, "Phone")
		got := showText(t, r, "Phone")
		if got["URL"] != "http://localhost:1080/" || got["Username"] != "Phone" || got["Password"] != sec.Password {
			t.Errorf("show = %q", r.out.String())
		}
		if _, ok := got[fpLabel]; ok {
			t.Errorf("a fingerprint line over plain HTTP:\n%s", r.out.String())
		}
	})
	t.Run("offline, TLS", func(t *testing.T) {
		t.Parallel()
		r := clientRig(t)
		writeConfig(t, r, "[caldav]\nlisten = \"192.0.2.10:8443\"\n")
		sec := addClient(t, r, "Phone")
		// Before pcb serve made the certificate there is no fingerprint
		// yet, and show creates none.
		got := showText(t, r, "Phone")
		if got["URL"] != "https://192.0.2.10:8443/" || got["Password"] != sec.Password || !strings.Contains(got[fpLabel], "pcb serve") {
			t.Errorf("show before a certificate = %q", r.out.String())
		}
		if _, err := os.Stat(filepath.Join(r.dirs.State, "tls")); err == nil {
			t.Error("client show created the TLS directory")
		}
		res, err := tlsutil.SelfSigned(tlsutil.Options{Dir: filepath.Join(r.dirs.State, "tls"), BindIP: net.ParseIP("192.0.2.10")})
		if err != nil {
			t.Fatal(err)
		}
		if got := showText(t, r, "Phone"); got[fpLabel] != res.Fingerprint {
			t.Errorf("fingerprint = %q, want %q", got[fpLabel], res.Fingerprint)
		}
	})
	t.Run("daemon", func(t *testing.T) {
		t.Parallel()
		r := clientRig(t)
		sec := addClient(t, r, "Phone")
		startDaemon(t, r, &daemonAPI{}, &ctl.Listener{Addr: "0.0.0.0:8443", URL: "https://pcb.lan:8443/", TLS: true, Fingerprint: "AB:CD"})
		got := showText(t, r, "Phone")
		if got["URL"] != "https://pcb.lan:8443/" || got[fpLabel] != "AB:CD" || got["Username"] != "Phone" || got["Password"] != sec.Password {
			t.Errorf("show with a daemon = %q", r.out.String())
		}
	})
}

// writeConfig replaces the rig's config.toml, keeping its Proton URL.
func writeConfig(t *testing.T, r *rig, extra string) {
	t.Helper()
	if err := os.WriteFile(config.Path(r.dirs), []byte("[proton]\nbase_url = \"http://127.0.0.1:1\"\n"+extra), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClientShowJSONHidesThePasswordWithoutReveal(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	sec := addClient(t, r, "Phone")
	r.want(exitcode.OK, "client", "show", "Phone", "--json")
	e := r.envelope()
	var data map[string]any
	mustUnmarshal(t, e.Data, &data)
	if strings.Contains(r.out.String(), sec.Password) || data["password"] != nil {
		t.Fatalf("--json without --reveal carries the password: %s", r.out.String())
	}
	if !e.OK || e.Command != "client show" || data["username"] != "Phone" || data["url"] != "http://localhost:1080/" || data["recoverable"] != true || data["id"] != sec.ID {
		t.Fatalf("envelope = %+v, data %v", e, data)
	}
	r.want(exitcode.OK, "client", "show", "Phone", "--json", "--reveal")
	mustUnmarshal(t, r.envelope().Data, &data)
	if data["password"] != sec.Password {
		t.Errorf("--reveal: password = %v, want the client's", data["password"])
	}
	if r.errb.Len() != 0 {
		t.Errorf("stderr = %q, want empty in JSON mode", r.errb.String())
	}
}

func TestClientShowNamesRotateForAnUnrecoverablePassword(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	addClient(t, r, "Old")
	path := filepath.Join(r.dirs.State, "clients.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	mustUnmarshal(t, raw, &f)
	f["version"] = 1
	for _, c := range f["clients"].([]any) {
		delete(c.(map[string]any), "sealed")
	}
	raw, _ = json.Marshal(f)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := showText(t, r, "Old")
	if want := "not recoverable (made before pcb could show passwords): run pcb client rotate Old"; got["Password"] != want {
		t.Errorf("Password line = %q, want %q", got["Password"], want)
	}
	r.want(exitcode.OK, "client", "show", "Old", "--json", "--reveal")
	var data map[string]any
	mustUnmarshal(t, r.envelope().Data, &data)
	if data["recoverable"] != false || data["password"] != nil {
		t.Errorf("JSON = %v, want recoverable false and no password", data)
	}
	// Rotating makes it showable.
	r.want(exitcode.OK, "client", "rotate", "Old", "--json")
	var sec service.ClientSecret
	mustUnmarshal(t, r.envelope().Data, &sec)
	if got := showText(t, r, "Old"); got["Password"] != sec.Password || len(sec.Password) != 32 {
		t.Errorf("after rotate: Password line %q", got["Password"])
	}
}

func TestClientShowWithoutANamePicksDefaultElseTheOnlyClient(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	phone := addClient(t, r, "Phone")
	if got := showText(t, r); got["Username"] != "Phone" || got["Password"] != phone.Password {
		t.Errorf("the only client: %q", r.out.String())
	}
	addClient(t, r, "Laptop")
	r.want(exitcode.MustAct, "client", "show", "--json")
	if e := r.envelope(); e.Error == nil || e.Error.Hint != "name a client: pcb client show NAME (pcb client list)" {
		t.Errorf("several clients: %+v", e.Error)
	}
	r.want(exitcode.Usage, "client", "show", "a", "b")
}

func TestClientRotatePrintsTheNewPasswordAndTheHint(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	old := addClient(t, r, "Phone")
	r.want(exitcode.OK, "client", "rotate", "phone")
	out := r.out.String()
	got := showText(t, r, "Phone")
	if got["Password"] == old.Password || !strings.Contains(out, "Password: "+got["Password"]) || !strings.Contains(out, "update the password in your CalDAV client") {
		t.Errorf("rotate printed %q; show now gives a new password %v", out, got["Password"] != old.Password)
	}
	r.want(exitcode.Usage, "client", "rotate")
	r.want(exitcode.Usage, "client", "rotate", "nobody")
}

func TestLoginTextNamesClientShowWhenItCreatedDefault(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive")
	if !strings.HasSuffix(strings.TrimSpace(r.out.String()), "CalDAV credentials: pcb client show") {
		t.Errorf("login text = %q, want it to end with the credentials line", r.out.String())
	}
	got := showText(t, r)
	if got["Username"] != "default" || len(got["Password"]) != 32 {
		t.Errorf("show after the first login = %q", r.out.String())
	}
	// A login that created nothing does not say it.
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	sr := stubRig(t, s)
	sr.want(exitcode.OK, "login", "--non-interactive")
	if strings.Contains(sr.out.String(), "CalDAV credentials") {
		t.Errorf("login text without a new client = %q", sr.out.String())
	}
}

// showStub shows one client whatever is asked.
type showStub struct{ stubAPI }

func (*showStub) ShowClient(context.Context, string) (service.ClientCredentials, error) {
	return service.ClientCredentials{Client: service.Client{ID: "c1", Name: "Phone"}, Username: "Phone", Password: "pw", Recoverable: true}, nil
}

// A configuration that no longer loads is named, never an empty URL line.
func TestClientShowNamesAnUnknownURL(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	r.svc = &showStub{}
	writeConfig(t, r, "[caldav]\nlisten = \"no port\"\n")
	got := showText(t, r, "Phone")
	if !strings.HasPrefix(got["URL"], "not known") || !strings.Contains(got["URL"], "listen") {
		t.Errorf("URL line = %q; want not known with the config error\n%s", got["URL"], r.out.String())
	}
	r.want(exitcode.OK, "client", "show", "Phone", "--json")
	var data map[string]any
	mustUnmarshal(t, r.envelope().Data, &data)
	if _, ok := data["url"]; ok {
		t.Errorf("JSON url = %v, want none", data["url"])
	}
}

// ux Minor: empty lists say so, with the next step, instead of a bare
// header: clients, calendars, and cached events (with and without a range).
func TestEmptyListsSaySo(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.useLocal()
	r.want(exitcode.OK, "client", "list")
	if got, want := r.out.String(), "No CalDAV client yet.\nHint: run pcb client add NAME\n"; got != want {
		t.Errorf("pcb client list: stdout %q, want %q", got, want)
	}
	s := stubRig(t, &stubAPI{})
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"calendar", "list"}, "No calendar yet.\nHint: create one in the Proton Calendar web app, then run pcb calendar list again\n"},
		{[]string{"event", "list", "--calendar", "Work"}, "No event cached for this calendar.\nHint: run pcb sync to bring the cache up to date\n"},
		{[]string{"event", "list", "--calendar", "Work", "--from", "2026-11-01"},
			"No cached event in that range.\nHint: widen --from and --to, or run pcb sync to bring the cache up to date\n"},
	} {
		s.want(exitcode.OK, c.args...)
		if got := s.out.String(); got != c.want {
			t.Errorf("pcb %s: stdout %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
	}
}
