// Package record writes redacted Proton API exchanges to files that are
// committed to the repository (spec/experiments/raw/E##/).
package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/redact"
)

// Exchange is one request/response pair.
type Exchange struct {
	Method string
	Path   string
	Query  url.Values
	Status int
	Header http.Header
	Body   []byte
	At     time.Time
}

// Recorder saves redacted exchanges into Dir. Not safe for concurrent use.
type Recorder struct {
	Dir string

	forbidden []string
}

var (
	nameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	keptHeaders  = []string{"Content-Type", "Retry-After", "X-Pm-Code"}
	errForbidden = errors.New("record: output contains a forbidden secret; nothing written")
)

// Forbid registers a secret that must never appear in a written file, in raw
// or JSON-escaped form. Empty strings are ignored.
func (r *Recorder) Forbid(secret string) {
	if secret != "" {
		r.forbidden = append(r.forbidden, secret)
	}
}

type document struct {
	Method string              `json:"method"`
	Path   string              `json:"path"`
	Query  json.RawMessage     `json:"query"`
	Status int                 `json:"status"`
	Header map[string][]string `json:"header"`
	Body   json.RawMessage     `json:"body"`
	At     string              `json:"at"`
}

// Save redacts e and writes <Dir>/<name>.json (0644, Dir created 0755). The
// final bytes are checked against the Forbid list; on any hit, or a non-empty
// non-JSON body, nothing is written and an error is returned.
func (r *Recorder) Save(name string, e Exchange) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("record: unsafe name %q", name)
	}
	q := e.Query
	if q == nil {
		q = url.Values{}
	}
	qb, err := json.Marshal(map[string][]string(q))
	if err != nil {
		return "", err
	}
	qr, err := redact.JSON(qb)
	if err != nil {
		return "", err
	}
	doc := document{Method: e.Method, Path: e.Path, Query: qr, Status: e.Status,
		Header: map[string][]string{}, At: e.At.UTC().Format(time.RFC3339), Body: json.RawMessage("null")}
	for _, h := range keptHeaders {
		if v := e.Header.Values(h); len(v) > 0 {
			doc.Header[h] = v
		}
	}
	if len(bytes.TrimSpace(e.Body)) > 0 {
		body, err := redact.JSON(e.Body)
		if err != nil {
			return "", fmt.Errorf("record: refusing non-JSON body: %w", err)
		}
		doc.Body = body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	out := buf.Bytes()
	for _, s := range r.forbidden {
		esc, _ := json.Marshal(s)
		if bytes.Contains(out, []byte(s)) || bytes.Contains(out, bytes.Trim(esc, `"`)) {
			return "", errForbidden
		}
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(r.Dir, name+".json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
