package cli

import (
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// SetLocalZone replaces the zone of date bounds and printed times, returning
// the function that restores it.
func SetLocalZone(z *time.Location) (restore func()) {
	old := localZone
	localZone = z
	return func() { localZone = old }
}

// APIHub is the one-account ctl.Hub the daemon serves, over api.
func APIHub(api service.API) ctl.Hub { return apiHub{api} }
