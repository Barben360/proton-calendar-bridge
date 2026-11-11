package ical

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
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
		"no colon":                "BEGIN:VEVENT\r\nUID:1\r\n" + secret + "\r\nEND:VEVENT\r\n",
		"unterminated":            "BEGIN:VEVENT\r\nSUMMARY:" + secret + "\r\n",
		"stray end":               "END:VEVENT\r\n",
		"mismatch":                "BEGIN:VEVENT\r\nSUMMARY:" + secret + "\r\nEND:VCALENDAR\r\n",
		"outside":                 "SUMMARY:" + secret + "\r\n",
		"leading space":           " " + secret + "\r\n",
		"blank then continuation": "BEGIN:VEVENT\r\nSUMMARY:a\r\n\r\n " + secret + "\r\nEND:VEVENT\r\n",
		"too deep":                strings.Repeat("BEGIN:X\r\n", 9) + secret + "\r\n",
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

func TestParseErrorLineNumbers(t *testing.T) {
	cases := []struct {
		src  string
		line int
	}{
		{"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:1\r\n", 2}, // innermost unclosed BEGIN
		{"BEGIN:VEVENT\r\nSUMMARY:a\r\n\r\n b\r\nEND:VEVENT\r\n", 4},
		{strings.Repeat("BEGIN:X\r\n", 9), 9},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.src))
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Line != c.line {
			t.Errorf("%q: want line %d, got %v", c.src, c.line, err)
		}
	}
	if _, err := Parse([]byte(strings.Repeat("BEGIN:X\r\n", 8) + strings.Repeat("END:X\r\n", 8))); err != nil {
		t.Errorf("depth 8 must parse: %v", err)
	}
}

func TestUnfoldHostileContinuationsIsLinear(t *testing.T) {
	const n = 200000
	src := []byte("BEGIN:VEVENT\r\nSUMMARY:x")
	src = append(src, bytes.Repeat([]byte("\n a"), n)...)
	src = append(src, "\nEND:VEVENT\n"...)
	start := time.Now()
	comps, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second { // quadratic concatenation takes minutes
		t.Fatalf("unfold took %v", d)
	}
	if p, _ := comps[0].Get("SUMMARY"); len(p.Value) != n+1 {
		t.Fatalf("value length %d", len(p.Value))
	}
}
