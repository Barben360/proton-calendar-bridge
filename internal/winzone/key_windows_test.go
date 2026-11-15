//go:build windows

package winzone

import "testing"

func TestKeyIsReadable(t *testing.T) {
	k, ok := Key()
	if !ok || k == "" {
		t.Fatalf("Key() = %q, %v", k, ok)
	}
	if _, mapped := IANA(k); !mapped {
		t.Errorf("zone key %q is not in the CLDR table", k)
	}
}
