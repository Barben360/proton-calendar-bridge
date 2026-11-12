package probe

import (
	"bytes"
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
	"github.com/Barben360/proton-calendar-bridge/internal/ical"
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
	Writes    int          `json:"writes"`
	Stopped   string       `json:"stopped,omitempty"` // why no further write was sent
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

	run, err := runID()
	if err != nil {
		return MatrixResult{}, err
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
	res.Stopped = m.stopped
	switch {
	case m.stopped != "":
		res.Cleanup = "not tried (writes stopped): the rows whose UID starts with " + m.prefix + " are left"
	case o.Keep:
		res.Cleanup = fmt.Sprintf("kept (--keep): %d UID(s) starting with %s", len(m.uids), m.prefix)
	default:
		res.Cleanup = m.cleanup(ctx)
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
}

// card is an event's lines per part, after UID and DTSTAMP.
type card struct {
	shared, enc, cal []string
}

func (k card) parts(uid string) map[ical.PartKind][]byte {
	return map[ical.PartKind][]byte{
		ical.SharedSigned:    fragment(uid, k.shared),
		ical.SharedEncrypted: fragment(uid, k.enc),
		ical.CalendarSigned:  fragment(uid, k.cal),
	}
}

// fragment is a part in Proton's form: CRLF, no VERSION or PRODID, no CRLF
// after the last line (the matrix's lines are all under 75 octets).
func fragment(uid string, lines []string) []byte {
	all := append([]string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "UID:" + uid, dtstamp}, lines...)
	return []byte(strings.Join(append(all, "END:VEVENT", "END:VCALENDAR"), "\r\n"))
}

// simple is a one-hour event in Europe/Paris on day (YYYYMMDD) at 10:00.
func simple(day, seq, summary string, extra ...string) card {
	return card{
		shared: append([]string{"DTSTART;TZID=" + paris + ":" + day + "T100000", "DTEND;TZID=" + paris + ":" + day + "T110000"}, append(extra, "SEQUENCE:"+seq)...),
		enc:    []string{"SUMMARY:pcb-lab wm " + summary},
		cal:    []string{},
	}
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
	sealed, err := cards.Seal(k.parts(uid), cards.SealKeys{Signer: m.signer, CalendarPrimary: m.primary})
	if err != nil {
		return Step{What: what, Why: "sealing failed: " + err.Error()}
	}
	b, err := body(events.Body{WireEvent: sealed, Notifications: n}, extra)
	if err != nil {
		return Step{What: what, Why: err.Error()}
	}
	a := m.sync(ctx, entry{body: b})[0]
	return Step{What: what, Answer: &a}
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
	sealed, err := cards.Seal(k.parts(uid), cards.SealKeys{Signer: m.signer, Shared: shared, Calendar: calKey, Update: true})
	if err != nil {
		return Step{What: what, Why: "sealing failed: " + err.Error()}
	}
	b, err := body(events.Body{WireEvent: sealed, Notifications: n}, nil)
	if err != nil {
		return Step{What: what, Why: err.Error()}
	}
	a := m.sync(ctx, entry{id: r.id, body: b})[0]
	return Step{What: what, Answer: &a}
}

func (m *matrix) remember(uid string) {
	for _, u := range m.uids {
		if u == uid {
			return
		}
	}
	m.uids = append(m.uids, uid)
}

// readRow is a row read back, by its structural facts.
type readRow struct {
	id, keyPacket string
	start, end    int64
	startTZ       string
	rrule         *string
	recurrence    bool
	notifications json.RawMessage
	personal      int
	open          *cards.Event
	parts         map[cards.Kind][]byte
	openErr       error
}

// readBack lists the rows of uid (exactly that UID) and opens each.
func (m *matrix) readBack(ctx context.Context, uid string) ([]readRow, error) {
	rows, err := events.ListByUID(ctx, m.c, m.calID, uid)
	if err != nil {
		return nil, err
	}
	var out []readRow
	for _, r := range rows {
		if r.UID != uid {
			continue
		}
		var w struct {
			SharedKeyPacket string
			StartTimezone   string
			RRule           *string
			RecurrenceID    *int64
			Notifications   json.RawMessage
			PersonalEvents  []json.RawMessage
		}
		_ = json.Unmarshal(r.Raw, &w)
		rr := readRow{
			id: r.ID, keyPacket: w.SharedKeyPacket, start: r.Start, end: r.End, startTZ: w.StartTimezone,
			rrule: w.RRule, recurrence: w.RecurrenceID != nil, notifications: compact(w.Notifications), personal: len(w.PersonalEvents),
		}
		ev, err := cards.Open(r.Raw, m.calKR, m.addrs, m.own)
		if err != nil {
			rr.openErr = err
		} else {
			rr.open = &ev
			rr.parts = map[cards.Kind][]byte{}
			for _, p := range ev.Parts {
				if p.Kind != cards.Personal {
					rr.parts[p.Kind] = p.Text
				}
			}
		}
		out = append(out, rr)
	}
	return out, nil
}

func compact(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("absent")
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

const asSent = "parts as sent"

// partsFact compares r's parts with sent, by kind: the text is never shown.
func partsFact(r readRow, sent map[ical.PartKind][]byte) string {
	if r.openErr != nil {
		return "parts do not open with the row's keys"
	}
	var diff []string
	for k := ical.SharedSigned; k <= ical.Attendees; k++ {
		got, have := r.parts[cards.Kind(k)]
		want, sentIt := sent[k]
		switch {
		case have && !sentIt:
			diff = append(diff, "extra "+k.String())
		case sentIt && !have:
			diff = append(diff, "no "+k.String())
		case sentIt && !bytes.Equal(got, want):
			diff = append(diff, k.String())
		}
	}
	if len(diff) == 0 {
		return asSent
	}
	return "parts differ: " + strings.Join(diff, ", ")
}

// partLine is the value of the first name: line of r's part k, or "absent".
func partLine(r readRow, k ical.PartKind, name string) string {
	for _, l := range strings.Split(string(r.parts[cards.Kind(k)]), "\r\n") {
		if v, ok := strings.CutPrefix(l, name+":"); ok {
			return v
		}
		if strings.HasPrefix(l, name+";") {
			return l[len(name):]
		}
	}
	return "absent"
}

func utc(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

// readFailed is the read-back fact of a failed listing.
func readFailed(err error) string { return "listing failed: " + answerOf(err).String() }

// matrixCase is one case: its name (the UID's end), its reference and how
// it runs.
type matrixCase struct {
	name, ref string
	run       func(ctx context.Context, m *matrix, uid string) CaseResult
}

var matrixCases = []matrixCase{
	{"key-packets-on-update", "L.4 #3", caseKeyPackets},
	{"second-master", "L.4 #5", caseSecondMaster},
	{"rrule-count-50", "L.4 #6", rruleCase("20270302", "FREQ=WEEKLY;COUNT=50")},
	{"rrule-until-2038", "L.4 #6", rruleCase("20270303", "FREQ=WEEKLY;UNTIL=20380106T090000Z")},
	{"rrule-bysetpos-5", "L.4 #6", rruleCase("20270304", "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=5;COUNT=3")},
	{"rrule-hourly", "L.4 #6", rruleCase("20270305", "FREQ=HOURLY;COUNT=3")},
	{"rrule-interval-1000", "L.4 #6", rruleCase("20270306", "FREQ=DAILY;INTERVAL=1000;COUNT=2")},
	{"status-tentative", "L.4 #8", caseTentative},
	{"floating-time", "L.4 #8", timeCase("DTSTART:20270310T100000", "DTEND:20270310T110000")},
	{"non-iana-tzid", "L.4 #8", timeCase("DTSTART;TZID=Mars/Olympus:20270311T100000", "DTEND;TZID=Mars/Olympus:20270311T110000")},
	{"duration", "L.4 #8", caseDuration},
	{"unbumped-time-edit", "R4", caseUnbumpedTime},
	{"notifications-absent", "L.4 #10", caseNotificationsAbsent},
	{"personal-part-left-out", "L.4 #10", casePersonal},
}

// one reads uid back expecting one openable row for a follow-up write; it
// returns the row, or the read-back fact that stops the case.
func (m *matrix) one(ctx context.Context, uid string) (readRow, string) {
	rows, err := m.readBack(ctx, uid)
	switch {
	case err != nil:
		return readRow{}, readFailed(err)
	case len(rows) != 1:
		return readRow{}, fmt.Sprintf("%d row(s)", len(rows))
	case rows[0].openErr != nil:
		return readRow{}, "1 row, parts do not open with the row's keys"
	}
	return rows[0], ""
}

func caseKeyPackets(ctx context.Context, m *matrix, uid string) CaseResult {
	first := simple("20270301", "0", "key packets")
	st := m.create(ctx, "create", uid, first, noNotifications, nil)
	res := CaseResult{Steps: []Step{st}}
	r, stop := m.one(ctx, uid)
	if stop != "" {
		res.Steps = append(res.Steps, Step{What: "update with new key packets", Why: "no single row to update"})
		res.ReadBack = stop
		return res
	}
	next := simple("20270301", "1", "key packets renamed")
	sealed, err := cards.Seal(next.parts(uid), cards.SealKeys{Signer: m.signer, CalendarPrimary: m.primary})
	if err != nil {
		res.Steps = append(res.Steps, Step{What: "update with new key packets", Why: "sealing failed: " + err.Error()})
		res.ReadBack = "1 row"
		return res
	}
	b, err := body(events.Body{WireEvent: sealed, Notifications: noNotifications}, nil)
	if err != nil {
		res.Steps = append(res.Steps, Step{What: "update with new key packets", Why: err.Error()})
		res.ReadBack = "1 row"
		return res
	}
	a := m.sync(ctx, entry{id: r.id, body: b})[0]
	res.Steps = append(res.Steps, Step{What: "update with new key packets", Answer: &a})
	rows, err := m.readBack(ctx, uid)
	if err != nil {
		res.ReadBack = readFailed(err)
		return res
	}
	var facts []string
	for _, row := range rows {
		packet := "another"
		switch row.keyPacket {
		case sealed.SharedKeyPacket:
			packet = "the new one"
		case r.keyPacket:
			packet = "the old one"
		}
		facts = append(facts, "key packet "+packet+", "+partsFact(row, next.parts(uid)))
	}
	res.ReadBack = fmt.Sprintf("%d row(s)", len(rows))
	if len(facts) > 0 {
		res.ReadBack += ": " + strings.Join(facts, "; ")
	}
	return res
}

func caseSecondMaster(ctx context.Context, m *matrix, uid string) CaseResult {
	first, second := simple("20270302", "0", "first master"), simple("20270302", "0", "second master")
	res := CaseResult{Steps: []Step{
		m.create(ctx, "first create", uid, first, noNotifications, nil),
		m.create(ctx, "second create, same UID", uid, second, noNotifications, nil),
	}}
	rows, err := m.readBack(ctx, uid)
	if err != nil {
		res.ReadBack = readFailed(err)
		return res
	}
	masters, asFirst, asSecond := 0, 0, 0
	for _, r := range rows {
		if !r.recurrence {
			masters++
		}
		if partsFact(r, first.parts(uid)) == asSent {
			asFirst++
		} else if partsFact(r, second.parts(uid)) == asSent {
			asSecond++
		}
	}
	res.ReadBack = fmt.Sprintf("%d row(s), %d master(s); as the first create %d, as the second %d", len(rows), masters, asFirst, asSecond)
	return res
}

func rruleCase(day, rrule string) func(context.Context, *matrix, string) CaseResult {
	return func(ctx context.Context, m *matrix, uid string) CaseResult {
		k := simple(day, "0", "rrule", "RRULE:"+rrule)
		res := CaseResult{Steps: []Step{m.create(ctx, "create", uid, k, noNotifications, nil)}}
		rows, err := m.readBack(ctx, uid)
		if err != nil {
			res.ReadBack = readFailed(err)
			return res
		}
		facts := []string{fmt.Sprintf("%d row(s)", len(rows))}
		for _, r := range rows {
			col := "null"
			if r.rrule != nil {
				col = fmt.Sprintf("%q", *r.rrule)
			}
			if r.rrule != nil && *r.rrule == rrule {
				col += " (as sent)"
			} else {
				col += fmt.Sprintf(" (sent %q)", rrule)
			}
			facts = append(facts, "RRULE column "+col+", "+partsFact(r, k.parts(uid)))
		}
		res.ReadBack = strings.Join(facts, "; ")
		return res
	}
}

func caseTentative(ctx context.Context, m *matrix, uid string) CaseResult {
	k := simple("20270309", "0", "tentative")
	k.cal = []string{"STATUS:TENTATIVE"}
	res := CaseResult{Steps: []Step{m.create(ctx, "create", uid, k, noNotifications, nil)}}
	rows, err := m.readBack(ctx, uid)
	if err != nil {
		res.ReadBack = readFailed(err)
		return res
	}
	facts := []string{fmt.Sprintf("%d row(s)", len(rows))}
	for _, r := range rows {
		facts = append(facts, "STATUS "+partLine(r, ical.CalendarSigned, "STATUS")+" in the calendar signed part, "+partsFact(r, k.parts(uid)))
	}
	res.ReadBack = strings.Join(facts, "; ")
	return res
}

func timeCase(start, end string) func(context.Context, *matrix, string) CaseResult {
	return func(ctx context.Context, m *matrix, uid string) CaseResult {
		k := card{shared: []string{start, end, "SEQUENCE:0"}, enc: []string{"SUMMARY:pcb-lab wm time"}, cal: []string{}}
		res := CaseResult{Steps: []Step{m.create(ctx, "create", uid, k, noNotifications, nil)}}
		rows, err := m.readBack(ctx, uid)
		if err != nil {
			res.ReadBack = readFailed(err)
			return res
		}
		facts := []string{fmt.Sprintf("%d row(s)", len(rows))}
		for _, r := range rows {
			facts = append(facts, fmt.Sprintf("StartTimezone %q, StartTime %s, %s", r.startTZ, utc(r.start), partsFact(r, k.parts(uid))))
		}
		res.ReadBack = strings.Join(facts, "; ")
		return res
	}
}

func caseDuration(ctx context.Context, m *matrix, uid string) CaseResult {
	k := card{shared: []string{"DTSTART;TZID=" + paris + ":20270312T090000", "DURATION:PT45M", "SEQUENCE:0"}, enc: []string{"SUMMARY:pcb-lab wm duration"}, cal: []string{}}
	res := CaseResult{Steps: []Step{m.create(ctx, "create", uid, k, noNotifications, nil)}}
	rows, err := m.readBack(ctx, uid)
	if err != nil {
		res.ReadBack = readFailed(err)
		return res
	}
	facts := []string{fmt.Sprintf("%d row(s)", len(rows))}
	for _, r := range rows {
		facts = append(facts, fmt.Sprintf("EndTime - StartTime %d s, DURATION %s, DTEND %s in the shared signed part, %s",
			r.end-r.start, partLine(r, ical.SharedSigned, "DURATION"), partLine(r, ical.SharedSigned, "DTEND"), partsFact(r, k.parts(uid))))
	}
	res.ReadBack = strings.Join(facts, "; ")
	return res
}

func caseUnbumpedTime(ctx context.Context, m *matrix, uid string) CaseResult {
	first := simple("20270315", "0", "unbumped time")
	res := CaseResult{Steps: []Step{m.create(ctx, "create", uid, first, noNotifications, nil)}}
	r, stop := m.one(ctx, uid)
	if stop != "" {
		res.Steps = append(res.Steps, Step{What: "time edit, SEQUENCE unchanged", Why: "no single row to update"})
		res.ReadBack = stop
		return res
	}
	moved := card{
		shared: []string{"DTSTART;TZID=" + paris + ":20270315T103000", "DTEND;TZID=" + paris + ":20270315T113000", "SEQUENCE:0"},
		enc:    first.enc, cal: first.cal,
	}
	res.Steps = append(res.Steps, m.update(ctx, "time edit, SEQUENCE unchanged", uid, r, moved, noNotifications))
	after, stop := m.one(ctx, uid)
	if stop != "" {
		res.ReadBack = stop
		return res
	}
	shift := "unchanged by the update"
	if d := after.start - r.start; d != 0 {
		shift = fmt.Sprintf("moved %+d s by the update", d)
	}
	res.ReadBack = "1 row, StartTime " + shift + ", " + partsFact(after, moved.parts(uid))
	return res
}

func caseNotificationsAbsent(ctx context.Context, m *matrix, uid string) CaseResult {
	first := simple("20270316", "0", "notifications")
	reminder := events.Value(events.NotificationList{{Type: 1, Trigger: "-PT15M"}})
	res := CaseResult{Steps: []Step{m.create(ctx, "create with one reminder", uid, first, reminder, nil)}}
	r, stop := m.one(ctx, uid)
	if stop != "" {
		res.Steps = append(res.Steps, Step{What: "update, Notifications absent", Why: "no single row to update"})
		res.ReadBack = stop
		return res
	}
	next := simple("20270316", "1", "notifications renamed")
	res.Steps = append(res.Steps, m.update(ctx, "update, Notifications absent", uid, r, next, events.Optional[events.NotificationList]{}))
	after, stop := m.one(ctx, uid)
	if stop != "" {
		res.ReadBack = fmt.Sprintf("Notifications after the create %s; after the update: %s", r.notifications, stop)
		return res
	}
	res.ReadBack = fmt.Sprintf("Notifications after the create %s, after the update %s, %s", r.notifications, after.notifications, partsFact(after, next.parts(uid)))
	return res
}

func casePersonal(ctx context.Context, m *matrix, uid string) CaseResult {
	first := simple("20270317", "0", "personal part")
	text := fragment(uid, []string{"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT10M", "DESCRIPTION:pcb-lab", "END:VALARM"})
	res := CaseResult{}
	sig, err := m.signer.SignDetached(crypto.NewPlainMessage(text))
	var armored string
	if err == nil {
		armored, err = sig.GetArmored()
	}
	if err != nil {
		res.Steps = []Step{{What: "create with a personal part", Why: "signing failed"}}
		res.ReadBack = "0 row(s)"
		return res
	}
	personal := map[string]any{"PersonalEventContent": map[string]any{"Type": 2, "Data": string(text), "Signature": armored}}
	res.Steps = []Step{m.create(ctx, "create with a personal part", uid, first, noNotifications, personal)}
	r, stop := m.one(ctx, uid)
	if stop != "" {
		res.Steps = append(res.Steps, Step{What: "update leaving it out", Why: "no single row to update"})
		res.ReadBack = stop
		return res
	}
	if r.personal == 0 {
		res.Steps = append(res.Steps, Step{What: "update leaving it out", Why: "the create stored no personal part"})
		res.ReadBack = "1 row, personal parts after the create 0"
		return res
	}
	next := simple("20270317", "1", "personal part renamed")
	res.Steps = append(res.Steps, m.update(ctx, "update leaving it out", uid, r, next, noNotifications))
	after, stop := m.one(ctx, uid)
	if stop != "" {
		res.ReadBack = fmt.Sprintf("personal parts after the create %d; after the update: %s", r.personal, stop)
		return res
	}
	res.ReadBack = fmt.Sprintf("1 row, personal parts after the create %d, after the update %d, %s", r.personal, after.personal, partsFact(after, next.parts(uid)))
	return res
}

// cleanup deletes every row of the UIDs written, in one call, and reads
// them back. It never deletes a row whose UID lacks the run's prefix.
func (m *matrix) cleanup(ctx context.Context) string {
	var del []entry
	for _, uid := range m.uids {
		if !strings.HasPrefix(uid, m.prefix) {
			continue
		}
		rows, err := m.readBack(ctx, uid)
		if err != nil {
			return "listing " + uid + " failed: " + answerOf(err).String() + "; nothing deleted"
		}
		for _, r := range rows {
			del = append(del, entry{id: r.id})
		}
	}
	if len(del) == 0 {
		return "nothing to delete"
	}
	answers := m.sync(ctx, del...)
	refused := 0
	for _, a := range answers {
		if a.Status != http.StatusOK || (a.Code != 0 && a.Code != api.CodeOK) {
			refused++
		}
	}
	left := 0
	for _, uid := range m.uids {
		rows, err := m.readBack(ctx, uid)
		if err != nil {
			return fmt.Sprintf("delete %d row(s): %s, %d entr(ies) refused; read-back failed: %s", len(del), answers[0], refused, answerOf(err))
		}
		left += len(rows)
	}
	return fmt.Sprintf("delete %d row(s) in one call: HTTP %d, %d entr(ies) refused; left %d", len(del), answers[0].Status, refused, left)
}
