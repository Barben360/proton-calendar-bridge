// Package winzone names the process's time zone on Windows. Go reads no TZ
// there and exposes no IANA name, so the zone is found from the Windows
// zone key (Key) and mapped to an IANA name by the table generated from
// Unicode CLDR's windowsZones.xml (IANA).
//
// Regenerate the table with the file of the CLDR release to adopt:
//
//	curl -fsSLo /tmp/windowsZones.xml https://raw.githubusercontent.com/unicode-org/cldr/release-NN/common/supplemental/windowsZones.xml
//	CLDR_XML=/tmp/windowsZones.xml go generate ./internal/winzone
//
// The generator pins the SHA-256 of the file of every release it knows and
// refuses any other.
package winzone

//go:generate go run gen.go $CLDR_XML

// IANA is the IANA zone of a Windows zone key (the "TimeZoneKeyName", such
// as "W. Europe Standard Time"), by CLDR's primary (territory 001)
// mapping. Not ok for a key CLDR does not list.
func IANA(key string) (string, bool) {
	name, ok := zones[key]
	return name, ok
}
