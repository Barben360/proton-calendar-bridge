package events_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// scripted is an httptest server answering every request with h, counting
// them; the client has no retry sleeps and a short timeout.
func scripted(t *testing.T, h http.HandlerFunc) (*api.Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return scriptedClient(srv.URL, nil), &n
}

func scriptedClient(base string, hc *http.Client) *api.Client {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	return api.New(api.Tokens{UID: "u", AccessToken: "a"}, api.Options{
		BaseURL: base, HTTP: hc, Limiter: rate.NewLimiter(rate.Inf, 1),
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

var oneUpdate = []events.Op{{Update: &events.Update{ID: "row1", Body: events.Body{}}}}

func TestBodyJSONShape(t *testing.T) {
	t.Parallel()
	enc := func(b events.Body) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Notifications: absent, null, [] and an array are four different bodies.
	for _, c := range []struct {
		name    string
		n       events.Optional[events.NotificationList]
		present bool
		want    string
	}{
		{"absent", events.Optional[events.NotificationList]{}, false, ""},
		{"null", events.Null[events.NotificationList](), true, "null"},
		{"empty", events.Value(events.NotificationList{}), true, "[]"},
		{"nil list", events.Value(events.NotificationList(nil)), true, "[]"},
		{"array", events.Value(events.NotificationList{{Type: 1, Trigger: "-PT15M"}, {Type: 0, Trigger: "-P1D"}}), true,
			`[{"Type":1,"Trigger":"-PT15M"},{"Type":0,"Trigger":"-P1D"}]`},
	} {
		got, ok := enc(events.Body{Notifications: c.n})["Notifications"]
		if ok != c.present || (ok && string(got) != c.want) {
			t.Errorf("Notifications %s: present=%v %s, want present=%v %s", c.name, ok, got, c.present, c.want)
		}
	}
	for _, c := range []struct {
		name    string
		v       events.Optional[string]
		present bool
		want    string
	}{
		{"absent", events.Optional[string]{}, false, ""},
		{"null", events.Null[string](), true, "null"},
		{"value", events.Value("#8080FF"), true, `"#8080FF"`},
	} {
		got, ok := enc(events.Body{Color: c.v})["Color"]
		if ok != c.present || (ok && string(got) != c.want) {
			t.Errorf("Color %s: present=%v %s, want present=%v %s", c.name, ok, got, c.present, c.want)
		}
	}
	// A bare body: lists are [], never null; Permissions is 1; no key packets.
	m := enc(events.Body{})
	for _, k := range []string{"SharedEventContent", "CalendarEventContent", "AttendeesEventContent", "Attendees"} {
		if string(m[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, m[k])
		}
	}
	if string(m["Permissions"]) != "1" {
		t.Errorf("Permissions = %s, want 1", m["Permissions"])
	}
	for _, k := range []string{"SharedKeyPacket", "CalendarKeyPacket", "IsOrganizer"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s is sent on an empty body", k)
		}
	}
	one := 1
	m = enc(events.Body{
		WireEvent:   events.WireEvent{SharedKeyPacket: "kp"},
		IsOrganizer: &one,
		Attendees:   events.AttendeeList{{Token: "tok", Status: 3}},
	})
	if string(m["SharedKeyPacket"]) != `"kp"` || string(m["IsOrganizer"]) != "1" ||
		!strings.Contains(string(m["Attendees"]), `"Token":"tok"`) || !strings.Contains(string(m["Attendees"]), `"Status":3`) {
		t.Errorf("body = %v", m)
	}
}

// capture runs Sync against a server that records the body and answers ok.
func captureBody(t *testing.T, ops []events.Op) map[string]json.RawMessage {
	t.Helper()
	var body []byte
	var method, path string
	c, _ := scripted(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body, _ = readAll(r)
		reply(w, 200, `{"Code":1001,"Responses":[]}`)
	})
	if _, err := events.Sync(t.Context(), c, "cal/1", "member1", ops); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || path != "/calendar/v1/cal/1/events/sync" {
		t.Errorf("request %s %s", method, path)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSyncEnvelope(t *testing.T) {
	t.Parallel()
	m := captureBody(t, []events.Op{
		{Create: &events.Body{WireEvent: events.WireEvent{SharedKeyPacket: "kp"}}},
		{Update: &events.Update{ID: "r2"}},
		{Delete: &events.Delete{ID: "r3"}},
	})
	if string(m["MemberID"]) != `"member1"` || string(m["IsImport"]) != "0" {
		t.Errorf("envelope = %v", m)
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(m["Events"], &entries); err != nil || len(entries) != 3 {
		t.Fatalf("entries = %s (%v)", m["Events"], err)
	}
	if string(entries[0]["Overwrite"]) != "0" || entries[0]["ID"] != nil || entries[0]["Event"] == nil {
		t.Errorf("create entry = %v", entries[0])
	}
	if string(entries[1]["ID"]) != `"r2"` || entries[1]["Event"] == nil || entries[1]["Overwrite"] != nil {
		t.Errorf("update entry = %v", entries[1])
	}
	if string(entries[2]["ID"]) != `"r3"` || entries[2]["Event"] != nil || entries[2]["Overwrite"] != nil {
		t.Errorf("delete entry = %v", entries[2])
	}
	// IsImport goes with creates only.
	m = captureBody(t, []events.Op{{Delete: &events.Delete{ID: "r3"}}})
	if _, ok := m["IsImport"]; ok {
		t.Errorf("a call of deletes sends IsImport: %v", m)
	}
}

func TestSyncRefusesMalformedOpsWithoutSending(t *testing.T) {
	t.Parallel()
	c, n := scripted(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, `{"Code":1001}`) })
	for name, ops := range map[string][]events.Op{
		"none":              nil,
		"empty op":          {{}},
		"two kinds":         {{Create: &events.Body{}, Delete: &events.Delete{ID: "x"}}},
		"update without ID": {{Update: &events.Update{}}},
		"delete without ID": {{Delete: &events.Delete{}}},
	} {
		if _, err := events.Sync(t.Context(), c, "cal", "m", ops); err == nil || errors.Is(err, events.ErrAmbiguous) {
			t.Errorf("%s: err = %v, want a plain refusal", name, err)
		}
	}
	if _, err := events.Sync(t.Context(), c, "cal", "", oneUpdate); err == nil {
		t.Error("an empty member ID is accepted")
	}
	if n.Load() != 0 {
		t.Errorf("%d requests sent for refused calls", n.Load())
	}
}

func TestSyncMapsEntryCodes(t *testing.T) {
	t.Parallel()
	const leak = "LEAKED-PROTON-TEXT"
	entry := func(i, code int, ev string) string {
		e := ""
		if ev != "" {
			e = `,"Event":` + ev
		}
		return fmt.Sprintf(`{"Index":%d,"Response":{"Code":%d,"Error":%q%s}}`, i, code, leak, e)
	}
	codes := []int{1000, 2001, 2000, 2011, 2501, 2061, 2999}
	var parts []string
	for i, code := range codes {
		ev := ""
		if code == 1000 {
			ev = `{"ID":"new1","UID":"u1","CalendarID":"cal"}`
		}
		parts = append(parts, entry(i, code, ev))
	}
	c, _ := scripted(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, `{"Code":1001,"Responses":[`+strings.Join(parts, ",")+`]}`)
	})
	ops := make([]events.Op, len(codes))
	for i := range ops {
		ops[i] = events.Op{Update: &events.Update{ID: "r" + strconv.Itoa(i)}}
	}
	res, err := events.Sync(t.Context(), c, "cal", "m", ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(codes) {
		t.Fatalf("%d results, want %d", len(res), len(codes))
	}
	want := []error{nil, events.ErrRefused, events.ErrStatus, events.ErrInvalid, events.ErrGone, events.ErrNotFound, events.ErrRefused}
	all := []error{events.ErrRefused, events.ErrStatus, events.ErrInvalid, events.ErrGone, events.ErrNotFound}
	for i, r := range res {
		if r.Index != i || r.Code != codes[i] {
			t.Errorf("result %d = {Index %d, Code %d}, want code %d", i, r.Index, r.Code, codes[i])
		}
		if want[i] == nil {
			if r.Err != nil || r.Row == nil || r.Row.ID != "new1" || r.Row.UID != "u1" {
				t.Errorf("result 0 = %+v, want success with the returned row", r)
			}
			continue
		}
		if r.Row != nil {
			t.Errorf("result %d carries a row on failure", i)
		}
		for _, k := range all {
			if got := errors.Is(r.Err, k); got != errors.Is(k, want[i]) {
				t.Errorf("code %d: errors.Is(%v) = %v", codes[i], k, got)
			}
		}
		var ee *events.EntryError
		if !errors.As(r.Err, &ee) || ee.Code != codes[i] || ee.Index != i {
			t.Errorf("code %d: no EntryError keeping the raw code: %v", codes[i], r.Err)
		}
		if strings.Contains(r.Err.Error(), leak) || !strings.Contains(r.Err.Error(), strconv.Itoa(codes[i])) {
			t.Errorf("code %d: error text %q must hold the code and none of Proton's text", codes[i], r.Err)
		}
	}
	if !errors.Is(events.FirstError(res), res[1].Err) {
		t.Errorf("FirstError = %v, want the first failing entry's", events.FirstError(res))
	}
	if events.FirstError(res[:1]) != nil {
		t.Error("FirstError of a success is not nil")
	}
}

func TestSyncDeleteHasNoResponses(t *testing.T) {
	t.Parallel()
	c, _ := scripted(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, `{"Code":1001,"Responses":[]}`) })
	res, err := events.Sync(t.Context(), c, "cal", "m", []events.Op{{Delete: &events.Delete{ID: "a"}}, {Delete: &events.Delete{ID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Index != 0 || res[1].Index != 1 {
		t.Fatalf("results = %+v, want one per delete", res)
	}
	for _, r := range res {
		if r.Code != 1000 || r.Err != nil || r.Row != nil {
			t.Errorf("delete result = %+v, want success without a row", r)
		}
	}
	// A create or update that gets no answer cannot be called a success or a
	// failure: Proton may have applied it.
	res, err = events.Sync(t.Context(), c, "cal", "m", []events.Op{{Delete: &events.Delete{ID: "a"}}, {Update: &events.Update{ID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil || !errors.Is(res[1].Err, events.ErrAmbiguous) {
		t.Errorf("results = %+v, want the delete done and the unanswered update ambiguous", res)
	}
}

func TestSyncIsNeverRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		retry     string
		ambiguous bool
	}{
		{"503", 503, "", true},
		{"500", 500, "", true},
		{"429", 429, "7", false},
	} {
		c, n := scripted(t, func(w http.ResponseWriter, _ *http.Request) {
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			reply(w, tc.status, `{"Code":2000,"Error":"busy"}`)
		})
		res, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
		if res != nil || err == nil {
			t.Fatalf("%s: res=%v err=%v, want an error", tc.name, res, err)
		}
		if n.Load() != 1 {
			t.Errorf("%s: the server saw %d requests, want exactly 1", tc.name, n.Load())
		}
		if errors.Is(err, events.ErrAmbiguous) != tc.ambiguous {
			t.Errorf("%s: ambiguous = %v, want %v (%v)", tc.name, !tc.ambiguous, tc.ambiguous, err)
		}
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Status != tc.status {
			t.Errorf("%s: the Proton error is not kept: %v", tc.name, err)
		}
		if tc.retry != "" && ae.RetryAfter != 7*time.Second {
			t.Errorf("%s: RetryAfter = %v, want 7s", tc.name, ae.RetryAfter)
		}
		// Session-wide as M2 classifies it, so callers answer 503.
		if !api.SessionWide(t.Context(), err) {
			t.Errorf("%s: api.SessionWide is false for %v", tc.name, err)
		}
	}
}

func TestSessionEndedIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	// A 401 is rejected before processing; with no refresh available the
	// session has ended, which is not ambiguous.
	c, n := scripted(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, 401, `{"Code":401,"Error":"x"}`) })
	_, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
	if !errors.Is(err, api.ErrSessionEnded) || errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("err = %v, want session ended and not ambiguous", err)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1", n.Load())
	}
}

func TestTimeoutAfterSendIsAmbiguous(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	received := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = readAll(r)
		received <- struct{}{}
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close
	c := scriptedClient(srv.URL, &http.Client{Timeout: 150 * time.Millisecond})
	res, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
	if res != nil || !errors.Is(err, events.ErrAmbiguous) {
		t.Fatalf("res=%v err=%v, want ErrAmbiguous", res, err)
	}
	select {
	case <-received:
	default:
		t.Fatal("the server never received the body: the timeout was not after the send")
	}
	if len(received) != 0 {
		t.Error("the timed-out write was re-sent")
	}
	if !api.SessionWide(t.Context(), err) {
		t.Errorf("api.SessionWide is false for %v", err)
	}
}

func TestResetAfterSendIsAmbiguous(t *testing.T) {
	t.Parallel()
	c, n := scripted(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = readAll(r)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	_, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
	if !errors.Is(err, events.ErrAmbiguous) || n.Load() != 1 {
		t.Errorf("err = %v after %d requests, want ErrAmbiguous after 1", err, n.Load())
	}
}

func TestCancelledAfterSendIsAmbiguous(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = readAll(r)
		cancel() // the caller gives up once the request has arrived
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	_, err := events.Sync(ctx, scriptedClient(srv.URL, nil), "cal", "m", oneUpdate)
	if !errors.Is(err, events.ErrAmbiguous) || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want ErrAmbiguous wrapping context.Canceled", err)
	}
}

func TestRefusedConnectionIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + l.Addr().String()
	_ = l.Close()
	_, err = events.Sync(t.Context(), scriptedClient(base, nil), "cal", "m", oneUpdate)
	if err == nil || errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("err = %v, want a failure that is not ambiguous: nothing reached Proton", err)
	}
	if !api.SessionWide(t.Context(), err) {
		t.Errorf("api.SessionWide is false for %v", err)
	}
}

func TestOtherRefusalsAreNotAmbiguous(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 403, 404, 422} {
		c, n := scripted(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, status, `{"Code":2001,"Error":"no"}`) })
		_, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
		var ae *api.Error
		if err == nil || errors.Is(err, events.ErrAmbiguous) || !errors.As(err, &ae) || ae.Status != status || n.Load() != 1 {
			t.Errorf("HTTP %d: err = %v after %d requests", status, err, n.Load())
		}
	}
}

func TestUnreadableAnswerAfterAcceptanceIsAmbiguous(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"not JSON":           `<html>`,
		"index out of range": `{"Code":1001,"Responses":[{"Index":5,"Response":{"Code":1000}}]}`,
		"index repeated":     `{"Code":1001,"Responses":[{"Index":0,"Response":{"Code":1000}},{"Index":0,"Response":{"Code":1000}}]}`,
		"unreadable row":     `{"Code":1001,"Responses":[{"Index":0,"Response":{"Code":1000,"Event":[1]}}]}`,
		"unknown top-level":  `{"Code":7000}`,
	} {
		c, _ := scripted(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, body) })
		res, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
		if res != nil || !errors.Is(err, events.ErrAmbiguous) {
			t.Errorf("%s: res=%v err=%v, want ErrAmbiguous", name, res, err)
		}
	}
}

// ---- against the fake: the contract ----

type rig struct {
	f      *protontest.Fake
	c      *api.Client
	cal    string
	member string
	signer *crypto.KeyRing
	calKey *crypto.KeyRing
}

func newRig(t *testing.T, o protontest.Options) *rig {
	t.Helper()
	f := protontest.New(t, o)
	c := api.New(f.Session(t, f.Username), api.Options{
		BaseURL: f.URL, Limiter: rate.NewLimiter(rate.Inf, 1), HTTP: &http.Client{Timeout: 10 * time.Second},
	})
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "W"})
	return &rig{f: f, c: c, cal: cal, member: f.MemberID(cal), signer: f.AddressKeyRing(), calKey: f.CalendarKeyRing(cal)}
}

func frag(uid string, lines ...string) string {
	all := append([]string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261111T090000Z"}, lines...)
	return strings.Join(append(all, "END:VEVENT", "END:VCALENDAR"), "\r\n")
}

func (r *rig) sign(t *testing.T, text string) string {
	t.Helper()
	// binary mode, as the fake requires
	sig, err := r.signer.SignDetached(crypto.NewPlainMessage([]byte(text)))
	if err != nil {
		t.Fatal(err)
	}
	armored, err := sig.GetArmored()
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

// body builds an event's wire body for uid under sk; create adds the key packet.
func (r *rig) body(t *testing.T, uid, summary string, sk *crypto.SessionKey, create bool) events.Body {
	t.Helper()
	signedText := frag(uid, "DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "SEQUENCE:0")
	encText := frag(uid, "SUMMARY:"+summary)
	data, err := sk.Encrypt(crypto.NewPlainMessage([]byte(encText)))
	if err != nil {
		t.Fatal(err)
	}
	b := events.Body{WireEvent: events.WireEvent{
		SharedEventContent: events.PartList{
			{Type: events.PartSigned, Data: signedText, Signature: r.sign(t, signedText)},
			{Type: events.PartEncrypted, Data: base64.StdEncoding.EncodeToString(data), Signature: r.sign(t, encText)},
		},
		CalendarEventContent: events.PartList{{Type: events.PartSigned, Data: frag(uid, "STATUS:CONFIRMED"), Signature: r.sign(t, frag(uid, "STATUS:CONFIRMED"))}},
	}}
	if create {
		kp, err := r.calKey.EncryptSessionKey(sk)
		if err != nil {
			t.Fatal(err)
		}
		b.SharedKeyPacket = base64.StdEncoding.EncodeToString(kp)
	}
	return b
}

func rowField(t *testing.T, r events.Row, name string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.Raw, &m); err != nil {
		t.Fatal(err)
	}
	return string(m[name])
}

func TestSyncAgainstTheFake(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{})
	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	create := r.body(t, "u1@pcb.test", "First", sk, true)
	create.Notifications = events.Value(events.NotificationList{{Type: 1, Trigger: "-PT15M"}})
	create.Color = events.Value("#8080FF")
	create.Attendees = nil
	res, err := events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Create: &create}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Err != nil || res[0].Code != 1000 || res[0].Row == nil || res[0].Row.UID != "u1@pcb.test" {
		t.Fatalf("create = %+v", res)
	}
	id := res[0].Row.ID
	if got := rowField(t, *res[0].Row, "Notifications"); got != `[{"Type":1,"Trigger":"-PT15M"}]` {
		t.Errorf("created Notifications = %s", got)
	}
	if got := rowField(t, *res[0].Row, "Color"); got != `"#8080FF"` {
		t.Errorf("created Color = %s", got)
	}

	// Update: no key packet; Notifications absent keeps, [] clears, null is null.
	upd := func(n events.Optional[events.NotificationList]) events.Row {
		t.Helper()
		b := r.body(t, "u1@pcb.test", "Second", sk, false)
		b.Notifications = n
		res, err := events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Update: &events.Update{ID: id, Body: b}}})
		if err != nil || len(res) != 1 || res[0].Err != nil {
			t.Fatalf("update: %v %+v", err, res)
		}
		rows, err := events.ListByUID(t.Context(), r.c, r.cal, "u1@pcb.test")
		if err != nil || len(rows) != 1 {
			t.Fatalf("read back: %v %d", err, len(rows))
		}
		return rows[0]
	}
	for _, c := range []struct {
		name string
		n    events.Optional[events.NotificationList]
		want string
	}{
		{"absent keeps", events.Optional[events.NotificationList]{}, `[{"Type":1,"Trigger":"-PT15M"}]`},
		{"empty clears", events.Value(events.NotificationList{}), `[]`},
		{"null is the defaults", events.Null[events.NotificationList](), `null`},
		{"array sets", events.Value(events.NotificationList{{Type: 0, Trigger: "-P1D"}}), `[{"Type":0,"Trigger":"-P1D"}]`},
	} {
		if got := rowField(t, upd(c.n), "Notifications"); got != c.want {
			t.Errorf("%s: Notifications = %s, want %s", c.name, got, c.want)
		}
	}

	// Delete: the answer has no responses; the row is gone.
	res, err = events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Delete: &events.Delete{ID: id}}})
	if err != nil || len(res) != 1 || res[0].Err != nil || res[0].Code != 1000 || res[0].Row != nil {
		t.Fatalf("delete = %v %+v", err, res)
	}
	if rows, _ := events.ListByUID(t.Context(), r.c, r.cal, "u1@pcb.test"); len(rows) != 0 {
		t.Errorf("%d rows after the delete", len(rows))
	}
	// Deleting it again: the fake says gone (2501); an ID never issued, not found (2061).
	res, err = events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Delete: &events.Delete{ID: id}}, {Delete: &events.Delete{ID: "never-issued"}}})
	if err != nil || len(res) != 2 {
		t.Fatalf("delete again = %v %+v", err, res)
	}
	if !errors.Is(res[0].Err, events.ErrGone) || !errors.Is(res[1].Err, events.ErrNotFound) {
		t.Errorf("codes %d/%d: errors %v / %v, want ErrGone and ErrNotFound", res[0].Code, res[1].Code, res[0].Err, res[1].Err)
	}
}

func TestSyncMapsTheFakesRefusals(t *testing.T) {
	t.Parallel()
	r := newRig(t, protontest.Options{})
	sk, _ := crypto.GenerateSessionKey()
	// A colour off the palette is 2011.
	b := r.body(t, "u2@pcb.test", "x", sk, true)
	b.Color = events.Value("#123456")
	res, err := events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Create: &b}})
	if err != nil || len(res) != 1 || !errors.Is(res[0].Err, events.ErrInvalid) || res[0].Code != 2011 {
		t.Errorf("off-palette colour: %v %+v", err, res)
	}
	// A create without a key packet is refused by code 2001, which stays
	// the combined ErrRefused: signature and sequence share it.
	b = r.body(t, "u2@pcb.test", "x", sk, false)
	res, err = events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Create: &b}})
	if err != nil || len(res) != 1 || !errors.Is(res[0].Err, events.ErrRefused) || res[0].Code != 2001 {
		t.Errorf("2001: %v %+v", err, res)
	}
	// A member that cannot write: the whole call is refused, not an entry.
	r.f.SetPermissions(r.cal, 1) // read only (0 serves everything)
	b = r.body(t, "u3@pcb.test", "x", sk, true)
	res, err = events.Sync(t.Context(), r.c, r.cal, r.member, []events.Op{{Create: &b}})
	var ae *api.Error
	if res != nil || !errors.As(err, &ae) || ae.Status != 403 || errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("read-only member: %v %v", res, err)
	}
}

func TestListByUIDPages(t *testing.T) {
	t.Parallel()
	f, c, cal := setup(t)
	for i := range 230 {
		f.AddEvent(cal, protontest.Event{UID: "same", Start: int64(1000 + i), End: int64(2000 + i)})
	}
	for i := range 40 {
		f.AddEvent(cal, protontest.Event{UID: "other" + strconv.Itoa(i), Start: 5, End: 6})
	}
	rows, err := events.ListByUID(t.Context(), c, cal, "same")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 230 {
		t.Fatalf("%d rows, want the 230 of the UID", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.UID != "same" || seen[r.ID] || r.CalendarID != cal || len(r.Raw) == 0 {
			t.Fatalf("row %+v: foreign, repeated or without its raw object", r)
		}
		seen[r.ID] = true
	}
	if n := f.Calls("GET", "/calendar/v1/"+cal+"/events"); n != 3 {
		t.Errorf("%d listing requests, want 3 (100+100+30)", n)
	}
	none, err := events.ListByUID(t.Context(), c, cal, "nobody")
	if err != nil || len(none) != 0 {
		t.Errorf("unknown UID: %v rows, err %v", len(none), err)
	}
	if _, err := events.ListByUID(t.Context(), c, cal, ""); err == nil {
		t.Error("an empty UID lists the whole calendar")
	}
}

func TestListByUIDSendsTheUID(t *testing.T) {
	t.Parallel()
	var got []string
	c, _ := scripted(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Get("UID")+"|"+r.URL.Query().Get("Page")+"|"+r.URL.Query().Get("PageSize"))
		reply(w, 200, `{"Code":1000,"Events":[],"Total":0}`)
	})
	if _, err := events.ListByUID(t.Context(), c, "cal", "a b&c=d@x"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "a b&c=d@x|0|100" {
		t.Errorf("queries = %q", got)
	}
}

// refreshing is a client with a Refresh that counts its calls.
func refreshing(base string, refreshes *atomic.Int32) *api.Client {
	return api.New(api.Tokens{UID: "u", AccessToken: "a", RefreshToken: "r"}, api.Options{
		BaseURL: base, HTTP: &http.Client{Timeout: 5 * time.Second}, Limiter: rate.NewLimiter(rate.Inf, 1),
		Sleep: func(context.Context, time.Duration) error { return nil },
		Refresh: func(_ context.Context, t api.Tokens) (api.Tokens, error) {
			refreshes.Add(1)
			t.AccessToken = "a2"
			return t, nil
		},
	})
}

// authServer answers by request number n (1-based) with h(n).
func authServer(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = readAll(r)
		h(int(n.Add(1)), w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// Ruling M4-R23: a 401 is answered before processing, so one resend after a
// refresh is allowed.
func TestSyncResendsOnceAfterA401Refresh(t *testing.T) {
	t.Parallel()
	var tokens []string
	base, n := authServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if n == 1 {
			reply(w, 401, `{"Code":401,"Error":"x"}`)
			return
		}
		reply(w, 200, `{"Code":1001,"Responses":[{"Index":0,"Response":{"Code":1000}}]}`)
	})
	var refreshes atomic.Int32
	res, err := events.Sync(t.Context(), refreshing(base, &refreshes), "cal", "m", oneUpdate)
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n.Load() != 2 || refreshes.Load() != 1 {
		t.Errorf("%d requests and %d refreshes, want 2 and 1", n.Load(), refreshes.Load())
	}
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Errorf("the resend did not carry the new token: %v", tokens)
	}
}

func TestResendAfter401ThenResetIsAmbiguous(t *testing.T) {
	t.Parallel()
	base, n := authServer(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			reply(w, 401, `{"Code":401,"Error":"x"}`)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	var refreshes atomic.Int32
	res, err := events.Sync(t.Context(), refreshing(base, &refreshes), "cal", "m", oneUpdate)
	if res != nil || !errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("res=%v err=%v, want ErrAmbiguous: the resend may have been applied", res, err)
	}
	if n.Load() != 2 {
		t.Errorf("%d requests, want 2", n.Load())
	}
}

// A second 401 ends the session: no third request. Should the guard
// regress, the third request is answered (and the call bounded), so the
// test fails on its assertions instead of hanging in a refresh loop.
func TestSecond401GivesNoThirdRequest(t *testing.T) {
	t.Parallel()
	base, n := authServer(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n > 2 {
			reply(w, 200, `{"Code":1001,"Responses":[]}`)
			return
		}
		reply(w, 401, `{"Code":401,"Error":"x"}`)
	})
	var refreshes atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := events.Sync(ctx, refreshing(base, &refreshes), "cal", "m", oneUpdate)
	if !errors.Is(err, api.ErrSessionEnded) || errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("err = %v, want session ended, not ambiguous", err)
	}
	if n.Load() != 2 || refreshes.Load() != 1 {
		t.Errorf("%d requests and %d refreshes, want 2 and 1", n.Load(), refreshes.Load())
	}
}

func TestCancelledBeforeSyncSendsNothing(t *testing.T) {
	t.Parallel()
	c, n := scripted(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, `{"Code":1001}`) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := events.Sync(ctx, c, "cal", "m", oneUpdate)
	if err == nil || errors.Is(err, events.ErrAmbiguous) || n.Load() != 0 {
		t.Errorf("err=%v after %d requests, want a non-ambiguous failure and none sent", err, n.Load())
	}
}

func TestUnresolvableHostIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	_, err := events.Sync(t.Context(), scriptedClient("http://nx.invalid", nil), "cal", "m", oneUpdate)
	if err == nil || errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("err = %v, want a dial failure that is not ambiguous", err)
	}
}

func TestTLSFailureIsAmbiguousOnPurpose(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { n.Add(1) }))
	t.Cleanup(srv.Close)
	// The client does not trust the test certificate: the handshake fails.
	_, err := events.Sync(t.Context(), scriptedClient(srv.URL, nil), "cal", "m", oneUpdate)
	if !errors.Is(err, events.ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous (the safe direction)", err)
	}
	if n.Load() != 0 {
		t.Errorf("the handler ran %d times", n.Load())
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	for _, status := range []int{307, 308} {
		var hits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
		t.Cleanup(target.Close)
		c, n := scripted(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/elsewhere", status)
		})
		_, err := events.Sync(t.Context(), c, "cal", "m", oneUpdate)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Status != status || errors.Is(err, events.ErrAmbiguous) {
			t.Errorf("%d: err = %v, want an *api.Error that is not ambiguous", status, err)
		}
		if n.Load() != 1 || hits.Load() != 0 {
			t.Errorf("%d: %d requests, %d at the target, want 1 and 0", status, n.Load(), hits.Load())
		}
	}
}
