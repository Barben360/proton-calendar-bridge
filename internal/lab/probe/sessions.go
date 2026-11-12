package probe

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// The sessions endpoints and their auth are as go-proton-api (MIT) uses
// them (auth.go: AuthSessions, AuthRevoke): GET /auth/v4/sessions and
// DELETE /auth/v4/sessions/{UID}, with the session's own access token and no
// extra scope. This is our own code on pcb's transport.
const sessionsPath = "/auth/v4/sessions"

// callTimeout bounds one HTTP round trip of a probe.
const callTimeout = 30 * time.Second

// SessionInfo is one session of the account, as structural facts only: the
// UID is redacted (session.RedactID), no token is ever read.
type SessionInfo struct {
	ID         string  `json:"id"`
	Created    string  `json:"created"` // UTC, RFC 3339
	ClientID   string  `json:"client_id"`
	ClientName string  `json:"client_name"`
	Revocable  bool    `json:"revocable"`
	Self       bool    `json:"self"`
	Revoke     *Answer `json:"revoke,omitempty"` // the DELETE's answer, when one was sent
	Left       string  `json:"left,omitempty"`   // why it was not revoked
	uid        string
}

// Line is the session's output line.
func (s SessionInfo) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s created %s client %q (%q) revocable %v", s.ID, s.Created, s.ClientID, s.ClientName, s.Revocable)
	if s.Self {
		b.WriteString(", this session")
	}
	switch {
	case s.Revoke != nil:
		fmt.Fprintf(&b, "; revoke: %s", s.Revoke)
	case s.Left != "":
		fmt.Fprintf(&b, "; left: %s", s.Left)
	}
	return b.String()
}

// SessionsResult is one `probe sessions` run.
type SessionsResult struct {
	Sessions []SessionInfo `json:"sessions"`
	Revoked  bool          `json:"revoke_others"`
	After    *int          `json:"after,omitempty"` // sessions listed after the revokes
	Failed   int           `json:"failed"`          // revokes Proton did not confirm
}

// Sessions lists the account's sessions and, with revokeOthers, revokes
// every revocable one but s's own (ruling L4), then lists them again. A
// refused revoke is reported in its session's Revoke, not as an error.
func Sessions(ctx context.Context, s *session.Session, revokeOthers bool) (SessionsResult, error) {
	hc, rec := newRecorder()
	c, err := s.API(hc, nil)
	if err != nil {
		return SessionsResult{}, err
	}
	res := SessionsResult{Revoked: revokeOthers}
	if res.Sessions, err = listSessions(ctx, c, s.UID); err != nil {
		return res, err
	}
	if !revokeOthers {
		return res, nil
	}
	for i := range res.Sessions {
		si := &res.Sessions[i]
		switch {
		case si.Self:
			si.Left = "this session"
			continue
		case si.uid == "":
			si.Left = "no UID"
			continue
		case !si.Revocable:
			si.Left = "not revocable"
			continue
		}
		var out struct{ Code int }
		var a Answer
		if err := c.Delete(ctx, sessionsPath+"/"+url.PathEscape(si.uid), &out); err != nil {
			a = revokeAnswer(err, *si)
		} else {
			a = Answer{Status: rec.status(), Code: out.Code}
		}
		si.Revoke = &a
		if !a.OK() {
			res.Failed++
		}
	}
	after, err := listSessions(ctx, c, s.UID)
	if err != nil {
		return res, err
	}
	n := len(after)
	res.After = &n
	return res, nil
}

// revokeAnswer is answerOf for a failed revoke of si: the error names the
// path, so the session's whole UID is replaced by its redacted ID.
func revokeAnswer(err error, si SessionInfo) Answer {
	a := answerOf(err)
	a.Note = strings.ReplaceAll(a.Note, url.PathEscape(si.uid), si.ID)
	return a
}

func listSessions(ctx context.Context, c *api.Client, self string) ([]SessionInfo, error) {
	var out struct{ Sessions []gpa.AuthSession }
	if err := c.Get(ctx, sessionsPath, nil, &out); err != nil {
		return nil, fmt.Errorf("listing the sessions: %w", err)
	}
	list := make([]SessionInfo, 0, len(out.Sessions))
	for _, a := range out.Sessions {
		list = append(list, SessionInfo{
			ID:         session.RedactID(a.UID),
			Created:    time.Unix(a.CreateTime, 0).UTC().Format(time.RFC3339),
			ClientID:   a.ClientID,
			ClientName: a.LocalizedClientName,
			Revocable:  bool(a.Revocable),
			Self:       a.UID == self,
			uid:        a.UID,
		})
	}
	return list, nil
}
