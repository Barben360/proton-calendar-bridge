package service

// The names a calendar's objects are served at (rulings M4-R6, M4-R22,
// M4-R38). Each UID has exactly one served name and each served name one
// UID, in every read (listings, multiget, sync-collection, deletions
// included) and every write:
//
//   - its alias, when a client created it at a name of its own choice
//     (PutObject records it; it outlives the UID's deletion, M4-R22);
//   - else its canonical name (ObjectName), unless another UID's alias
//     holds that name (an alias made before the UID existed): then its
//     alternative name, "~~" + its unpadded base64url + ".ics" (M4-R38).
//
// Why that is one UID per name: aliases are one per name (the cache's
// primary key); canonical names and alternative names are each injective;
// no canonical name starts with "~~" (a plain UID has no "~", and "~" +
// base64url never decodes a "~"); a canonical name held by an alias is not
// served as canonical; and an alternative name is never another UID's alias
// (PutObject refuses it, pcb:bad-resource-name).

import (
	"encoding/base64"
	"strings"
)

// alternativeName is uid's alternative resource name (ruling M4-R38).
func alternativeName(uid string) string {
	return "~~" + base64.RawURLEncoding.EncodeToString([]byte(uid)) + ".ics"
}

// uidOfAlternativeName is alternativeName's inverse, exact.
func uidOfAlternativeName(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, ".ics")
	if !ok {
		return "", false
	}
	enc, ok := strings.CutPrefix(stem, "~~")
	if !ok {
		return "", false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(enc)
	if err != nil || len(b) == 0 || alternativeName(string(b)) != name {
		return "", false
	}
	return string(b), true
}

// nameIndex is a calendar's aliases, both ways, as one snapshot holds them.
type nameIndex struct {
	alias map[string]string // UID → its alias
	held  map[string]string // alias → its UID
}

// newNames indexes aliases (UID to name, View.AliasesFor).
func newNames(aliases map[string]string) nameIndex {
	n := nameIndex{alias: aliases, held: make(map[string]string, len(aliases))}
	for uid, name := range aliases {
		n.held[name] = uid
	}
	return n
}

// served is the name uid is served at, whether it has rows or not (a
// deletion is reported at it).
func (n nameIndex) served(uid string) string {
	if a, ok := n.alias[uid]; ok {
		return a
	}
	c := ObjectName(uid)
	if other, ok := n.held[c]; ok && other != uid {
		return alternativeName(uid)
	}
	return c
}

// standsFor is the UID name refers to, whether served there or not: the
// UID of its alias, else the UID it is the canonical or the alternative
// name of; "" for none.
func (n nameIndex) standsFor(name string) string {
	if uid, ok := n.held[name]; ok {
		return uid
	}
	if uid, ok := UIDOfName(name); ok {
		return uid
	}
	if uid, ok := uidOfAlternativeName(name); ok {
		return uid
	}
	return ""
}

// resolve is the UID served at name, "" if none: a name a UID stands for
// but is not served at (the canonical name of an aliased UID) names nothing.
func (n nameIndex) resolve(name string) string {
	if uid := n.standsFor(name); uid != "" && n.served(uid) == name {
		return uid
	}
	return ""
}
