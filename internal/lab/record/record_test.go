package record

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ex(body string) Exchange {
	return Exchange{
		Method: "POST", Path: "/auth/v4", Status: 200,
		Query:  url.Values{"Page": {"0"}, "Token": {"qtoken-s3cr3t"}},
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(body),
		At:     time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
	}
}

func readOnly(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	return n
}

func TestRecorderRefusesUnredacted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "raw")
	r := &Recorder{Dir: dir}
	r.Forbid("hunter2")
	p, err := r.Save("login", ex(`{"Note":"hunter2"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if p != "" || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("path %q, error %q (must not echo the secret)", p, err)
	}
	if n := readOnly(t, dir); len(n) != 0 {
		t.Errorf("files written: %v", n)
	}
}

func TestRecorderForbidChecksEverythingWritten(t *testing.T) {
	cases := map[string]func(e *Exchange){
		"path":    func(e *Exchange) { e.Path = "/x/sekret-in-path" },
		"query":   func(e *Exchange) { e.Query = url.Values{"q": {"sekret-in-path"}} },
		"header":  func(e *Exchange) { e.Header.Set("X-Pm-Code", "sekret-in-path") },
		"body":    func(e *Exchange) { e.Body = []byte(`{"n":"sekret-in-path"}`) },
		"escaped": func(e *Exchange) { e.Body = []byte(`{"n":"a\"b\nc"}`) },
	}
	for name, mut := range cases {
		dir := t.TempDir()
		r := &Recorder{Dir: dir}
		r.Forbid("sekret-in-path")
		r.Forbid("a\"b\nc")
		e := ex(`{}`)
		mut(&e)
		if _, err := r.Save("x", e); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
		if n := readOnly(t, dir); len(n) != 0 {
			t.Errorf("%s: files written: %v", name, n)
		}
	}
}

func TestRecorderDropsAuthHeaders(t *testing.T) {
	dir := t.TempDir()
	r := &Recorder{Dir: dir}
	e := ex(`{"Code":1000}`)
	e.Header = http.Header{
		"Authorization": {"Bearer abc-s3cr3t"}, "X-Pm-Uid": {"uid-s3cr3t"},
		"Set-Cookie": {"Session-Id=cookie-s3cr3t"}, "Cookie": {"c=cookie-s3cr3t"},
		"Content-Type": {"application/json"}, "Retry-After": {"30"}, "X-Pm-Code": {"1000"},
	}
	e.Header.Add("Authorization", "second-s3cr3t")
	p, err := r.Save("hdr", e)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, leak := range []string{"s3cr3t", "Authorization", "x-pm-uid", "X-Pm-Uid", "Cookie"} {
		if strings.Contains(s, leak) {
			t.Errorf("%q kept:\n%s", leak, s)
		}
	}
	for _, keep := range []string{"Content-Type", "Retry-After", "X-Pm-Code", "application/json"} {
		if !strings.Contains(s, keep) {
			t.Errorf("%q dropped:\n%s", keep, s)
		}
	}
}

func TestRecorderRedactsBodyAndQueryAndWritesStableFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	r := &Recorder{Dir: dir}
	body := `{"Code":1000,"AccessToken":"tok-s3cr3t","CalendarID":"c1"}`
	p, err := r.Save("auth", ex(body))
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, "auth.json") {
		t.Errorf("path = %s", p)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != 0o755 {
		t.Errorf("dir mode = %v", dst.Mode().Perm())
	}
	b1, _ := os.ReadFile(p)
	s := string(b1)
	for _, leak := range []string{"s3cr3t"} {
		if strings.Contains(s, leak) {
			t.Errorf("leaked %q:\n%s", leak, s)
		}
	}
	for _, want := range []string{`"CalendarID": "c1"`, `"Code": 1000`, "2026-11-11T10:00:00Z", `"Page"`, "\n  "} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if _, err := r.Save("auth", ex(body)); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(p)
	if string(b1) != string(b2) {
		t.Error("not deterministic")
	}
}

func TestRecorderBodies(t *testing.T) {
	dir := t.TempDir()
	r := &Recorder{Dir: dir}
	if _, err := r.Save("empty", ex("")); err != nil {
		t.Errorf("empty body must be allowed: %v", err)
	}
	if _, err := r.Save("html", ex("<html>token=abc</html>")); err == nil {
		t.Error("non-JSON body must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "html.json")); err == nil {
		t.Error("non-JSON body file written")
	}
}

func TestRecorderRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	r := &Recorder{Dir: dir}
	for _, n := range []string{"", "../evil", "a/b", ".hidden", `a\b`, "x\x00y"} {
		if _, err := r.Save(n, ex(`{}`)); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	if n := readOnly(t, dir); len(n) != 0 {
		t.Errorf("files: %v", n)
	}
}
