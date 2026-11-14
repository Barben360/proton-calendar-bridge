package service

import (
	"encoding/base64"
	"testing"
)

// The served names (names.go, rulings M4-R6, M4-R38): an alias wins its
// name; a UID whose canonical name another UID's alias holds moves to its
// alternative name; a UID is reached at its served name only; and no two
// UIDs share a served name.
func TestServedNamesAreOneUIDPerName(t *testing.T) {
	t.Parallel()
	alt := func(uid string) string { return "~~" + base64.RawURLEncoding.EncodeToString([]byte(uid)) + ".ics" }
	n := newNames(map[string]string{
		"y@x":      "x@x.ics",     // holds x@x's canonical name
		"z@x":      "free.ics",    // a plain alias
		"gone@x":   "odd one.ics", // the alias of a UID with no rows: still its name (M4-R22)
		"odd uid/": "Odd%Name.ics",
	})
	for _, tc := range []struct{ uid, served string }{
		{"x@x", alt("x@x")},
		{"y@x", "x@x.ics"},
		{"z@x", "free.ics"},
		{"gone@x", "odd one.ics"},
		{"plain@x", "plain@x.ics"},
		{"odd uid/", "Odd%Name.ics"},
		{"other odd", "~" + base64.RawURLEncoding.EncodeToString([]byte("other odd")) + ".ics"},
	} {
		if got := n.served(tc.uid); got != tc.served {
			t.Errorf("served(%q) = %q, want %q", tc.uid, got, tc.served)
		}
		if got := n.resolve(tc.served); got != tc.uid {
			t.Errorf("resolve(%q) = %q, want %q", tc.served, got, tc.uid)
		}
	}
	for _, name := range []string{
		"y@x.ics", "z@x.ics", // canonical names of aliased UIDs
		alt("y@x"), alt("plain@x"), // alternative names of UIDs not served there
		"~~.ics", "~~bm90IGl0=.ics", "~~!!.ics", "~~eEB4", // not alternative names
		"nobody.ics.txt", "",
	} {
		if got := n.resolve(name); got != "" {
			t.Errorf("resolve(%q) = %q, want nothing", name, got)
		}
	}
	// What a write checks for conflicts: the UID a name stands for, served
	// there or not.
	for name, want := range map[string]string{"x@x.ics": "y@x", "z@x.ics": "z@x", alt("q@x"): "q@x", "free.ics": "z@x", "unknown name": ""} {
		if got := n.standsFor(name); got != want {
			t.Errorf("standsFor(%q) = %q, want %q", name, got, want)
		}
	}

	seen := map[string]string{}
	for _, uid := range []string{"x@x", "y@x", "z@x", "gone@x", "plain@x", "odd uid/", "free", "x@x.ics", "~~x", "not it", "Free.ics"} {
		s := n.served(uid)
		if other, dup := seen[s]; dup {
			t.Errorf("%q and %q are both served at %q", other, uid, s)
		}
		seen[s] = uid
	}
}

func TestAlternativeNamesDecodeExactly(t *testing.T) {
	t.Parallel()
	for _, uid := range []string{"x@x", "odd uid/é?#", "~~"} {
		got, ok := uidOfAlternativeName(alternativeName(uid))
		if !ok || got != uid {
			t.Errorf("uidOfAlternativeName(alternativeName(%q)) = %q, %v", uid, got, ok)
		}
		if _, canonical := UIDOfName(alternativeName(uid)); canonical {
			t.Errorf("alternativeName(%q) is a canonical name", uid)
		}
	}
	for _, name := range []string{"~~.ics", "~~eEB4=.ics", "~~eEB4", "~eEB4.ics", "~~eEB4.ICS", "~~eEB+.ics", "x~~eEB4.ics"} {
		if uid, ok := uidOfAlternativeName(name); ok {
			t.Errorf("uidOfAlternativeName(%q) = %q, want none", name, uid)
		}
	}
}
