package report

import (
	"context"
	"runtime"
	"sync"
	"time"
)

// Process is what the running binary records its reports with: where they
// wait, what they say of the process and where they would go. No DSN
// records nothing.
type Process struct {
	Store Store
	Meta  Meta
	DSN   string

	mu   sync.Mutex
	subs map[*chan struct{}]struct{}
}

var (
	current   *Process
	currentMu sync.Mutex
)

// Install makes p the process's recorder, replacing any other.
func Install(p *Process) {
	currentMu.Lock()
	defer currentMu.Unlock()
	current = p
}

// Uninstall removes the process's recorder.
func Uninstall() { Install(nil) }

// Current is the installed recorder, nil when there is none.
func Current() *Process {
	currentMu.Lock()
	defer currentMu.Unlock()
	return current
}

// Subscribe gives a channel that gets a signal (never blocking the
// recorder, one signal waiting at most) each time a report is saved, and
// the function that ends the subscription.
func (p *Process) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	if p.subs == nil {
		p.subs = map[*chan struct{}]struct{}{}
	}
	p.subs[&ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, &ch)
		p.mu.Unlock()
	}
}

func (p *Process) notify() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case *ch <- struct{}{}:
		default:
		}
	}
}

func (p *Process) now() time.Time {
	if p.Store.Now != nil {
		return p.Store.Now()
	}
	return time.Now()
}

// saveWait bounds how long a report waits for the store's lock: it is
// saved on the caller's path (a request, a command), which must not stall.
const saveWait = time.Second

func (p *Process) save(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), saveWait)
	defer cancel()
	if p.Store.Save(ctx, e) == nil {
		p.notify()
	}
}

// RecordError keeps a report of err, an error that points to a bug, with
// the caller's frames. It does nothing without an installed Process or a
// DSN, and never fails: a report is never worth disturbing the program.
func RecordError(err error) {
	p := Current()
	if p == nil || p.DSN == "" || err == nil {
		return
	}
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	var frames []Frame
	it := runtime.CallersFrames(pcs[:n])
	for {
		f, more := it.Next()
		frames = append(frames, newFrame(f.Function, f.File, f.Line))
		if !more {
			break
		}
	}
	// Reports list the outermost call first; Callers gives the innermost.
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	p.save(ErrorEvent(err, frames, p.Meta, p.now()))
}

// RecordPanic keeps a report of a recovered panic: an HTTP handler's, or
// one pcb's own recover caught (a command, an account's sync loop, a
// daemon goroutine), which never reaches the crash file. stack is
// debug.Stack()'s text taken in the recovering deferred call. The report's
// source is http-panic whichever recovered it (PanicEvent).
func RecordPanic(v any, stack []byte) {
	p := Current()
	if p == nil || p.DSN == "" {
		return
	}
	p.save(PanicEvent(v, endAtPanic(ParseStack(stack)), p.Meta, p.now()))
}

// PanicSite is the function of the last frame of pcb's code in a recovered
// panic's stack (debug.Stack()'s text taken in the recovering deferred
// call), "" when there is none: where a panic happened, without its text.
func PanicSite(stack []byte) string {
	frames := endAtPanic(ParseStack(stack))
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].InApp {
			return frames[i].Function
		}
	}
	return ""
}

// endAtPanic drops the recovery's own frames from a stack taken in a
// deferred call: the runtime's panic frame and every frame after it (the
// deferred function, debug.Stack), so the panicking frame is the last, the
// one a report groups and names by. A stack without a panic frame is kept.
func endAtPanic(frames []Frame) []Frame {
	for i := len(frames) - 1; i >= 0; i-- {
		if f := frames[i].Function; f == "panic" || f == "runtime.gopanic" {
			return frames[:i]
		}
	}
	return frames
}
