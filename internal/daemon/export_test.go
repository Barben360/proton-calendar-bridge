package daemon

import "net/http"

// RecordPanics is recordPanics, for the external tests.
func RecordPanics(h http.Handler) http.Handler { return recordPanics(h) }
