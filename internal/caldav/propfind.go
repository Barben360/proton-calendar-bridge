package caldav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"net/http"
	"slices"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

var errBadRequest = errors.New("caldav: not a request this server understands")

// propRequest is what a PROPFIND (or a report's prop selection) asks for.
type propRequest struct {
	all   bool       // allprop; props then holds its include list
	names bool       // propname
	props []xml.Name // prop
}

func (q propRequest) wants(name xml.Name) bool { return slices.Contains(q.props, name) }

// maxProps caps the distinct property names a prop or include may list
// (ruling M3-R27): each is written back on every resource, so the list
// bounds the response. Thunderbird asks for 7 at most.
const maxProps = 64

// propSelection reads the prop, allprop or propname child of n; none is
// allprop.
func propSelection(n *node) (propRequest, error) {
	if p := n.child(dav("prop")); p != nil {
		props, err := names(p)
		return propRequest{props: props}, err
	}
	if n.child(dav("propname")) != nil {
		return propRequest{names: true}, nil
	}
	q := propRequest{all: true}
	if inc := n.child(dav("include")); inc != nil {
		var err error
		q.props, err = names(inc)
		return q, err
	}
	return q, nil
}

// names lists the names of n's children, each once; more than maxProps is
// errBadRequest.
func names(n *node) ([]xml.Name, error) {
	seen := make(map[xml.Name]bool, len(n.kids))
	out := make([]xml.Name, 0, len(n.kids))
	for _, k := range n.kids {
		if seen[k.name] {
			continue
		}
		if len(out) == maxProps {
			return nil, errBadRequest
		}
		seen[k.name] = true
		out = append(out, k.name)
	}
	return out, nil
}

// parsePropfind reads a PROPFIND body; an empty one is allprop (RFC 4918).
func parsePropfind(body []byte) (propRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return propRequest{all: true}, nil
	}
	root, err := parseXML(body)
	if err != nil {
		return propRequest{}, err
	}
	if root.name != dav("propfind") || root.child(dav("prop")) == nil && root.child(dav("allprop")) == nil && root.child(dav("propname")) == nil {
		return propRequest{}, errBadRequest
	}
	return propSelection(root)
}

// liveProp is a property a resource has; hidden ones are left out of
// allprop.
type liveProp struct {
	prop
	hidden bool
}

// resource is a resource's href and properties, in the order written.
type resource struct {
	href  string
	props []liveProp
}

func (res *resource) add(name xml.Name, inner string) {
	res.props = append(res.props, liveProp{prop: prop{name: name, inner: inner}})
}

func (res *resource) addHidden(name xml.Name, inner string) {
	res.props = append(res.props, liveProp{prop: prop{name: name, inner: inner}, hidden: true})
}

func (res *resource) lookup(name xml.Name) (prop, bool) {
	for _, p := range res.props {
		if p.name == name {
			return p.prop, true
		}
	}
	return prop{}, false
}

// answer splits what q asks of res into found and missing properties.
func (res *resource) answer(q propRequest) (found []prop, missing []xml.Name) {
	switch {
	case q.names:
		for _, p := range res.props {
			found = append(found, prop{name: p.name})
		}
		return found, nil
	case q.all:
		for _, p := range res.props {
			if !p.hidden {
				found = append(found, p.prop)
			}
		}
	}
	for _, name := range q.props {
		p, ok := res.lookup(name)
		switch {
		case !ok:
			missing = append(missing, name)
		case !q.all || res.isHidden(name):
			found = append(found, p)
		}
	}
	return found, missing
}

func (res *resource) isHidden(name xml.Name) bool {
	for _, p := range res.props {
		if p.name == name {
			return p.hidden
		}
	}
	return false
}

func (res *resource) write(ms *multistatus, q propRequest) {
	found, missing := res.answer(q)
	ms.props(res.href, found, missing)
}

var principalHref = hrefXML(principalPath)

func rootResource() *resource {
	res := &resource{href: "/"}
	res.add(dav("resourcetype"), "<D:collection/>")
	res.add(dav("current-user-principal"), principalHref)
	return res
}

func principalResource() *resource {
	res := &resource{href: principalPath}
	res.add(dav("resourcetype"), "<D:collection/><D:principal/>")
	res.add(dav("current-user-principal"), principalHref)
	res.add(dav("principal-URL"), principalHref)
	res.add(caldav("calendar-home-set"), hrefXML(homePath))
	return res
}

func homeResource() *resource {
	res := &resource{href: homePath}
	res.add(dav("resourcetype"), "<D:collection/>")
	res.add(dav("current-user-principal"), principalHref)
	res.add(dav("owner"), principalHref)
	return res
}

// The privileges a calendar advertises (rulings M3-R1, M3-R15): a writable
// one says so already, though writes are refused until M4.
const (
	readPrivileges  = "<D:privilege><D:read/></D:privilege>"
	writePrivileges = readPrivileges + "<D:privilege><D:write/></D:privilege><D:privilege><D:write-content/></D:privilege>" +
		"<D:privilege><D:bind/></D:privilege><D:privilege><D:unbind/></D:privilege>"
)

func calendarResource(c service.Collection) *resource {
	res := &resource{href: calendarHref(c.ID)}
	res.add(dav("resourcetype"), "<D:collection/><C:calendar/>")
	if c.Name != "" {
		res.add(dav("displayname"), text(c.Name))
	}
	if c.Color != "" {
		res.add(xml.Name{Space: nsApple, Local: "calendar-color"}, text(c.Color))
	}
	if c.Description != "" {
		res.add(caldav("calendar-description"), text(c.Description))
	}
	res.add(caldav("supported-calendar-component-set"), `<C:comp name="VEVENT"/>`)
	res.add(caldav("supported-calendar-data"), `<C:calendar-data content-type="text/calendar" version="2.0"/>`)
	res.add(dav("supported-report-set"), "<D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report>"+
		"<D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report>")
	privileges := writePrivileges
	if c.ReadOnly {
		privileges = readPrivileges
	}
	res.add(dav("current-user-privilege-set"), privileges)
	res.add(xml.Name{Space: nsCS, Local: "getctag"}, text(c.CTag))
	res.addHidden(dav("sync-token"), text(c.SyncToken)) // RFC 6578 §4: not in allprop
	res.add(dav("owner"), principalHref)
	res.add(dav("current-user-principal"), principalHref)
	return res
}

// objectResource is an object's properties; calendar-data only when data
// was read (never in allprop, RFC 4791 §9.6).
func objectResource(calID, uid, etag string, data []byte, haveData bool) *resource {
	res := &resource{href: objectHref(calID, uid)}
	res.add(dav("resourcetype"), "")
	res.add(dav("getetag"), text(etag))
	res.add(dav("getcontenttype"), componentType)
	if haveData {
		res.addHidden(caldav("calendar-data"), text(string(data)))
	}
	return res
}

// propfind answers PROPFIND at Depth 0 or 1; infinity (or no Depth, which
// means infinity) is 403 propfind-finite-depth.
func (s *server) propfind(w http.ResponseWriter, r *http.Request, t target) {
	depth := r.Header.Get("Depth")
	switch depth {
	case "0", "1":
	case "", "infinity":
		forbidden(w, errFiniteDepth)
		return
	default:
		empty(w, http.StatusBadRequest)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	q, err := parsePropfind(body)
	if err != nil {
		badBody(w, err)
		return
	}
	ms := newMultistatus()
	switch t.kind {
	case kindRoot:
		rootResource().write(ms, q)
	case kindPrincipal:
		principalResource().write(ms, q)
	case kindHome:
		homeResource().write(ms, q)
		if depth == "1" {
			colls, err := s.api.Collections(r.Context())
			if err != nil {
				s.fail(w, r, err)
				return
			}
			for _, c := range colls {
				if !c.Pending { // listed once synced (ruling M3-R25)
					calendarResource(c).write(ms, q)
				}
			}
		}
	case kindCalendar:
		c, ok := s.collection(w, r, t.calseg)
		if !ok {
			return
		}
		calendarResource(c).write(ms, q)
		if depth == "1" && !s.listObjects(w, r, ms, c, q) {
			return
		}
	case kindObject:
		c, o, ok := s.object(w, r, t)
		if !ok {
			return
		}
		objectResource(c.ID, o.UID, o.ETag, o.Data, true).write(ms, q)
	}
	ms.send(w)
}

// listObjects writes the calendar's objects into ms: from the listing (no
// decryption) unless calendar-data is asked for.
func (s *server) listObjects(w http.ResponseWriter, r *http.Request, ms *multistatus, c service.Collection, q propRequest) bool {
	infos, _, err := s.api.Objects(r.Context(), c.ID)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if !q.wants(caldav("calendar-data")) {
		for _, o := range infos {
			objectResource(c.ID, o.UID, o.ETag, nil, false).write(ms, q)
		}
		return true
	}
	uids := make([]string, len(infos))
	for i, o := range infos {
		uids[i] = o.UID
	}
	objs, err := s.api.ObjectsByUID(r.Context(), c.ID, uids)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	for _, o := range objs {
		if !o.Missing { // gone since the listing
			objectResource(c.ID, o.UID, o.ETag, o.Data, true).write(ms, q)
		}
	}
	return true
}
