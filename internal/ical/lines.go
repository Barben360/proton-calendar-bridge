// Package ical is the iCalendar codec of the bridge: RFC 5545 content lines
// (unfold, parse, fold, serialise) kept byte-exact for content it does not
// model, the merge of Proton's decrypted card parts into one VEVENT, and the
// per-UID VCALENDAR that CalDAV serves; and its inverse for writes: what a
// client PUTs validated and normalised (validate.go), routed to card parts
// and patched line by line over the stored ones (write.go), its alarms made
// Proton notifications (alarms_write.go). Pure functions, no Proton or
// crypto imports.
package ical

import (
	"bytes"
	"fmt"
	"strings"
)

// Property is one content line, split but not interpreted: Name, Params (the
// raw text from the first ";" up to the value colon, ";" included, or empty)
// and Value. Name + Params + ":" + Value is the unfolded line, byte for byte.
type Property struct {
	Name   string
	Params string
	Value  string
}

// Line is the unfolded content line.
func (p Property) Line() string { return p.Name + p.Params + ":" + p.Value }

// UnescapeText is a TEXT value unescaped (RFC 5545 §3.3.11): "\\", "\;",
// "\," and "\n" or "\N" (a newline) become what they stand for; any other
// backslash is kept as written (ParseWrite refuses a UID holding one).
// Values are otherwise kept escaped, as written; this is for a UID, which
// Proton stores unescaped (SPEC §5.3).
func UnescapeText(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		switch c := v[i+1]; c {
		case '\\', ';', ',':
			b.WriteByte(c)
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte('\\')
			continue
		}
		i++
	}
	return b.String()
}

// EscapeText writes v as a TEXT value (RFC 5545 §3.3.11): a backslash,
// ";", "," and a newline are escaped; UnescapeText undoes it. pcb serves
// Proton's unescaped UID column so where no part carries the UID line
// (a placeholder).
func EscapeText(v string) string {
	if !strings.ContainsAny(v, "\\;,\n") {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\', ';', ',':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Item is a property or a nested component (Comp != nil), in source order.
type Item struct {
	Prop Property
	Comp *Component
}

// Component is a BEGIN/END block. Items keep the source order of properties
// and nested components.
type Component struct {
	Name  string
	Items []Item
}

// ParseError reports a malformed input by physical line number only: the
// content of a line is decrypted calendar data and never goes into an error.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("ical: line %d: %s", e.Line, e.Msg) }

// maxDepth caps component nesting (VCALENDAR > VEVENT > VALARM is 3).
const maxDepth = 8

type logicalLine struct {
	no   int
	text string
}

// unfold joins continuation lines (CRLF or LF followed by SP or TAB) and
// drops blank lines. The line number is that of the first physical line. A
// continuation right after a blank line is malformed. Chunks of one logical
// line are accumulated in a buffer, so hostile input stays linear.
func unfold(b []byte) ([]logicalLine, error) {
	var out []logicalLine
	var cur []byte
	curNo := 0
	flush := func() {
		if curNo > 0 {
			out = append(out, logicalLine{no: curNo, text: string(cur)})
		}
		cur, curNo = cur[:0], 0
	}
	no := 0
	blankBefore := false
	for len(b) > 0 {
		no++
		var line []byte
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			line, b = b, nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if curNo == 0 {
				return nil, &ParseError{Line: no, Msg: "continuation without a line to continue"}
			}
			if blankBefore {
				return nil, &ParseError{Line: no, Msg: "continuation after a blank line"}
			}
			cur = append(cur, line[1:]...)
			continue
		}
		if len(line) == 0 {
			blankBefore = true
			continue
		}
		blankBefore = false
		flush()
		curNo = no
		cur = append(cur, line...)
	}
	flush()
	return out, nil
}

// splitLine splits an unfolded line into name, params and value. The value
// starts at the first colon outside double quotes.
func splitLine(s string) (Property, bool) {
	inQuote := false
	nameEnd := -1
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == ';' && nameEnd < 0:
			nameEnd = i
		case c == ':':
			if nameEnd < 0 {
				nameEnd = i
			}
			if nameEnd == 0 {
				return Property{}, false
			}
			return Property{Name: s[:nameEnd], Params: s[nameEnd:i], Value: s[i+1:]}, true
		}
	}
	return Property{}, false
}

// Parse reads iCalendar text (CRLF or LF line ends, final line terminator
// optional) into its top-level components: a VCALENDAR, or a bare VEVENT.
func Parse(b []byte) ([]Component, error) {
	lines, err := unfold(b)
	if err != nil {
		return nil, err
	}
	var top []Component
	var stack []*Component
	var begins []int // physical line of each open BEGIN
	for _, l := range lines {
		p, ok := splitLine(l.text)
		if !ok {
			return nil, &ParseError{Line: l.no, Msg: "not a content line"}
		}
		switch {
		case strings.EqualFold(p.Name, "BEGIN") && p.Params == "":
			if len(stack) >= maxDepth {
				return nil, &ParseError{Line: l.no, Msg: "components nested too deeply"}
			}
			stack = append(stack, &Component{Name: strings.TrimSpace(p.Value)})
			begins = append(begins, l.no)
		case strings.EqualFold(p.Name, "END") && p.Params == "":
			if len(stack) == 0 {
				return nil, &ParseError{Line: l.no, Msg: "END without BEGIN"}
			}
			c := stack[len(stack)-1]
			if !strings.EqualFold(c.Name, strings.TrimSpace(p.Value)) {
				return nil, &ParseError{Line: l.no, Msg: "END does not match BEGIN"}
			}
			stack = stack[:len(stack)-1]
			begins = begins[:len(begins)-1]
			if len(stack) == 0 {
				top = append(top, *c)
			} else {
				parent := stack[len(stack)-1]
				parent.Items = append(parent.Items, Item{Comp: c})
			}
		default:
			if len(stack) == 0 {
				return nil, &ParseError{Line: l.no, Msg: "property outside a component"}
			}
			c := stack[len(stack)-1]
			c.Items = append(c.Items, Item{Prop: p})
		}
	}
	if len(stack) > 0 {
		return nil, &ParseError{Line: begins[len(begins)-1], Msg: "component not closed"}
	}
	return top, nil
}

// foldLine appends line folded at 75 octets (CRLF excluded; continuation lines
// start with one space) and a CRLF. A multi-byte UTF-8 sequence is never split.
func foldLine(dst []byte, line string) []byte {
	const limit = 75
	for first := true; ; first = false {
		room := limit
		if !first {
			dst = append(dst, ' ')
			room-- // the leading space counts
		}
		if len(line) <= room {
			break
		}
		n := room
		for n > 0 && line[n]&0xC0 == 0x80 { // line[n] would start mid-sequence
			n--
		}
		if n == 0 { // not UTF-8 at all: hard cut
			n = room
		}
		dst = append(dst, line[:n]...)
		dst = append(dst, '\r', '\n')
		line = line[n:]
	}
	dst = append(dst, line...)
	return append(dst, '\r', '\n')
}

// Bytes serialises the component with CRLF line ends (final line included),
// folded at 75 octets.
func (c Component) Bytes() []byte { return c.appendTo(nil) }

func (c Component) appendTo(dst []byte) []byte {
	dst = foldLine(dst, "BEGIN:"+c.Name)
	for _, it := range c.Items {
		if it.Comp != nil {
			dst = it.Comp.appendTo(dst)
		} else {
			dst = foldLine(dst, it.Prop.Line())
		}
	}
	return foldLine(dst, "END:"+c.Name)
}

// Props returns the component's own properties in order.
func (c Component) Props() []Property {
	var out []Property
	for _, it := range c.Items {
		if it.Comp == nil {
			out = append(out, it.Prop)
		}
	}
	return out
}

// Get returns the first property called name (case-insensitive).
func (c Component) Get(name string) (Property, bool) {
	for _, it := range c.Items {
		if it.Comp == nil && strings.EqualFold(it.Prop.Name, name) {
			return it.Prop, true
		}
	}
	return Property{}, false
}

// Subs returns the nested components called name (case-insensitive).
func (c Component) Subs(name string) []Component {
	var out []Component
	for _, it := range c.Items {
		if it.Comp != nil && strings.EqualFold(it.Comp.Name, name) {
			out = append(out, *it.Comp)
		}
	}
	return out
}

// Clone is a deep copy.
func (c Component) Clone() Component {
	out := Component{Name: c.Name, Items: make([]Item, len(c.Items))}
	for i, it := range c.Items {
		out.Items[i] = it
		if it.Comp != nil {
			sub := it.Comp.Clone()
			out.Items[i].Comp = &sub
		}
	}
	return out
}

// removeProps deletes every property for which drop returns true.
func (c *Component) removeProps(drop func(Property) bool) {
	kept := c.Items[:0:0]
	for _, it := range c.Items {
		if it.Comp == nil && drop(it.Prop) {
			continue
		}
		kept = append(kept, it)
	}
	c.Items = kept
}

// firstSub is the index of the first nested component in Items, or len(Items).
func (c Component) firstSub() int {
	for i, it := range c.Items {
		if it.Comp != nil {
			return i
		}
	}
	return len(c.Items)
}
