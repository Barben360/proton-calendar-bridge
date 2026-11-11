package cli_test

import (
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
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

func TestClientAddTextShowsThePasswordOnceAndHowToUseIt(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	r.want(exitcode.OK, "client", "add", "Phone")
	out := r.out.String()
	i := strings.Index(out, "Password (shown once, not stored): ")
	if i < 0 {
		t.Fatalf("no password line:\n%s", out)
	}
	pw := strings.Fields(out[i+len("Password (shown once, not stored): "):])[0]
	if len(pw) != 32 || strings.Count(out, pw) != 1 {
		t.Errorf("password %q (%d chars) appears %d times, want 32 chars, once", pw, len(pw), strings.Count(out, pw))
	}
	for _, want := range []string{"Thunderbird", "Username", "anything", "pcb serve"} {
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
