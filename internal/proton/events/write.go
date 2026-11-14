package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// ListByUID returns every row of the calendar that has this UID (a master and
// its exceptions), paged as List pages. It is how a write reads its result
// back: the change feed lags the write by a second or two (research A.5).
func ListByUID(ctx context.Context, c *api.Client, calID, uid string) ([]Row, error) {
	if uid == "" {
		return nil, errors.New("listing events by UID: empty UID")
	}
	l, err := list(ctx, c, calID, url.Values{"UID": {uid}})
	return l.Rows, err
}

// Optional is a JSON field that is absent, null or a value: the three states
// Proton tells apart for Notifications and Color (research A.2, D). The zero
// value is absent and is omitted from the body.
type Optional[T any] struct {
	state optionalState
	v     T
}

type optionalState uint8

const (
	absent optionalState = iota
	null
	set
)

// Null is the field sent as null.
func Null[T any]() Optional[T] { return Optional[T]{state: null} }

// Value is the field sent as v.
func Value[T any](v T) Optional[T] { return Optional[T]{state: set, v: v} }

// IsZero reports whether the field is absent (encoding/json omitzero).
func (o Optional[T]) IsZero() bool { return o.state == absent }

// MarshalJSON encodes null, or the value; absent is left out by omitzero.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if o.state == set {
		return json.Marshal(o.v)
	}
	return []byte("null"), nil
}

// Notification is a reminder: Type 0 email, 1 device; Trigger a duration
// before the start such as -PT15M (research D).
type Notification struct {
	Type    int
	Trigger string
}

// NotificationList encodes as [] when empty or nil, never null: null means
// "the calendar's defaults" and is sent with Null.
type NotificationList []Notification

// MarshalJSON encodes l, [] when nil.
func (l NotificationList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Notification(l))
}

// Attendee is an attendee's status as the row holds it: re-sent on every
// update, because omitting the list wipes the RSVPs (research A.2).
type Attendee struct {
	Token   string
	Status  int
	Comment json.RawMessage
}

// AttendeeList encodes as [] when empty or nil, never null.
type AttendeeList []Attendee

// MarshalJSON encodes l, [] when nil.
func (l AttendeeList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Attendee(l))
}

// eventPermissions is the Permissions every bridge sends for its own events
// (research A.2).
const eventPermissions = 1

// Body is a sync entry's Event (research A.2): the card parts and key
// packets of WireEvent, and the plaintext fields around them.
type Body struct {
	WireEvent
	// Permissions defaults to 1 (every writer sends 1 for its own events).
	Permissions int
	// IsOrganizer is omitted when nil.
	IsOrganizer *int `json:",omitempty"`
	// Attendees: re-send the row's {Token, Status, Comment} on an update.
	Attendees AttendeeList
	// Notifications: absent is null, on an update as on a create (never the
	// row's own) [verified: live 2026-11-13, L.4 #10]; null is the
	// calendar's defaults; [] none; an array is custom (research D).
	Notifications Optional[NotificationList] `json:",omitzero"`
	// Color: absent or null keep the row's on an update; a palette colour
	// otherwise.
	Color Optional[string] `json:",omitzero"`
}

// MarshalJSON encodes b with Permissions defaulted.
func (b Body) MarshalJSON() ([]byte, error) {
	type plain Body // no methods: no recursion
	if b.Permissions == 0 {
		b.Permissions = eventPermissions
	}
	return json.Marshal(plain(b))
}

// Update replaces the row ID with Body. The Body carries no key packets: the
// row keeps its own (research A.1).
type Update struct {
	ID   string
	Body Body
}

// Delete removes the row ID.
type Delete struct{ ID string }

// Op is one operation of a Sync call: exactly one field is set.
type Op struct {
	Create *Body
	Update *Update
	Delete *Delete
}

func (o Op) kind() (n int, id string) {
	if o.Create != nil {
		n++
	}
	if o.Update != nil {
		n++
		id = o.Update.ID
	}
	if o.Delete != nil {
		n++
		id = o.Delete.ID
	}
	return n, id
}

// wireEntry is an Op as the endpoint takes it.
type wireEntry struct {
	Overwrite *int   `json:",omitempty"`
	ID        string `json:",omitempty"`
	Event     *Body  `json:",omitempty"`
}

func (o Op) wire() wireEntry {
	switch {
	case o.Create != nil:
		zero := 0
		return wireEntry{Overwrite: &zero, Event: o.Create}
	case o.Update != nil:
		return wireEntry{ID: o.Update.ID, Event: &o.Update.Body}
	}
	return wireEntry{ID: o.Delete.ID}
}

// Proton's per-entry codes (research A.4).
const (
	codeBadValue   = 2000 // STATUS:TENTATIVE
	codeSignOrSeq  = 2001 // signature or exception SEQUENCE: not told apart
	codeNotAllowed = 2011 // unsupported properties, a colour off the palette
	codeGone       = 2501
	codeNotFound   = 2061
)

// Per-entry failures, wrapped by EntryError (test with errors.Is). Proton's
// own message is never kept.
var (
	// ErrStatus: code 2000, a STATUS Proton refuses (TENTATIVE).
	ErrStatus = errors.New("proton refused the event status")
	// ErrInvalid: code 2011, properties or a colour Proton does not support.
	ErrInvalid = errors.New("proton refused unsupported event content")
	// ErrNotFound: code 2061, an ID or member Proton does not know.
	ErrNotFound = errors.New("proton does not know the event")
	// ErrRefused: any other refusal, including code 2001. Code 2001 is both
	// a bad signature and an exception SEQUENCE below the master's. Research
	// A.4 records two different messages, but we deliberately do not parse
	// Proton's text (ruling M4-R24): the two stay one error, and the caller
	// checks the SEQUENCE before sending.
	ErrRefused = errors.New("proton refused the write")
	// ErrAmbiguous: the request may have reached Proton without a usable
	// answer (timeout or reset after the body was sent, a 5xx, an answer
	// that cannot be read), so the write may have been applied. Read back
	// before deciding anything. It wraps the cause. ErrGone, above, is
	// code 2501.
	ErrAmbiguous = errors.New("the write may have been applied")
)

// EntryError is one entry's refusal: which operation, Proton's raw code, and
// a kind to test with errors.Is. It holds no text from Proton.
type EntryError struct {
	Index, Code int
	kind        error
}

// Error implements error.
func (e *EntryError) Error() string {
	return fmt.Sprintf("write entry %d: %v (code %d)", e.Index, e.kind, e.Code)
}

// Unwrap returns the kind.
func (e *EntryError) Unwrap() error { return e.kind }

func entryError(index, code int) *EntryError {
	kind := ErrRefused
	switch code {
	case codeBadValue:
		kind = ErrStatus
	case codeNotAllowed:
		kind = ErrInvalid
	case codeGone:
		kind = ErrGone
	case codeNotFound:
		kind = ErrNotFound
	}
	return &EntryError{Index: index, Code: code, kind: kind}
}

// Result is one operation's outcome, in the order of the Sync ops. Err is
// nil on success, an *EntryError for a refusal, or wraps ErrAmbiguous for an
// entry the answer does not mention. Row is the row Proton returned: always
// for a create, sometimes for an update, never for a delete.
type Result struct {
	Index, Code int
	Row         *Row
	Err         error
}

// FirstError returns the first failing entry's error, nil when all succeeded.
func FirstError(res []Result) error {
	for _, r := range res {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

type syncRequest struct {
	MemberID string
	IsImport *int `json:",omitempty"`
	Events   []wireEntry
}

type syncAnswer struct {
	Responses []struct {
		Index    *int
		Response struct {
			Code  int
			Event json.RawMessage
		}
	}
}

// Sync applies ops to the calendar in one PUT (research A.1) and returns one
// Result per op, in order. A refused entry is a Result with Err, not an
// error of Sync: entries stand alone, so the others may have been applied.
//
// The request is sent once, never retried (a write has no idempotency key).
// Sync returns a nil slice and an error when the call as a whole failed:
//   - wrapping ErrAmbiguous when the request may have reached Proton without
//     a usable answer (it left and the connection failed, timed out or was
//     cancelled, Proton answered 5xx, or the answer cannot be read): read the
//     rows back with ListByUID before deciding;
//   - an *api.Error for a 429 (refused before processing; RetryAfter says
//     when), another 4xx, or one wrapping api.ErrSessionEnded;
//   - a plain error when ops are malformed, before anything is sent, and for
//     a request that could not leave (api.ErrNotSent), such as a refused
//     connection.
//
// Every error keeps its cause, so api.SessionWide classifies them as it does
// a read.
func Sync(ctx context.Context, c *api.Client, calID, memberID string, ops []Op) ([]Result, error) {
	if memberID == "" {
		return nil, errors.New("writing events: empty member ID")
	}
	if len(ops) == 0 {
		return nil, errors.New("writing events: no operations")
	}
	req := syncRequest{MemberID: memberID, Events: make([]wireEntry, len(ops))}
	for i, op := range ops {
		n, id := op.kind()
		if n != 1 || (op.Create == nil && id == "") {
			return nil, fmt.Errorf("writing events: operation %d must be exactly one of create, update or delete, with an ID", i)
		}
		if op.Create != nil {
			zero := 0
			req.IsImport = &zero // on creates only (research A.1)
		}
		req.Events[i] = op.wire()
	}
	var ans syncAnswer
	if err := c.PutOnce(ctx, "/calendar/v1/"+url.PathEscape(calID)+"/events/sync", req, &ans); err != nil {
		if ambiguous(err) {
			return nil, fmt.Errorf("%w: %w", ErrAmbiguous, err)
		}
		return nil, fmt.Errorf("writing events: %w", err)
	}
	res := make([]Result, len(ops))
	answered := make([]bool, len(ops))
	for _, r := range ans.Responses {
		if r.Index == nil || *r.Index < 0 || *r.Index >= len(ops) || answered[*r.Index] {
			return nil, fmt.Errorf("%w: the answer to the write does not match its operations", ErrAmbiguous)
		}
		i := *r.Index
		answered[i] = true
		res[i] = Result{Index: i, Code: r.Response.Code}
		if r.Response.Code != api.CodeOK {
			res[i].Err = entryError(i, r.Response.Code)
			continue
		}
		if len(r.Response.Event) > 0 && string(r.Response.Event) != "null" {
			row, err := parseRow(r.Response.Event)
			if err != nil {
				return nil, fmt.Errorf("%w: the answer to the write holds an unreadable row", ErrAmbiguous)
			}
			res[i].Row = &row
		}
	}
	for i, op := range ops {
		switch {
		case answered[i]:
		case op.Delete != nil:
			// A delete that succeeds answers nothing (research A.3).
			res[i] = Result{Index: i, Code: api.CodeOK}
		default:
			res[i] = Result{Index: i, Err: fmt.Errorf("%w: write entry %d has no answer", ErrAmbiguous, i)}
		}
	}
	return res, nil
}

// ambiguous reports whether a failed PUT may have been applied. Not when it
// could not have left (api.ErrNotSent), when the session ended (a 401 is
// answered before processing), or when Proton answered with a 3xx or 4xx
// (429 included: rejected before processing). Yes for a 5xx or an unexpected
// 2xx, and for any transport failure once the request could have left:
// timeout, reset, read error, caller cancelled, an answer that does not decode.
// A TLS handshake failure and a Client.Timeout that fires while dialling are
// classed ambiguous on purpose although nothing was sent: they cannot be told
// from a failure after the send, and ambiguous is the safe direction (read
// back, then 503). Do not "fix" them to not-ambiguous.
func ambiguous(err error) bool {
	if errors.Is(err, api.ErrNotSent) {
		return false
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Status >= http.StatusInternalServerError || (ae.Status >= 200 && ae.Status < 300)
	}
	return true
}
