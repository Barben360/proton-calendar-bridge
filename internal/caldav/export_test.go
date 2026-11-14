package caldav

import (
	"net/http"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// HandlerWithDataCap is Handler rendering at most n objects' calendar-data
// per PROPFIND Depth 1 or sync-collection (maxCalendarData in production),
// so a test reaches the cap with a few objects.
func HandlerWithDataCap(r service.Router, o Options, n int) http.Handler {
	s := Handler(r, o).(*server)
	s.dataCap = n
	return s
}

// WriteBudget is how long a PUT or DELETE keeps its connection usable.
const WriteBudget = writeBudget

// RefusalLogSize is the number of distinct refusals the log's dedup keeps.
const RefusalLogSize = refusalLogSize

// RefusalLogLen is how many distinct refusals h's dedup holds.
func RefusalLogLen(h http.Handler) int {
	l := h.(*server).refusals
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}
