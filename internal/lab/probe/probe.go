// Package probe holds the lab's live measurements. Each probe resumes the
// saved session (never a new login), makes a handful of calls and reports
// status and Proton code, optionally recording the redacted answers.
package probe

import (
	"regexp"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Label turns a candidate such as "web-calendar@5.0.1" into a file-name part.
func Label(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if s == "" || s[0] == '_' || s[0] == '.' || s[0] == '-' {
		s = "x" + s
	}
	return s
}

// save records r as <label>-<step> when a recorder is given.
func save(rec *record.Recorder, label, step, method, path string, r session.Response) error {
	if rec == nil {
		return nil
	}
	_, err := rec.Save(Label(label)+"-"+step, record.Exchange{
		Method: method, Path: path, Status: r.Status, Header: r.Header, Body: r.Body, At: r.At,
	})
	return err
}
