package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// tokenData is the data of pcb mcp token add (and of one listed token).
type tokenData struct {
	Name      string     `json:"name"`
	Caps      []string   `json:"caps"`
	Accounts  []string   `json:"accounts"`
	Calendars []string   `json:"calendars"`
	Created   time.Time  `json:"created"`
	LastUsed  *time.Time `json:"last_used"`
	Expires   *time.Time `json:"expires"`
	Expired   bool       `json:"expired"`
	Token     string     `json:"token"`
	Plan      bool       `json:"plan"`
}

func tokenFile(r *rig) string { return filepath.Join(r.dirs.State, "mcp-tokens.json") }

// tokenAdd runs pcb mcp token add with args and --json and returns its data.
func tokenAdd(t *testing.T, r *rig, args ...string) tokenData {
	t.Helper()
	r.want(exitcode.OK, append(append([]string{"mcp", "token", "add"}, args...), "--json")...)
	var d tokenData
	mustUnmarshal(t, r.envelope().Data, &d)
	return d
}

func tokenList(t *testing.T, r *rig) []tokenData {
	t.Helper()
	r.want(exitcode.OK, "mcp", "token", "list", "--json")
	var d struct {
		Tokens []tokenData `json:"tokens"`
	}
	mustUnmarshal(t, r.envelope().Data, &d)
	if d.Tokens == nil {
		t.Fatalf("mcp token list --json: no tokens array: %s", r.out.String())
	}
	return d.Tokens
}

// verifies reports whether secret is a token of the rig's state.
func verifies(t *testing.T, r *rig, secret string) bool {
	t.Helper()
	_, ok, err := access.NewTokenStore(r.dirs.State, nil).Verify(secret)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func noTokenFile(t *testing.T, r *rig) {
	t.Helper()
	if _, err := os.Stat(tokenFile(r)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the token file exists (%v), want none", err)
	}
}

func TestMCPTokenAddPrintsOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	d := tokenAdd(t, r, "claude", "--allow", "read,write", "--calendar", "cal-1")
	if e := r.envelope(); !e.OK || e.Command != "mcp token add" {
		t.Errorf("envelope = %+v", e)
	}
	if !strings.HasPrefix(d.Token, access.TokenPrefix) || len(d.Token) != 48 || d.Name != "claude" ||
		!slices.Equal(d.Caps, []string{"read", "write"}) || !slices.Equal(d.Calendars, []string{"cal-1"}) ||
		len(d.Accounts) != 0 || d.Expires != nil || d.Created.IsZero() || d.Plan {
		t.Fatalf("data = %+v", d)
	}
	if n := strings.Count(r.out.String(), d.Token); n != 1 {
		t.Errorf("the token appears %d times on stdout, want once", n)
	}
	if r.errb.Len() != 0 {
		t.Errorf("stderr = %q, want empty in JSON mode", r.errb.String())
	}
	if !verifies(t, r, d.Token) {
		t.Fatal("the printed token does not verify")
	}
	// Text mode prints it once too, with how to use it.
	r.want(exitcode.OK, "mcp", "token", "add", "cursor", "--allow", "freebusy")
	out := r.out.String()
	i := strings.Index(out, "Token: ")
	if i < 0 {
		t.Fatalf("no token line:\n%s", out)
	}
	text := strings.Fields(out[i+len("Token: "):])[0]
	if !strings.HasPrefix(text, access.TokenPrefix) || strings.Count(out, text) != 1 || !verifies(t, r, text) {
		t.Errorf("token %q appears %d times or does not verify:\n%s", text, strings.Count(out, text), out)
	}
	for _, want := range []string{"Authorization: Bearer", "shown only this once", "pcb mcp token remove cursor"} {
		if !strings.Contains(out, want) {
			t.Errorf("the text lacks %q:\n%s", want, out)
		}
	}
	// A list never shows a secret, in either form.
	list := tokenList(t, r)
	if len(list) != 2 || list[0].Name != "claude" || list[1].Name != "cursor" || list[0].Token != "" ||
		!slices.Equal(list[0].Caps, []string{"read", "write"}) {
		t.Errorf("list = %+v", list)
	}
	listJSON := r.out.String()
	r.want(exitcode.OK, "mcp", "token", "list")
	for _, out := range []string{listJSON, r.out.String()} {
		if strings.Contains(out, d.Token) || strings.Contains(out, text) || strings.Contains(out, access.TokenPrefix) {
			t.Errorf("a list shows a token:\n%s", out)
		}
	}
	for _, want := range []string{"claude", "cursor", "read,write", "freebusy", "never"} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("the list lacks %q:\n%s", want, r.out.String())
		}
	}
	// A taken name is a bad invocation.
	r.want(exitcode.Usage, "mcp", "token", "add", "CLAUDE", "--allow", "read", "--json")
	if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, "already exists") {
		t.Errorf("a taken name: %+v", e)
	}
}

func TestMCPTokenListEmpty(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	if list := tokenList(t, r); len(list) != 0 {
		t.Errorf("list = %+v", list)
	}
	r.want(exitcode.OK, "mcp", "token", "list")
	if !strings.Contains(r.out.String(), "No MCP token yet") {
		t.Errorf("text = %q", r.out.String())
	}
	noTokenFile(t, r)
}

func TestMCPTokenRefusesLoginCap(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	const msg = "login and manage are stdio only: grant them with pcb mcp --profile"
	for _, allow := range []string{"login", "read,manage", "manage,login"} {
		r.want(exitcode.Usage, "mcp", "token", "add", "claude", "--allow", allow, "--json")
		if e := r.envelope(); e.OK || e.Error == nil || e.Error.Message != msg {
			t.Errorf("--allow %s: envelope = %+v, want %q", allow, e, msg)
		}
	}
	r.want(exitcode.Usage, "mcp", "token", "add", "claude", "--allow", "read,login")
	if !strings.Contains(r.errb.String(), "Error: "+msg+"\n") {
		t.Errorf("text stderr = %q", r.errb.String())
	}
	// An unknown capability names the valid ones.
	r.want(exitcode.Usage, "mcp", "token", "add", "claude", "--allow", "read,admin", "--json")
	if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, `"admin"`) ||
		!strings.Contains(e.Error.Message, "freebusy, read, write, delete") {
		t.Errorf("unknown capability: %+v", e)
	}
	// --allow is required and takes at least one.
	for _, args := range [][]string{{}, {"--allow", ""}, {"--allow", ","}} {
		r.want(exitcode.Usage, append([]string{"mcp", "token", "add", "claude", "--json"}, args...)...)
		if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, "--allow") {
			t.Errorf("%v: %+v", args, e)
		}
	}
	noTokenFile(t, r)
}

func TestMCPTokenUnknownAccount(t *testing.T) {
	t.Parallel()
	hubOf := func() *stubHub {
		h := newStubHub(&stubAPI{statusPhase: "ready"}, &stubAPI{statusPhase: "ready"})
		h.accounts[0].address, h.accounts[1].address = userA, userB
		return h
	}
	check := func(t *testing.T, r *rig) {
		t.Helper()
		r.want(exitcode.Usage, "mcp", "token", "add", "claude", "--allow", "read", "--account", userA, "--account", "c@x", "--json")
		if e := r.envelope(); e.Error == nil || e.Error.Message != "no account c@x" {
			t.Errorf("an unknown account: %+v", e)
		}
		noTokenFile(t, r)
		// Known ones, by address or local ID, are stored as given.
		d := tokenAdd(t, r, "claude", "--allow", "read", "--account", userA, "--account", "00000001")
		if !slices.Equal(d.Accounts, []string{userA, "00000001"}) {
			t.Errorf("accounts = %v", d.Accounts)
		}
		if list := tokenList(t, r); len(list) != 1 || !slices.Equal(list[0].Accounts, d.Accounts) {
			t.Errorf("list = %+v", list)
		}
	}
	t.Run("in process", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, nil)
		r.hub = hubOf()
		check(t, r)
	})
	t.Run("daemon", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, nil)
		startDaemon(t, r, hubOf(), nil)
		noInProcess(t, r)
		check(t, r)
	})
	t.Run("no account named, none opened", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, nil)
		noInProcess(t, r)
		tokenAdd(t, r, "claude", "--allow", "read")
	})
}

func TestMCPTokenPlanChangesNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.want(exitcode.OK, "mcp", "token", "add", "claude", "--allow", "read,delete", "--expires", "30d", "--plan", "--json")
	var d tokenData
	mustUnmarshal(t, r.envelope().Data, &d)
	if !d.Plan || d.Name != "claude" || !slices.Equal(d.Caps, []string{"read", "delete"}) || d.Token != "" || d.Expires == nil {
		t.Errorf("plan data = %+v", d)
	}
	if strings.Contains(r.out.String(), access.TokenPrefix) {
		t.Errorf("a plan printed a token: %s", r.out.String())
	}
	r.want(exitcode.OK, "mcp", "token", "add", "claude", "--allow", "read", "--plan")
	if out := r.out.String(); !strings.Contains(out, "nothing was changed") || strings.Contains(out, access.TokenPrefix) {
		t.Errorf("plan text:\n%s", out)
	}
	noTokenFile(t, r)
	// A plan reports the refusal the add would meet, changing nothing.
	tokenAdd(t, r, "claude", "--allow", "read")
	before, err := os.ReadFile(tokenFile(r))
	if err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.Usage, "mcp", "token", "add", "Claude", "--allow", "read", "--plan", "--json")
	r.want(exitcode.Usage, "mcp", "token", "add", "other", "--allow", "login", "--plan", "--json")
	if after, _ := os.ReadFile(tokenFile(r)); string(after) != string(before) {
		t.Errorf("a plan changed the file")
	}
	if list := tokenList(t, r); len(list) != 1 {
		t.Errorf("list after the plans = %+v", list)
	}
}

func TestMCPTokenExpires(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	for flag, d := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "36h": 36 * time.Hour, "1h30m": 90 * time.Minute} {
		before := time.Now().Truncate(time.Second)
		got := tokenAdd(t, r, "t"+flag, "--allow", "read", "--expires", flag)
		after := time.Now()
		if got.Expires == nil || got.Expires.Before(before.Add(d)) || got.Expires.After(after.Add(d)) {
			t.Errorf("--expires %s: expires %v, want about now + %v", flag, got.Expires, d)
		}
	}
	for _, bad := range []string{"soon", "0s", "-1h", "0d", "1.5d", "d"} {
		r.want(exitcode.Usage, "mcp", "token", "add", "x", "--allow", "read", "--expires", bad, "--json")
		if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, "--expires") {
			t.Errorf("--expires %s: %+v", bad, e)
		}
	}
	if list := tokenList(t, r); len(list) != 3 {
		t.Errorf("list = %+v", list)
	}
}

func TestMCPTokenRemoveNeedsYes(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	secret := tokenAdd(t, r, "claude", "--allow", "read").Token
	// No terminal: --yes is required.
	r.want(exitcode.MustAct, "mcp", "token", "remove", "claude", "--json")
	if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, "--yes") ||
		e.Error.Hint != "pcb mcp token remove claude --yes" {
		t.Errorf("without --yes: %+v", e)
	}
	if !verifies(t, r, secret) {
		t.Fatal("a refused remove removed the token")
	}
	// At a terminal, anything but "remove" changes nothing.
	r.term.interactive, r.term.lines = true, []string{"yes"}
	r.want(exitcode.MustAct, "mcp", "token", "remove", "claude")
	if !verifies(t, r, secret) {
		t.Fatal("an unconfirmed remove removed the token")
	}
	if len(r.term.linePrompts) != 1 || !strings.Contains(r.term.linePrompts[0], `"claude"`) {
		t.Errorf("prompts = %q", r.term.linePrompts)
	}
	r.term.lines = []string{"remove"}
	r.want(exitcode.OK, "mcp", "token", "remove", "claude")
	if verifies(t, r, secret) {
		t.Fatal("a confirmed remove kept the token")
	}
	// --yes removes without asking; an unknown name is a bad invocation.
	r.term.interactive = false
	secret = tokenAdd(t, r, "claude", "--allow", "read").Token
	r.want(exitcode.OK, "mcp", "token", "remove", "CLAUDE", "--yes", "--json")
	var d struct {
		Removed tokenData `json:"removed"`
	}
	mustUnmarshal(t, r.envelope().Data, &d)
	if d.Removed.Name != "claude" || verifies(t, r, secret) {
		t.Errorf("removed = %+v, still verifies: %v", d.Removed, verifies(t, r, secret))
	}
	r.want(exitcode.Usage, "mcp", "token", "remove", "claude", "--yes", "--json")
	if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, `no MCP token "claude"`) {
		t.Errorf("an unknown name: %+v", e)
	}
	r.want(exitcode.Usage, "mcp", "token", "remove", "claude", "--json")
}
