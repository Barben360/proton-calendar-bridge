package caldav

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// report answers REPORT: sync-collection and calendar-multiget on a
// calendar. Any other report, or one of ours elsewhere, is 403
// supported-report (ruling M3-R23); the service is not asked.
func (s *server) report(w http.ResponseWriter, r *http.Request, t target) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	root, err := parseXML(body)
	if err != nil {
		badBody(w, err)
		return
	}
	switch {
	case t.kind == kindCalendar && root.name == dav("sync-collection"):
		s.syncCollection(w, r, t, root)
	case t.kind == kindCalendar && root.name == caldav("calendar-multiget"):
		s.multiget(w, r, t, root)
	default:
		forbidden(w, errSupportedReport)
	}
}

// syncCollection answers RFC 6578's report as the service computed the
// changes (ruling M3-R11): changed objects with the asked properties,
// deletions as response-level 404s, and the new token. An empty token lists
// everything; one the service cannot answer from is 403 valid-sync-token.
// Depth is ignored (Thunderbird sends Depth: 1), and so is a DAV:limit: the
// answer is never paged (ruling M3-R23).
func (s *server) syncCollection(w http.ResponseWriter, r *http.Request, t target, root *node) {
	tok, sel := root.child(dav("sync-token")), root.child(dav("prop"))
	if tok == nil || sel == nil {
		empty(w, http.StatusBadRequest)
		return
	}
	if lvl := root.child(dav("sync-level")); lvl != nil {
		// A calendar holds no collection: both levels list the same.
		if v := strings.TrimSpace(string(lvl.text)); v != "1" && v != "infinite" {
			empty(w, http.StatusBadRequest)
			return
		}
	}
	props, err := names(sel)
	if err != nil {
		empty(w, http.StatusBadRequest)
		return
	}
	q := propRequest{props: props}
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return
	}
	cs, err := s.api.Changes(r.Context(), c.ID, strings.TrimSpace(string(tok.text)))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ms := newMultistatus()
	deleted := cs.Deleted
	if q.wants(caldav("calendar-data")) {
		uids := make([]string, len(cs.Changed))
		for i, o := range cs.Changed {
			uids[i] = o.UID
		}
		objs, err := s.api.ObjectsByUID(r.Context(), c.ID, uids)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, o := range objs {
			if o.Missing { // deleted since the changes were read
				deleted = append(deleted, o.UID)
				continue
			}
			objectResource(c.ID, o.UID, o.ETag, o.Data, true).write(ms, q)
		}
	} else {
		for _, o := range cs.Changed {
			objectResource(c.ID, o.UID, o.ETag, nil, false).write(ms, q)
		}
	}
	for _, uid := range deleted {
		ms.notFound(objectHref(c.ID, uid))
	}
	ms.syncToken(cs.Token)
	ms.send(w)
}

// multiget answers RFC 4791's calendar-multiget: one response per href, in
// order; an href that names no object of this calendar (missing, another
// calendar's, a name that does not decode) is a response-level 404, never
// an empty propstat (E08 §3.4). At most maxHrefs hrefs, else 413.
func (s *server) multiget(w http.ResponseWriter, r *http.Request, t target, root *node) {
	var hrefs []string
	for _, k := range root.kids {
		if k.name == dav("href") {
			hrefs = append(hrefs, strings.TrimSpace(string(k.text)))
		}
	}
	switch {
	case len(hrefs) == 0:
		empty(w, http.StatusBadRequest)
		return
	case len(hrefs) > maxHrefs:
		empty(w, http.StatusRequestEntityTooLarge)
		return
	}
	q, err := propSelection(root)
	if err != nil {
		empty(w, http.StatusBadRequest)
		return
	}
	c, ok := s.collection(w, r, t.calseg)
	if !ok {
		return
	}
	// uidOf[i] is the UID hrefs[i] names in this calendar, "" if none.
	uidOf := make([]string, len(hrefs))
	var uids []string
	for i, h := range hrefs {
		u, err := url.Parse(h)
		if err != nil {
			continue
		}
		if ht := resolve(u.Path); ht.kind == kindObject && ht.calseg == t.calseg {
			uidOf[i] = ht.uid
			uids = append(uids, ht.uid)
		}
	}
	var objs []service.Object
	if len(uids) > 0 {
		var err error
		if objs, err = s.api.ObjectsByUID(r.Context(), c.ID, uids); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	ms := newMultistatus()
	next := 0
	for i, h := range hrefs {
		if uidOf[i] == "" {
			ms.notFound(h)
			continue
		}
		var o service.Object
		if next < len(objs) {
			o = objs[next]
		}
		next++
		if o.Missing || o.UID != uidOf[i] {
			ms.notFound(objectHref(c.ID, uidOf[i]))
			continue
		}
		objectResource(c.ID, o.UID, o.ETag, o.Data, true).write(ms, q)
	}
	ms.send(w)
}
