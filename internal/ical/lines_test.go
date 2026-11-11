package ical

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFoldUnfoldRoundTrip(t *testing.T) {
	// Multi-byte runes at every alignment so some fold point lands inside one.
	for pad := 0; pad < 6; pad++ {
		val := strings.Repeat("a", pad) + strings.Repeat("é€😀", 60)
		src := "BEGIN:VEVENT\r\nSUMMARY:" + val + "\r\nEND:VEVENT\r\n"
		comps, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		out := comps[0].Bytes()
		if !utf8.Valid(out) {
			t.Fatalf("pad %d: output is not valid UTF-8 (a rune was split)", pad)
		}
		for _, l := range bytes.Split(bytes.TrimSuffix(out, []byte("\r\n")), []byte("\r\n")) {
			if len(l) > 75 {
				t.Fatalf("pad %d: line of %d octets", pad, len(l))
			}
		}
		if !bytes.Contains(out, []byte("\r\n ")) {
			t.Fatalf("pad %d: expected folding", pad)
		}
		again, err := Parse(out)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := again[0].Get("SUMMARY")
		if p.Value != val {
			t.Fatalf("pad %d: value changed by fold/unfold", pad)
		}
		if !bytes.Equal(again[0].Bytes(), out) {
			t.Fatalf("pad %d: not stable", pad)
		}
	}
}

func TestFoldExactly75Octets(t *testing.T) {
	line := "X-A:" + strings.Repeat("b", 71) // 75 octets: no fold
	out := foldLine(nil, line)
	if string(out) != line+"\r\n" {
		t.Fatalf("75-octet line must not fold: %q", out)
	}
	out = foldLine(nil, line+"c") // 76
	if string(out) != line+"\r\n c\r\n" {
		t.Fatalf("76-octet line must fold after 75: %q", out)
	}
}

func TestUnfoldAcceptsLFAndTab(t *testing.T) {
	src := "BEGIN:VEVENT\nSUMMARY:ab\n cd\n\tef\r\n gh\nEND:VEVENT"
	comps, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := comps[0].Get("SUMMARY")
	if p.Value != "abcdefgh" {
		t.Fatalf("got %q", p.Value)
	}
}

func TestParseBareVEVENTAndNoFinalCRLF(t *testing.T) {
	comps, err := Parse([]byte("BEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT"))
	if err != nil || len(comps) != 1 || comps[0].Name != "VEVENT" {
		t.Fatalf("%v %v", comps, err)
	}
}

func TestUnknownPropertiesSurvive(t *testing.T) {
	lines := []string{
		"X-MOZ-LASTACK:20261101T100000Z",
		"X-MOZ-SNOOZE-TIME-1793527200000000:20261101T101500Z",
		"X-PM-SHARED-EVENT-ID:abc==",
		`ATTENDEE;CN="Doe, John; Jr.";X-PM-TOKEN=tok1:mailto:j@example.com`,
		`X-WEIRD;P1="a:b,c";P2=d:value: with colon;and semicolon`,
		"X-EMPTY:",
		"lower-case;x=y:kept as is",
	}
	src := "BEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"
	comps, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(comps[0].Bytes()); got != src {
		t.Fatalf("not byte-exact:\n%q\n%q", got, src)
	}
	p, _ := comps[0].Get("X-WEIRD")
	if p.Params != `;P1="a:b,c";P2=d` || p.Value != "value: with colon;and semicolon" {
		t.Fatalf("params/value split wrong: %q %q", p.Params, p.Value)
	}
}

func TestParseErrorsCarryLineNumberNotContent(t *testing.T) {
	secret := "SECRETSUMMARYTEXT"
	cases := map[string]string{
		"no colon":      "BEGIN:VEVENT\r\nUID:1\r\n" + secret + "\r\nEND:VEVENT\r\n",
		"unterminated":  "BEGIN:VEVENT\r\nSUMMARY:" + secret + "\r\n",
		"stray end":     "END:VEVENT\r\n",
		"mismatch":      "BEGIN:VEVENT\r\nSUMMARY:" + secret + "\r\nEND:VCALENDAR\r\n",
		"outside":       "SUMMARY:" + secret + "\r\n",
		"leading space": " " + secret + "\r\n",
	}
	for name, src := range cases {
		_, err := Parse([]byte(src))
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: error leaks content: %v", name, err)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: not a *ParseError: %v", name, err)
		}
	}
	_, err := Parse([]byte("BEGIN:VEVENT\r\nUID:1\r\nbroken\r\nEND:VEVENT\r\n"))
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 3 {
		t.Fatalf("want line 3, got %v", err)
	}
}

func TestNestedComponentsKeepOrder(t *testing.T) {
	src := "BEGIN:VEVENT\r\nUID:1\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nEND:VALARM\r\nSUMMARY:after alarm\r\nEND:VEVENT\r\n"
	comps, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if string(comps[0].Bytes()) != src {
		t.Fatalf("order changed: %q", comps[0].Bytes())
	}
}
