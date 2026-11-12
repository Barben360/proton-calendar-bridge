package caldav

import (
	"net/http"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// HandlerWithDataCap is Handler rendering at most n objects' calendar-data
// per PROPFIND Depth 1 or sync-collection (maxCalendarData in production),
// so a test reaches the cap with a few objects.
func HandlerWithDataCap(api service.API, o Options, n int) http.Handler {
	s := Handler(api, o).(*server)
	s.dataCap = n
	return s
}

// WriteBudget is how long a PUT or DELETE keeps its connection usable.
const WriteBudget = writeBudget
