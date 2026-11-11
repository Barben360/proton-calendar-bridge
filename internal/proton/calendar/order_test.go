package calendar

import (
	"testing"

	gpa "github.com/ProtonMail/go-proton-api"
)

func TestAddressOrderPutsTheMemberAddressFirst(t *testing.T) {
	t.Parallel()
	addrs := []gpa.Address{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	var got string
	for _, a := range addressOrder(addrs, "b") {
		got += a.ID
	}
	if got != "bac" {
		t.Fatalf("order = %q, want bac", got)
	}
	got = ""
	for _, a := range addressOrder(addrs, "") { // unknown: plain address order
		got += a.ID
	}
	if got != "abc" {
		t.Fatalf("order = %q, want abc", got)
	}
}
