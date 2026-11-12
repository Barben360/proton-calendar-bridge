package caldav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// The namespaces the server speaks, with the prefixes it writes them with
// (clients match by namespace, E08 §5).
const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsCS     = "http://calendarserver.org/ns/"
	nsApple  = "http://apple.com/ns/ical/"
)

var prefixes = map[string]string{nsDAV: "D", nsCalDAV: "C", nsCS: "CS", nsApple: "A"}

func dav(local string) xml.Name    { return xml.Name{Space: nsDAV, Local: local} }
func caldav(local string) xml.Name { return xml.Name{Space: nsCalDAV, Local: local} }

// Limits of a request body's XML: nesting, element count and attributes per
// element (namespace declarations included, ruling M3-R27) bound what a body
// may cost beyond its size cap (maxQueryBody).
const (
	maxDepth    = 32
	maxElements = 10_000
	maxAttrs    = 64
)

var (
	errDoctype  = errors.New("caldav: a DOCTYPE in a request body")
	errTooDeep  = errors.New("caldav: request XML nested too deep")
	errTooLarge = errors.New("caldav: request XML has too many elements")
	errNoRoot   = errors.New("caldav: request XML has no root element")
	errBadName  = errors.New("caldav: request XML has an element name that is not an XML name")
	// errTooManyAttrs is checked once the decoder parsed the element: an
	// element of the 8 MiB body still costs its attributes once.
	errTooManyAttrs = errors.New("caldav: request XML has an element with too many attributes")
)

// ncName reports whether s is an XML name without a colon (XML 1.0 fifth
// edition NameStartChar and NameChar), so an element named by a request
// can be written back as is.
func ncName(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for i, r := range s {
		start := r == '_' || 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || 0xC0 <= r && r <= 0xD6 || 0xD8 <= r && r <= 0xF6 ||
			0xF8 <= r && r <= 0x2FF || 0x370 <= r && r <= 0x37D || 0x37F <= r && r <= 0x1FFF || 0x200C <= r && r <= 0x200D ||
			0x2070 <= r && r <= 0x218F || 0x2C00 <= r && r <= 0x2FEF || 0x3001 <= r && r <= 0xD7FF || 0xF900 <= r && r <= 0xFDCF ||
			0xFDF0 <= r && r <= 0xFFFD || 0x10000 <= r && r <= 0xEFFFF
		more := r == '-' || r == '.' || '0' <= r && r <= '9' || r == 0xB7 || 0x300 <= r && r <= 0x36F || 0x203F <= r && r <= 0x2040
		if !start && (i == 0 || !more) {
			return false
		}
	}
	return true
}

// node is an element of a request body, namespaces resolved; text is its
// character data (all of it, its children's excluded).
type node struct {
	name xml.Name
	kids []*node
	text []byte
}

// child returns the first child named name, nil if none.
func (n *node) child(name xml.Name) *node {
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

// parseXML parses a request body into its root element. A DOCTYPE (or any
// other directive) is refused outright, so no entity is ever declared, let
// alone expanded: encoding/xml knows only the five predefined ones.
func parseXML(body []byte) (*node, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var root *node
	var stack []*node
	elements := 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch tok := tok.(type) {
		case xml.Directive:
			return nil, errDoctype
		case xml.StartElement:
			if elements++; elements > maxElements {
				return nil, errTooLarge
			}
			if len(stack) >= maxDepth {
				return nil, errTooDeep
			}
			if !ncName(tok.Name.Local) { // encoding/xml lets "<:/>" through
				return nil, errBadName
			}
			if len(tok.Attr) > maxAttrs {
				return nil, errTooManyAttrs
			}
			n := &node{name: tok.Name}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("caldav: two root elements")
				}
				root = n
			} else {
				top := stack[len(stack)-1]
				top.kids = append(top.kids, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.text = append(top.text, tok...)
			}
		}
	}
	if root == nil {
		return nil, errNoRoot
	}
	return root, nil
}

// badBody answers a body parseXML refused: 413 when it has too many
// elements, else 400; never with the parser's text.
func badBody(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooLarge) {
		empty(w, http.StatusRequestEntityTooLarge)
		return
	}
	empty(w, http.StatusBadRequest)
}

// escape appends s as XML character data (attr: as an attribute value).
// CR is written as a reference so it survives the parser's end-of-line
// normalisation (calendar data keeps its CRLF); a byte or rune XML cannot
// carry becomes U+FFFD.
func escape(b *bytes.Buffer, s string, attr bool) {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '\r':
			b.WriteString("&#13;")
		case attr && r == '"':
			b.WriteString("&quot;")
		case attr && (r == '\n' || r == '\t'):
			b.WriteString("&#" + strconv.Itoa(int(r)) + ";")
		case r == utf8.RuneError && size == 1, !xmlChar(r):
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
}

// xmlChar is XML 1.0's Char production.
func xmlChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF
}

// tag writes the start (or end) tag of name: a known namespace with its
// prefix, any other with a local declaration.
func tag(b *bytes.Buffer, name xml.Name, end bool) {
	b.WriteByte('<')
	if end {
		b.WriteByte('/')
	}
	switch p, ok := prefixes[name.Space]; {
	case ok:
		b.WriteString(p + ":" + name.Local)
	case name.Space == "":
		b.WriteString(name.Local)
	default:
		b.WriteString("x:" + name.Local)
		if !end {
			b.WriteString(` xmlns:x="`)
			escape(b, name.Space, true)
			b.WriteByte('"')
		}
	}
	b.WriteByte('>')
}

// element writes name around inner, raw XML.
func element(b *bytes.Buffer, name xml.Name, inner string) {
	tag(b, name, false)
	b.WriteString(inner)
	tag(b, name, true)
}

// text is s as escaped character data.
func text(s string) string {
	var b bytes.Buffer
	escape(&b, s, false)
	return b.String()
}

// hrefXML is a DAV:href holding href.
func hrefXML(href string) string { return "<D:href>" + text(href) + "</D:href>" }

// prop is a property value: its name and raw inner XML.
type prop struct {
	name  xml.Name
	inner string
}

// multistatus builds a 207 body: no whitespace anywhere between or inside
// leaf elements (E08 AVOID).
type multistatus struct{ b bytes.Buffer }

func newMultistatus() *multistatus {
	m := &multistatus{}
	m.b.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CS="http://calendarserver.org/ns/" xmlns:A="http://apple.com/ns/ical/">`)
	return m
}

// props writes a response for href: the found properties in a 200 propstat,
// the missing ones in a 404 propstat.
func (m *multistatus) props(href string, found []prop, missing []xml.Name) {
	m.b.WriteString("<D:response>" + hrefXML(href))
	if len(found) > 0 || len(missing) == 0 {
		m.b.WriteString("<D:propstat><D:prop>")
		for _, p := range found {
			element(&m.b, p.name, p.inner)
		}
		m.b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
	}
	if len(missing) > 0 {
		m.b.WriteString("<D:propstat><D:prop>")
		for _, n := range missing {
			element(&m.b, n, "")
		}
		m.b.WriteString("</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>")
	}
	m.b.WriteString("</D:response>")
}

// notFound writes href's response-level 404, alone (E08 §3.2, §3.4).
func (m *multistatus) notFound(href string) {
	m.b.WriteString("<D:response>" + hrefXML(href) + "<D:status>HTTP/1.1 404 Not Found</D:status></D:response>")
}

func (m *multistatus) syncToken(token string) {
	m.b.WriteString("<D:sync-token>" + text(token) + "</D:sync-token>")
}

func (m *multistatus) send(w http.ResponseWriter) {
	m.b.WriteString("</D:multistatus>")
	w.Header().Set("Content-Type", xmlType)
	w.Header().Set("Content-Length", strconv.Itoa(m.b.Len()))
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write(m.b.Bytes())
}

const xmlType = "application/xml; charset=utf-8"

// The DAV:error bodies, exactly. A 403 never carries an empty DAV:error:
// Thunderbird reads that as rate limiting (E08 §3.3).
const (
	errValidSyncToken  = `<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`
	errSupportedReport = `<D:error xmlns:D="DAV:"><D:supported-report/></D:error>`
	errFiniteDepth     = `<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`
)

// forbidden answers 403 with a DAV:error body.
func forbidden(w http.ResponseWriter, body string) { xmlError(w, http.StatusForbidden, body) }

// xmlError answers status with a DAV:error body.
func xmlError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", xmlType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// empty answers status with no body.
func empty(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}
