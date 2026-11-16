package loginflow

import "time"

// SetNow replaces the flow's clock (time.Now).
func (f *Flow) SetNow(now func() time.Time) { f.now = now }
