package probe

// The write matrix's cases: what each one writes and what its read-back
// reports.

import (
	"context"
	"fmt"
	"strings"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

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
	// Sealed as a create (fresh session keys, packets to the primary
	// calendar key), sent as an update of the row.
	st, sealed := m.send(ctx, "update with new key packets", r.id, uid, next, cards.SealKeys{Signer: m.signer, CalendarPrimary: m.primary}, noNotifications, nil)
	res.Steps = append(res.Steps, st)
	if st.Answer == nil {
		res.ReadBack = "1 row"
		return res
	}
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
