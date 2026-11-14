package protontest

import (
	"fmt"
	"os"
	"sync"
)

// TB is what a fake needs of its holder: testing.TB satisfies it, for tests;
// Process does, for a program (cmd/pcb-fake).
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// Process is the TB of a program that runs a fake outside go test. Fatal
// and Fatalf panic with a *Failure (Recover turns it back into an error);
// Errorf, which a fake calls from its request handlers, reports to Report;
// Close runs the cleanups, the last registered first. Its zero value is
// ready, reporting to stderr.
type Process struct {
	// Report receives each Errorf message; nil: a line on stderr.
	Report func(msg string)

	mu       sync.Mutex
	cleanups []func()
	closed   bool
}

var _ TB = (*Process)(nil)

// Failure is what Fatal and Fatalf of a Process panic with.
type Failure struct{ Msg string }

func (f *Failure) Error() string { return f.Msg }

// Helper does nothing: a program has no test frames to skip.
func (*Process) Helper() {}

// Cleanup registers fn for Close. After Close it runs fn at once.
func (p *Process) Cleanup(fn func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		fn()
		return
	}
	p.cleanups = append(p.cleanups, fn)
	p.mu.Unlock()
}

// Errorf reports a failure that does not stop the program.
func (p *Process) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if p.Report != nil {
		p.Report(msg)
		return
	}
	fmt.Fprintln(os.Stderr, msg)
}

// Fatal panics with a *Failure of its arguments, formatted as fmt.Sprint.
func (*Process) Fatal(args ...any) { panic(&Failure{fmt.Sprint(args...)}) }

// Fatalf panics with a *Failure of its arguments, formatted as fmt.Sprintf.
func (*Process) Fatalf(format string, args ...any) { panic(&Failure{fmt.Sprintf(format, args...)}) }

// Close runs the cleanups registered so far, the last first, once.
func (p *Process) Close() {
	p.mu.Lock()
	fns := p.cleanups
	p.cleanups, p.closed = nil, true
	p.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// Recover, deferred, turns a *Failure panic of a Process into *err; any
// other panic goes on.
func Recover(err *error) {
	switch r := recover().(type) {
	case nil:
	case *Failure:
		*err = r
	default:
		panic(r)
	}
}
