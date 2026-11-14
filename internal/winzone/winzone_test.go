package winzone

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestIANAForFixedKeys(t *testing.T) {
	for key, want := range map[string]string{
		"W. Europe Standard Time": "Europe/Berlin", "Romance Standard Time": "Europe/Paris",
		"Eastern Standard Time": "America/New_York", "Pacific Standard Time": "America/Los_Angeles",
		"GMT Standard Time": "Europe/London", "Tokyo Standard Time": "Asia/Tokyo",
		"AUS Eastern Standard Time": "Australia/Sydney", "India Standard Time": "Asia/Calcutta",
		"Central Europe Standard Time": "Europe/Budapest",
	} {
		if got, ok := IANA(key); !ok || got != want {
			t.Errorf("IANA(%q) = %q, %v; want %q, true", key, got, ok, want)
		}
	}
}

func TestUnknownKey(t *testing.T) {
	for _, key := range []string{"Mars Standard Time", "", "w. europe standard time"} {
		if got, ok := IANA(key); ok || got != "" {
			t.Errorf("IANA(%q) = %q, %v; want \"\", false", key, got, ok)
		}
	}
}

func TestEveryMappedZoneLoads(t *testing.T) {
	if len(zones) < 130 {
		t.Fatalf("%d zones: the table is truncated", len(zones))
	}
	for key, name := range zones {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("%q maps to %q: %v", key, name, err)
		}
	}
}

func TestCLDRVersionIsRecorded(t *testing.T) {
	if CLDRVersion == "" || strings.Trim(CLDRVersion, "0123456789") != "" {
		t.Errorf("CLDRVersion = %q, want a release number", CLDRVersion)
	}
}
