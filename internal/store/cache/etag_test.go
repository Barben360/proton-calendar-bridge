package cache

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustETag(t *testing.T, s string) string {
	t.Helper()
	e, err := ETag(json.RawMessage(s))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestETagCanonical(t *testing.T) {
	a := mustETag(t, `{"ID":"e1","Start":1731000000123,"Parts":[{"B":1,"A":"x"}]}`)
	b := mustETag(t, "{ \"Parts\": [ {\"A\":\"x\",  \"B\":1} ],\n \"Start\":1731000000123, \"ID\":\"e1\" }")
	if a != b {
		t.Errorf("reordered keys/spacing changed the ETag: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, "\"v1-") || !strings.HasSuffix(a, "\"") || len(a) != len(`"v1-`)+32+1 {
		t.Errorf("ETag shape = %s", a)
	}
	if c := mustETag(t, `{"ID":"e1","Start":1731000000124,"Parts":[{"B":1,"A":"x"}]}`); c == a {
		t.Error("a changed value kept the ETag")
	}
	if c := mustETag(t, `{"ID":"e1","Start":1731000000123,"Parts":[{"B":2,"A":"x"}]}`); c == a {
		t.Error("a changed nested value kept the ETag")
	}
}

// Numbers beyond float64 precision and HTML characters must be hashed as
// they are, not rounded or escaped.
func TestETagKeepsNumbersAndHTMLExact(t *testing.T) {
	if mustETag(t, `{"N":9007199254740993}`) == mustETag(t, `{"N":9007199254740992}`) {
		t.Error("numbers above 2^53 were rounded")
	}
	if mustETag(t, `{"N":1731000000123}`) == mustETag(t, `{"N":1731000000124}`) {
		t.Error("1731000000123 not kept exact")
	}
	if mustETag(t, `{"S":"a<b&c"}`) != mustETag(t, `{"S":"a\u003cb\u0026c"}`) {
		t.Error("equal strings written with and without escapes differ")
	}
	if mustETag(t, `{"S":"a<b&c"}`) == mustETag(t, `{"S":"a\\u003cb"}`) {
		t.Error("distinct strings collide")
	}
	// The canonical form is what is hashed: pin one value so a change of
	// canonicalisation (e.g. HTML escaping on) is noticed.
	if got, want := mustETag(t, `{"S":"<&>","B":1}`), `"v1-`+sha32(`{"B":1,"S":"<&>"}`)+`"`; got != want {
		t.Errorf("ETag = %s, want %s", got, want)
	}
}

func TestETagRejectsInvalidJSON(t *testing.T) {
	if _, err := ETag(json.RawMessage(`{"a":`)); err == nil {
		t.Error("truncated JSON accepted")
	}
	if _, err := ETag(json.RawMessage(`{} {}`)); err == nil {
		t.Error("trailing data accepted")
	}
}
