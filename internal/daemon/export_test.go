package daemon

import (
	"log/slog"
	"net/http"
)

// RecordPanics is recordPanics, for the external tests.
func RecordPanics(h http.Handler, log *slog.Logger) http.Handler { return recordPanics(h, log) }
