package cli

import "time"

// SetLocalZone replaces the zone of date bounds and printed times, returning
// the function that restores it.
func SetLocalZone(z *time.Location) (restore func()) {
	old := localZone
	localZone = z
	return func() { localZone = old }
}
