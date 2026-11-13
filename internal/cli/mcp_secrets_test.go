package cli_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// recorder keeps everything an agent and the person's terminal got from
// pcb: every tool result and MCP log message, as JSON.
type recorder struct {
	mu   sync.Mutex
	outs []string // "<where>: <json>"
	urls []string // the URL elicitations
}

func (rec *recorder) add(where string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(err.Error())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.outs = append(rec.outs, where+": "+string(b))
}

// agent connects an agent to a pcb mcp run that declares form and URL
// elicitation, accepts every question, and records every log message.
func (rec *recorder) agent(t *testing.T, m *mcpRun, name string) *sdkmcp.ClientSession {
	t.Helper()
	c := sdkmcp.NewClient(&sdkmcp.Implementation{Name: name, Version: "0"}, &sdkmcp.ClientOptions{
		Capabilities: &sdkmcp.ClientCapabilities{Elicitation: &sdkmcp.ElicitationCapabilities{
			Form: &sdkmcp.FormElicitationCapabilities{}, URL: &sdkmcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			if req.Params.Mode == "url" {
				rec.mu.Lock()
				rec.urls = append(rec.urls, req.Params.URL)
				rec.mu.Unlock()
			}
			return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		},
		//nolint:staticcheck // deprecated at 2026-07-28, still sent on the versions pcb negotiates: scanned too
		LoggingMessageHandler: func(_ context.Context, req *sdkmcp.LoggingMessageRequest) { rec.add(name+" log", req.Params) },
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cs, err := c.Connect(ctx, m.agent, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, m.errb.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call calls a tool, records its result whatever it is, and returns its
// structured content (nil on a tool error).
func (rec *recorder) call(t *testing.T, cs *sdkmcp.ClientSession, where, tool string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s %s: %v", where, tool, err)
	}
	rec.add(where+" "+tool, res)
	if res.IsError {
		t.Logf("%s %s: %v", where, tool, res.Content[0].(*sdkmcp.TextContent).Text)
		return nil
	}
	out, _ := res.StructuredContent.(map[string]any)
	return out
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// browser fills the login page as a person does: from the page's origin,
// keeping its claim cookie.
type browser struct {
	t  *testing.T
	hc *http.Client
	u  string
}

func (b *browser) post(step string, kv ...string) string {
	b.t.Helper()
	form := url.Values{"step": {step}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	req, err := http.NewRequestWithContext(b.t.Context(), http.MethodPost, b.u, strings.NewReader(form.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}
	pu, _ := url.Parse(b.u)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+pu.Host)
	res, err := b.hc.Do(req)
	if err != nil {
		b.t.Fatalf("POST %s: %v", step, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		b.t.Fatalf("POST %s: %d %s", step, res.StatusCode, body)
	}
	return string(body)
}

// One scripted session over every entry: an agent logs in through the
// loopback page (password, CAPTCHA, 2FA code on the fake), calls every
// tool, a remote agent calls with its token, and a relayed agent logs the
// account out. No tool result, MCP log message, audit or log line (stderr),
// nor any file pcb wrote, holds the password, the 2FA code or secret, the
// CAPTCHA token, the MCP token or the login page's nonce.
func TestNoSecretInAnyMCPOutput(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	captcha := "captcha-" + randomHex(t)
	totpURI := r.f.SetTwoFA(r.f.Username, true)
	r.f.SetCaptchaFor(r.f.Username, captcha)
	r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	writeConfig(t, r, "[secrets]\nbackend = \"keyfile\"\n[mcp]\nremote = true\n")
	r.want(exitcode.OK, "mcp", "token", "add", "agent", "--allow", "read", "--json")
	var added struct {
		Token string `json:"token"`
	}
	mustUnmarshal(t, r.envelope().Data, &added) // the one place it is printed
	if added.Token == "" {
		t.Fatal("token add printed no token")
	}
	rec := &recorder{}

	// 1. pcb mcp serves the session itself: the agent starts the login,
	// the person finishes it on the page.
	m1 := startMCP(t, r, nil, "--profile", "full")
	cs := rec.agent(t, m1, "stdio")
	out := rec.call(t, cs, "stdio", "login", map[string]any{"username": r.f.Username})
	if out == nil || out["where"] != "page" || len(rec.urls) != 1 {
		t.Fatalf("login = %v, URL elicitations %v", out, rec.urls)
	}
	pageURL := rec.urls[0]
	nonce := pageURL[strings.LastIndex(pageURL, "/")+1:]
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, hc: &http.Client{Jar: jar}, u: pageURL}
	if body := b.post("credentials", "password", r.f.Password); !strings.Contains(body, "Open the verification") {
		t.Fatalf("after the password: want the CAPTCHA step:\n%s", body)
	}
	r.advance(61 * time.Second) // the CAPTCHA retry is a second login: past the guard's minute
	if body := b.post("captcha"); !strings.Contains(body, `name="code"`) {
		t.Fatalf("after the CAPTCHA: want the 2FA step:\n%s", body)
	}
	o, err := otp.Parse(totpURI)
	if err != nil {
		t.Fatal(err)
	}
	if body := b.post("totp", "code", o.Code(r.now())); !strings.Contains(body, "added") {
		t.Fatalf("after the code: want the end:\n%s", body)
	}

	// Every other tool once, on the account just added.
	var account string
	for deadline := time.Now().Add(20 * time.Second); account == "" && time.Now().Before(deadline); {
		st := rec.call(t, cs, "stdio", "status", map[string]any{})
		if accts, _ := st["accounts"].([]any); len(accts) == 1 && accts[0].(map[string]any)["last_sync_age_s"] != nil {
			account, _ = accts[0].(map[string]any)["account"].(string)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if account == "" {
		t.Fatalf("the account never showed in status, synced\nstderr: %s", m1.errb.String())
	}
	week := map[string]any{"accounts": []string{account}, "from": "2026-11-16T00:00:00Z", "to": "2026-11-23T00:00:00Z"}
	cals := rec.call(t, cs, "stdio", "calendars_list", map[string]any{"accounts": []string{account}})
	rec.call(t, cs, "stdio", "free_busy", week)
	rec.call(t, cs, "stdio", "events_list", week)
	search := map[string]any{"text": "x"}
	for k, v := range week {
		search[k] = v
	}
	rec.call(t, cs, "stdio", "events_search", search)
	var calendar string
	if cl, _ := cals["calendars"].([]any); len(cl) > 0 {
		calendar, _ = cl[0].(map[string]any)["id"].(string)
	}
	created := rec.call(t, cs, "stdio", "event_create", map[string]any{"account": account, "calendar": calendar, "summary": "Secret check",
		"start": "2026-11-18T10:00:00Z", "end": "2026-11-18T11:00:00Z"})
	id, _ := created["event_id"].(string)
	etag, _ := created["etag"].(string)
	rec.call(t, cs, "stdio", "event_get", map[string]any{"event_id": id})
	updated := rec.call(t, cs, "stdio", "event_update", map[string]any{"event_id": id, "etag": etag, "scope": "series", "changes": map[string]any{"summary": "Secret check 2"}})
	if e, _ := updated["etag"].(string); e != "" {
		etag = e
	}
	rec.call(t, cs, "stdio", "event_delete", map[string]any{"event_id": id, "etag": etag, "scope": "series"})
	_ = cs.Close()
	if code := m1.exit(); code != exitcode.OK {
		t.Fatalf("pcb mcp exit %d\nstderr: %s", code, m1.errb.String())
	}

	// 2. pcb serve: a remote agent with its token, and a relayed agent
	// that logs the account out (confirmed).
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	addr := s.listening()
	remote, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "remote", Version: "0"}, &sdkmcp.ClientOptions{
		//nolint:staticcheck // as in recorder.agent
		LoggingMessageHandler: func(_ context.Context, req *sdkmcp.LoggingMessageRequest) { rec.add("remote log", req.Params) },
	}).Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{secret: added.Token}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.call(t, remote, "remote", "status", map[string]any{}) == nil {
		t.Error("the remote status failed")
	}
	rec.call(t, remote, "remote", "events_list", week)
	_ = remote.Close()
	m2 := startMCP(t, r, nil, "--profile", "full")
	relay := rec.agent(t, m2, "relay")
	if out := rec.call(t, relay, "relay", "logout", map[string]any{"account": account}); out == nil || out["logged_out"] != true {
		t.Errorf("logout = %v", out)
	}
	_ = relay.Close()
	if code := m2.exit(); code != exitcode.OK {
		t.Fatalf("relay exit %d\nstderr: %s", code, m2.errb.String())
	}
	s.stop()
	// Its one account signed out, pcb serve ends with "log in" (10).
	if code := s.exit(); code != exitcode.OK && code != exitcode.MustAct {
		t.Fatalf("pcb serve exit %d\nstderr: %s", code, s.errb.String())
	}

	// The scan.
	codes := r.f.TOTPCodes()
	if len(codes) == 0 {
		t.Fatal("control: the fake received no 2FA code")
	}
	u, err := url.Parse(totpURI)
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"the password":      r.f.Password,
		"the TOTP secret":   u.Query().Get("secret"),
		"the CAPTCHA token": captcha,
		"the MCP token":     added.Token,
		"the page's nonce":  nonce,
	}
	for i, c := range codes {
		secrets["2FA code "+string(rune('1'+i))] = c
	}
	contains := func(text, s string) bool {
		if len(s) <= 8 { // a code: digits on their own, not inside a longer number
			return regexp.MustCompile(`(^|[^0-9])` + regexp.QuoteMeta(s) + `([^0-9]|$)`).MatchString(text)
		}
		return strings.Contains(text, s)
	}
	scan := func(where, text string, skip ...string) {
		t.Helper()
		for what, s := range secrets {
			if s == "" {
				t.Fatalf("control: %s is empty", what)
			}
			if !strings.Contains(strings.Join(skip, ","), what) && contains(text, s) {
				t.Errorf("%s holds %s", where, what)
			}
		}
	}
	rec.mu.Lock()
	outs := rec.outs
	rec.mu.Unlock()
	if len(outs) < 12 {
		t.Fatalf("control: %d outputs recorded", len(outs))
	}
	for _, o := range outs {
		where, _, _ := strings.Cut(o, ":")
		scan(where, o)
	}
	for name, text := range map[string]string{
		"pcb mcp's stderr (session)": m1.errb.String(), "pcb mcp's stderr (relay)": m2.errb.String(), "pcb serve's stderr": s.errb.String(),
		"pcb serve's stdout": s.out.String(),
	} {
		if !strings.Contains(text, "mcp call") && name != "pcb serve's stdout" && name != "pcb mcp's stderr (relay)" {
			t.Errorf("control: %s has no audit line:\n%s", name, text)
		}
		scan(name, text)
	}
	// The protocol stream carries the URL elicitation itself (to the
	// agent's client, which shows it to the person): the nonce only.
	scan("pcb mcp's stdout (session)", m1.stdout.String(), "the page's nonce")
	scan("pcb mcp's stdout (relay)", m2.stdout.String())
	if !strings.Contains(m1.stdout.String(), nonce) {
		t.Error("control: the session's stream does not carry the URL elicitation")
	}
	for _, root := range []string{r.dirs.State, r.dirs.Config} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Type()&fs.ModeSocket != 0 {
				return nil //nolint:nilerr // a file gone meanwhile
			}
			b, err := os.ReadFile(p)
			if err == nil {
				scan("file "+p, string(b))
			}
			return nil
		})
	}
}
