package ical

import (
	"bytes"
	"reflect"
	"testing"
)

func FuzzParse(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nSUMMARY:é\r\n x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	f.Add([]byte("BEGIN:VEVENT\nX;A=\"b:c\":d\n\te\nEND:VEVENT"))
	f.Add(append([]byte("BEGIN:VEVENT\nSUMMARY:x"), bytes.Repeat([]byte("\n a"), 2000)...))
	f.Add([]byte("BEGIN:VEVENT\nA:b\n\n c\nEND:VEVENT"))
	f.Add([]byte("garbage\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		comps, err := Parse(in)
		if err != nil {
			return
		}
		var out []byte
		for _, c := range comps {
			out = append(out, c.Bytes()...)
		}
		again, err := Parse(out)
		if err != nil {
			t.Fatalf("reparse of own output failed: %v", err)
		}
		if !reflect.DeepEqual(comps, again) {
			t.Fatalf("structures differ after round trip")
		}
		var out2 []byte
		for _, c := range again {
			out2 = append(out2, c.Bytes()...)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("unstable:\n%q\n%q", out, out2)
		}
	})
}

func FuzzMergeEvent(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nEXDATE:1\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), []byte("BEGIN:VEVENT\nSUMMARY:x\nX-PM-SESSION-KEY:k\nEND:VEVENT"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		ev, err := MergeEvent([][]byte{a, b})
		if err != nil {
			return
		}
		ApplyRow(&ev, "#abc", map[string]int{"t": 1})
		if _, err := Parse(ev.Bytes()); err != nil {
			t.Fatalf("merged output does not parse: %v", err)
		}
	})
}
