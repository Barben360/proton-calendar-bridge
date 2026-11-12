package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// noteHandler is the logger of the commands other than pcb serve (ruling
// A2): what the service warns about (a login time in the future, a default
// client that could not be created) is one "Warning: ..." line on stderr,
// in pcb's message style: the message, its fields in parentheses, and a
// "Hint: ..." line when the record carries a hint. Below Warn nothing is
// printed. It never writes to stdout, so a --json envelope stays alone.
type noteHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	attrs []slog.Attr
}

// newNoteLogger is the commands' logger on w (stderr).
func newNoteLogger(w io.Writer) *slog.Logger {
	return slog.New(&noteHandler{mu: &sync.Mutex{}, w: w})
}

// Enabled implements slog.Handler.
func (*noteHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

// Handle implements slog.Handler.
func (h *noteHandler) Handle(_ context.Context, r slog.Record) error {
	label := "Warning"
	if r.Level >= slog.LevelError {
		label = "Error"
	}
	var fields []string
	hint := ""
	add := func(a slog.Attr) bool {
		v := a.Value.Resolve()
		switch {
		case a.Key == "hint":
			hint = v.String()
		case v.Kind() == slog.KindTime:
			fields = append(fields, a.Key+": "+v.Time().UTC().Format(time.RFC3339))
		case a.Key != "":
			fields = append(fields, a.Key+": "+v.String())
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	line := label + ": " + r.Message
	if len(fields) > 0 {
		line += " (" + strings.Join(fields, ", ") + ")"
	}
	if hint != "" {
		line += "\nHint: " + hint
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.w, line)
	return err
}

// WithAttrs implements slog.Handler.
func (h *noteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &noteHandler{mu: h.mu, w: h.w, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

// WithGroup implements slog.Handler: groups are flattened.
func (h *noteHandler) WithGroup(string) slog.Handler { return h }

type loggerKey struct{}

// withLogger is ctx carrying log, the logger NewService gives the service.
func withLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// loggerFrom is the logger withLogger put in ctx, nil without one.
func loggerFrom(ctx context.Context) *slog.Logger {
	log, _ := ctx.Value(loggerKey{}).(*slog.Logger)
	return log
}
