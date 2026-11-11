package ical

import (
	"bytes"
	"testing"
)

func FuzzParse(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nSUMMARY:é\r\n x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	f.Add([]byte("BEGIN:VEVENT\nX;A=\"b:c\":d\n\te\nEND:VEVENT"))
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
		var out2 []byte
		for _, c := range again {
			out2 = append(out2, c.Bytes()...)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("unstable:\n%q\n%q", out, out2)
		}
	})
}
