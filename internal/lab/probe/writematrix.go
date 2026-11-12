package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
)

// The write matrix writes what pcb never sends (research L.4 #3, #5, #6, #8,
// #10, an unbumped time edit, an update without Notifications) straight to
// the sync endpoint, without the service's refusals (ruling L6), and reports
// what Proton did: the answer to each write and what the rows read back
// show, compared with what was sent, never with what was expected (ruling
// L5). It writes only into a calendar whose name starts with CalendarPrefix,
// every UID starts with UIDPrefix and the run ID, and it deletes what it
// wrote unless told to keep it (ruling L3).
const (
	CalendarPrefix = "pcb-lab"
	UIDPrefix      = "pcb-lab-wm-"
	dtstamp        = "DTSTAMP:20261201T000000Z"
	paris          = "Europe/Paris"
)

// MatrixOptions configure WriteMatrix.
type MatrixOptions struct {
	CalendarID string
	// Username and Password are the test account's: the password derives
	// the key passphrase in memory (and proves itself for the locked scope
	// when Proton asks); it is never stored.
	Username string
	Password []byte
	Keep     bool          // leave the rows written
	Interval time.Duration // pause before every write but the first
	// Sleep waits d or until ctx ends; nil: a real timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// RunID is the run's ID in every UID (8 hex digits); empty: a random one.
	RunID string
	// Limiter paces every request (nil: pcb's transport default, one per
	// 250 ms after a burst of 8).
	Limiter *rate.Limiter
}

func (o MatrixOptions) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if o.Sleep != nil {
		return o.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MatrixResult is one write-matrix run.
type MatrixResult struct {
	RunID     string       `json:"run_id"`
	UIDPrefix string       `json:"uid_prefix"`
	Scope     string       `json:"key_salts"` // how the key salts were read
	Cases     []CaseResult `json:"cases"`
	Cleanup   string       `json:"cleanup"`
	// CleanupFailed: rows of this run may be left (a listing or the delete
	// failed, or the read-back still lists rows).
	CleanupFailed bool   `json:"cleanup_failed,omitempty"`
	Writes        int    `json:"writes"`
	Stopped       string `json:"stopped,omitempty"` // why no further write was sent
	// RateLimited: the stop was a 429 (wait out its Retry-After).
	RateLimited bool `json:"rate_limited,omitempty"`
}

// CaseResult is one case: each write's answer and what the read-back shows.
type CaseResult struct {
	Case     string `json:"case"`
	Ref      string `json:"ref"`
	UID      string `json:"uid"`
	Steps    []Step `json:"steps"`
	ReadBack string `json:"read_back"`
	Line     string `json:"line"`
}

// Step is one write of a case and Proton's answer to its entry.
type Step struct {
	What   string  `json:"what"`
	Answer *Answer `json:"answer,omitempty"` // nil: not sent
	Why    string  `json:"not_sent,omitempty"`
}

func (s Step) String() string {
	if s.Answer == nil {
		return s.What + ": not sent (" + s.Why + ")"
	}
	return s.What + ": " + s.Answer.String()
}

// WriteMatrix runs every case on the calendar o.CalendarID of s's account.
// It refuses, before any key is unlocked or anything written, a calendar
// that is not the account's or whose name does not start with
// CalendarPrefix. A 429 or an ended session stops the writes (Stopped);
// the rows are then left, and no cleanup is tried.
func WriteMatrix(ctx context.Context, s *session.Session, o MatrixOptions) (MatrixResult, error) {
	hc, rec := newRecorder()
	c, err := s.API(hc, o.Limiter)
	if err != nil {
		return MatrixResult{}, err
	}
	cals, err := calendar.List(ctx, c)
	if err != nil {
		return MatrixResult{}, err
	}
	var cal *calendar.Calendar
	for i := range cals {
		if cals[i].ID == o.CalendarID {
			cal = &cals[i]
		}
	}
	switch {
	case cal == nil:
		return MatrixResult{}, &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("calendar %s is not one of the account's", o.CalendarID)}
	case !strings.HasPrefix(cal.Name, CalendarPrefix):
		// The name is not printed: it may be any calendar of the account.
		return MatrixResult{}, &exitcode.Error{Code: exitcode.Usage,
			Err:  fmt.Errorf("calendar %s: its name does not start with %q, refused (the matrix writes only into a %s calendar)", o.CalendarID, CalendarPrefix, CalendarPrefix),
			Hint: "create a calendar named \"pcb-lab\" in the web app of the test account and pass its ID"}
	}

	el := &countingElevator{inner: s.Elevator(c, o.Username, o.Password)}
	user, addrs, err := keys.Fetch(ctx, c)
	if err != nil {
		return MatrixResult{}, err
	}
	salts, err := keys.FetchSalts(ctx, c, el)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			return MatrixResult{}, &exitcode.Error{Code: exitcode.MustAct, Err: err, Hint: "PCB_LAB_PASSWORD must be the test account's password"}
		}
		return MatrixResult{}, err
	}
	pass, err := keys.DeriveKeyPass(user, addrs, salts, o.Password)
	if errors.Is(err, keys.ErrKeysLocked) {
		return MatrixResult{}, &exitcode.Error{Code: exitcode.MustAct, Err: err, Hint: "PCB_LAB_PASSWORD must be the test account's password"}
	}
	if err != nil {
		return MatrixResult{}, err
	}
	unl, err := keys.Unlock(user, addrs, pass)
	clear(pass)
	if err != nil {
		return MatrixResult{}, err
	}
	defer unl.Close()
	acc, err := calendar.Unlock(ctx, c, unl, *cal)
	if err != nil {
		return MatrixResult{}, err
	}
	defer acc.Close()
	if acc.Primary == nil || acc.Signer == nil {
		return MatrixResult{}, errors.New("the calendar's primary key or our address's signing key did not unlock: nothing can be sealed")
	}

	run := o.RunID
	if run == "" {
		if run, err = runID(); err != nil {
			return MatrixResult{}, err
		}
	}
	m := &matrix{
		o: o, c: c, rec: rec, calID: cal.ID, member: acc.MemberID, prefix: UIDPrefix + run + "-",
		signer: acc.Signer, primary: acc.Primary, calKR: acc.KR, addrs: unl.AddrKRs, own: ownKeys(unl),
	}
	res := MatrixResult{RunID: run, UIDPrefix: m.prefix, Scope: "read without the locked scope"}
	if el.unlocks > 0 {
		res.Scope = "read after an SRP unlock of this session (no new session)"
	}
	for _, cs := range matrixCases {
		if m.stopped != "" {
			break
		}
		r := cs.run(ctx, m, m.prefix+cs.name)
		r.Case, r.Ref, r.UID = cs.name, cs.ref, m.prefix+cs.name
		r.Line = fmt.Sprintf("%s (%s): ", r.Case, r.Ref)
		for _, st := range r.Steps {
			r.Line += st.String() + "; "
		}
		r.Line += "read-back: " + r.ReadBack
		res.Cases = append(res.Cases, r)
	}
	res.Stopped, res.RateLimited = m.stopped, m.rateLimited
	switch {
	case m.stopped != "":
		res.Cleanup = "not tried (writes stopped): the rows whose UID starts with " + m.prefix + " are left"
	case o.Keep:
		res.Cleanup = fmt.Sprintf("kept (--keep): %d UID(s) starting with %s", len(m.uids), m.prefix)
	default:
		var ok bool
		res.Cleanup, ok = m.cleanup(ctx)
		res.CleanupFailed = !ok
	}
	res.Writes = m.writes
	return res, nil
}

func runID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// countingElevator counts the scope elevations FetchSalts asked for.
type countingElevator struct {
	inner   keys.Elevator
	unlocks int
}

func (e *countingElevator) Unlock(ctx context.Context) error {
	e.unlocks++
	return e.inner.Unlock(ctx)
}

func (e *countingElevator) Lock(ctx context.Context) error { return e.inner.Lock(ctx) }

func ownKeys(u *keys.Unlocked) func(string) *crypto.KeyRing {
	byEmail := map[string]*crypto.KeyRing{}
	for _, a := range u.Addresses {
		if kr := u.AddrKRs[a.ID]; kr != nil {
			byEmail[strings.ToLower(a.Email)] = kr
		}
	}
	return func(email string) *crypto.KeyRing { return byEmail[strings.ToLower(email)] }
}

// matrix is one run's state.
type matrix struct {
	o                     MatrixOptions
	c                     *api.Client
	rec                   *statusRecorder
	calID, member, prefix string
	signer, primary       *crypto.KeyRing
	calKR                 *crypto.KeyRing
	addrs                 map[string]*crypto.KeyRing
	own                   func(string) *crypto.KeyRing
	writes                int
	uids                  []string
	stopped               string
	rateLimited           bool
}

// entry is one sync entry: a create (no id), an update or a delete (no body).
type entry struct {
	id   string
	body map[string]json.RawMessage
}

func (e entry) wire() map[string]any {
	switch {
	case e.body == nil:
		return map[string]any{"ID": e.id}
	case e.id == "":
		return map[string]any{"Overwrite": 0, "Event": e.body}
	}
	return map[string]any{"ID": e.id, "Event": e.body}
}

// body is b as the sync endpoint takes it, plus extra fields pcb's
// events.Body does not have (PersonalEventContent).
func body(b events.Body, extra map[string]any) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if out[k], err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// noNotifications is the Notifications every write sends unless a case says
// otherwise: [] (none), so no calendar default plays into a read-back.
var noNotifications = events.Value(events.NotificationList{})

// sync sends one sync call, after the interval, and returns each entry's
// answer. A whole-call failure is every entry's answer; a 429 or an ended
// session stops the run.
func (m *matrix) sync(ctx context.Context, entries ...entry) []Answer {
	out := make([]Answer, len(entries))
	if m.stopped != "" {
		for i := range out {
			out[i] = Answer{Note: "not sent: " + m.stopped}
		}
		return out
	}
	if m.writes > 0 {
		if err := m.o.sleep(ctx, m.o.Interval); err != nil {
			m.stopped = "cancelled"
			for i := range out {
				out[i] = Answer{Note: err.Error()}
			}
			return out
		}
	}
	m.writes++
	req := map[string]any{"MemberID": m.member}
	wire := make([]any, len(entries))
	for i, e := range entries {
		wire[i] = e.wire()
		if e.id == "" && e.body != nil {
			req["IsImport"] = 0
		}
	}
	req["Events"] = wire
	var ans struct {
		Responses []struct {
			Index    int
			Response struct {
				Code  int
				Error string
			}
		}
	}
	err := m.c.PutOnce(ctx, "/calendar/v1/"+url.PathEscape(m.calID)+"/events/sync", req, &ans)
	if err != nil {
		a := answerOf(err)
		var ae *api.Error
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
			m.stopped = fmt.Sprintf("Proton answered 429 (Retry-After %s)", ae.RetryAfter)
			m.rateLimited = true
		case errors.Is(err, api.ErrSessionEnded):
			m.stopped = "the session ended"
		case ctx.Err() != nil:
			m.stopped = "cancelled"
		}
		for i := range out {
			out[i] = a
		}
		return out
	}
	status := m.rec.status()
	for i := range out {
		out[i] = Answer{Status: status, Note: "no entry answer"}
	}
	for _, r := range ans.Responses {
		if r.Index >= 0 && r.Index < len(out) {
			out[r.Index] = Answer{Status: status, Code: r.Response.Code, Message: message(r.Response.Error)}
		}
	}
	return out
}

// create seals k as a new event of uid and sends it, with extra body fields.
func (m *matrix) create(ctx context.Context, what, uid string, k card, n events.Optional[events.NotificationList], extra map[string]any) Step {
	m.remember(uid)
	st, _ := m.send(ctx, what, "", uid, k, cards.SealKeys{Signer: m.signer, CalendarPrimary: m.primary}, n, extra)
	return st
}

// update seals k over row r, reusing r's session keys, and sends it.
func (m *matrix) update(ctx context.Context, what, uid string, r readRow, k card, n events.Optional[events.NotificationList]) Step {
	if r.open == nil {
		return Step{What: what, Why: "the row read back does not open"}
	}
	shared, calKey, err := r.open.SessionKeys()
	if err != nil || shared == nil {
		return Step{What: what, Why: "the row's session key is not proven ours"}
	}
	st, _ := m.send(ctx, what, r.id, uid, k, cards.SealKeys{Signer: m.signer, Shared: shared, Calendar: calKey, Update: true}, n, nil)
	return st
}

// send seals k for uid with sk and sends it: a create when id is empty, else
// an update of row id. It returns the step and what was sealed (its key
// packets, when sk mints them).
func (m *matrix) send(ctx context.Context, what, id, uid string, k card, sk cards.SealKeys, n events.Optional[events.NotificationList], extra map[string]any) (Step, events.WireEvent) {
	sealed, err := cards.Seal(k.parts(uid), sk)
	if err != nil {
		return Step{What: what, Why: "sealing failed: " + err.Error()}, sealed
	}
	b, err := body(events.Body{WireEvent: sealed, Notifications: n}, extra)
	if err != nil {
		return Step{What: what, Why: err.Error()}, sealed
	}
	a := m.sync(ctx, entry{id: id, body: b})[0]
	return Step{What: what, Answer: &a}, sealed
}

func (m *matrix) remember(uid string) {
	for _, u := range m.uids {
		if u == uid {
			return
		}
	}
	m.uids = append(m.uids, uid)
}

// cleanup deletes every row of the UIDs written, in one call, and reads
// them back. It never deletes a row whose UID lacks the run's prefix. ok is
// false unless nothing is left.
func (m *matrix) cleanup(ctx context.Context) (line string, ok bool) {
	var del []entry
	for _, uid := range m.uids {
		if !strings.HasPrefix(uid, m.prefix) {
			continue
		}
		rows, err := m.readBack(ctx, uid)
		if err != nil {
			return "listing " + uid + " failed: " + answerOf(err).String() + "; nothing deleted", false
		}
		for _, r := range rows {
			del = append(del, entry{id: r.id})
		}
	}
	if len(del) == 0 {
		return "nothing to delete; left 0", true
	}
	answers := m.sync(ctx, del...)
	refused := 0
	for _, a := range answers {
		if a.Status != http.StatusOK || (a.Code != 0 && a.Code != api.CodeOK) {
			refused++
		}
	}
	line = fmt.Sprintf("delete %d row(s) in one call: %s, %d entr(ies) refused", len(del), answers[0].String(), refused)
	var left []string
	n := 0
	for _, uid := range m.uids {
		rows, err := m.readBack(ctx, uid)
		if err != nil {
			return line + "; read-back failed: " + answerOf(err).String(), false
		}
		if len(rows) > 0 {
			n += len(rows)
			left = append(left, fmt.Sprintf("%s (%d)", uid, len(rows)))
		}
	}
	if n == 0 {
		return line + "; left 0", true
	}
	return fmt.Sprintf("%s; left %d: %s", line, n, strings.Join(left, ", ")), false
}
