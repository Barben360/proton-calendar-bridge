package caldav_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/caldav"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// fuzzBody serves body with method on the Work calendar and checks what any
// answer must be: a known status; a 207 that is well-formed XML; any other
// body empty or one of the DAV:error preconditions.
func fuzzBody(t *testing.T, method, depth string, body []byte) {
	s := newStub()
	s.deltas["tok-1"] = service.ChangeSet{Changed: []service.ObjectInfo{{UID: plainUID, ETag: `"v1-plain"`}}, Deleted: []string{"gone@x"}, Token: "tok-2"}
	rec := do(caldav.Handler(s, caldav.Options{}), method, calPath(workID), string(body), "Depth", depth)
	switch rec.Code {
	case http.StatusMultiStatus:
		d := xml.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		for {
			_, err := d.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("207 body is not XML: %v\n%s", err, rec.Body)
			}
		}
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		if rec.Body.Len() != 0 {
			t.Fatalf("%d with body %q", rec.Code, rec.Body)
		}
	case http.StatusForbidden:
		switch rec.Body.String() {
		case validSyncTokenBody, supportedReportBody, finiteDepthBody:
		default:
			t.Fatalf("403 with body %q", rec.Body)
		}
	default:
		t.Fatalf("status %d for %q", rec.Code, body)
	}
}

func FuzzReportBody(f *testing.F) {
	for _, seed := range []string{
		tbSync(""), tbSync("tok-1"), tbSync("urn:pcb:sync:1:x:1:y"),
		tbMultiget(tbHref(workID, plainUID), tbHref(workID, oddUID), "/x", "%zz", "http://h/"+calPath(workID)+"~.ics"),
		`<C:calendar-query xmlns:C="urn:ietf:params:xml:ns:caldav"/>`,
		`<!DOCTYPE x [<!ENTITY e "x">]><sync-collection xmlns="DAV:"><sync-token>&e;</sync-token></sync-collection>`,
		`<sync-collection xmlns="DAV:"><sync-token/><sync-level>infinite</sync-level><prop><getetag/><x:y xmlns:x="u"/></prop></sync-collection>`,
		"", "<", "<a><b><c></c></b></a>",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) { fuzzBody(t, "REPORT", "1", body) })
}

func FuzzPropfindBody(f *testing.F) {
	for _, seed := range []string{
		tbDetect, tbHomeSet, tbServerInfo, tbEtags, "",
		`<D:propfind xmlns:D="DAV:"><D:allprop/><D:include><D:sync-token/></D:include></D:propfind>`,
		`<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`,
		`<D:propfind xmlns:D="DAV:"><D:prop><x:a xmlns:x="&quot;'&lt;"/><b xmlns=""/></D:prop></D:propfind>`,
		`<!DOCTYPE p SYSTEM "file:///etc/passwd"><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) { fuzzBody(t, "PROPFIND", "1", body) })
}
