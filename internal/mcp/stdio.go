package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// pcb mcp over stdio (design §5.3): one MCP session on stdin and stdout,
// served by one backend after another. The backend is this process's own
// server (ServeStdio, when pcb mcp is the session daemon) or a daemon's
// /mcp on the control socket (Relay). Messages are copied one by one; the
// agent's handshake is kept and replayed to a new backend, so a daemon that
// ends hands the session over without the agent noticing more than the
// call in flight, which returns retry.

// drainTimeout bounds the wait for the answers still due once stdin ends,
// as pcb serve drains its listener.
const drainTimeout = 5 * time.Second

// replayTimeout bounds the replayed handshake with a new backend.
const replayTimeout = 10 * time.Second

// errBackendGone: the backend stopped answering (its daemon ended).
var errBackendGone = errors.New("the MCP backend went away")

// goneError is pump's end when its backend went away: whether the backend
// answered anything first (a backend that never did may be refusing this
// session rather than gone) and the first failure seen.
type goneError struct {
	answered bool
	cause    error
}

func (e *goneError) Error() string {
	if e.cause == nil {
		return errBackendGone.Error()
	}
	return errBackendGone.Error() + ": " + e.cause.Error()
}

func (e *goneError) Is(target error) bool { return target == errBackendGone }

// Stdio is the agent's side of pcb mcp: the session read from stdin and
// written to stdout, newline-delimited JSON-RPC. stdout carries MCP
// messages only.
type Stdio struct {
	conn sdkmcp.Connection
	wmu  sync.Mutex // writes to conn

	mu      sync.Mutex
	queue   []jsonrpc.Message // read from the agent, not delivered yet
	ended   error             // why stdin ended (io.EOF), once it did
	init    *jsonrpc.Request  // the agent's answered initialize: replayed to a new backend
	inited  bool              // the agent sent notifications/initialized
	version string            // the protocol version initialize negotiated
	replays int
}

// NewStdio reads the agent's messages from in and writes to out.
func NewStdio(in io.Reader, out io.Writer) *Stdio {
	conn, _ := (&sdkmcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}}).Connect(context.Background())
	return &Stdio{conn: conn}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// RunStdio serves one MCP session on in and out with the tools g allows
// (entry stdio), until in ends (nil, after up to 5 s for the answers still
// due) or ctx does.
func (s *Server) RunStdio(ctx context.Context, g Grant, in io.Reader, out io.Writer) error {
	return s.ServeStdio(ctx, NewStdio(in, out), g)
}

// ServeStdio serves st in process with the tools g allows (entry stdio),
// as RunStdio does. A session that another backend served so far goes on
// here: its handshake is replayed first.
func (s *Server) ServeStdio(ctx context.Context, st *Stdio, g Grant) error {
	serverSide, agentSide := sdkmcp.NewInMemoryTransports()
	ss, err := s.Session(g, EntryStdio).Connect(ctx, serverSide, nil)
	if err != nil {
		return err
	}
	defer func() { _ = ss.Close() }()
	b, err := agentSide.Connect(ctx)
	if err != nil {
		return err
	}
	if err := st.pump(ctx, b); errors.Is(err, errBackendGone) {
		return errors.New("the in-process MCP server stopped answering")
	} else if err != nil {
		return err
	}
	return nil
}

// negotiated is the protocol version the session's initialize settled,
// "" before one (or with the newest protocol, which has none).
func (st *Stdio) negotiated() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.version
}

// read is the agent's next message: a queued one first. Once stdin ended,
// it keeps returning why.
func (st *Stdio) read(ctx context.Context) (jsonrpc.Message, error) {
	st.mu.Lock()
	if len(st.queue) > 0 {
		m := st.queue[0]
		st.queue = st.queue[1:]
		st.mu.Unlock()
		return m, nil
	}
	if st.ended != nil {
		err := st.ended
		st.mu.Unlock()
		return nil, err
	}
	st.mu.Unlock()
	m, err := st.conn.Read(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st.mu.Lock()
		st.ended = err
		st.mu.Unlock()
	}
	return m, err
}

// await waits for the agent's next message and queues it: nil once one is
// queued, else why stdin ended or ctx's error.
func (st *Stdio) await(ctx context.Context) error {
	m, err := st.read(ctx)
	if err != nil {
		return err
	}
	st.requeue(m)
	return nil
}

// requeue puts messages back at the head of the queue, in order.
func (st *Stdio) requeue(ms ...jsonrpc.Message) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.queue = append(slices.Clone(ms), st.queue...)
}

// write sends m to the agent.
func (st *Stdio) write(m jsonrpc.Message) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_ = st.conn.Write(context.Background(), m) // stdout gone: the agent left, stdin ends next
}

// pass is one backend's share of the session: the agent's calls it has
// not answered yet, in the order sent, and the calls it made to the agent.
type pass struct {
	mu       sync.Mutex
	inflight []*jsonrpc.Request
	asked    map[jsonrpc.ID]bool // the backend's calls to the agent, unanswered
	drained  chan struct{}       // closed when inflight empties after stdin ended
	draining bool
	progress bool  // the backend answered something
	cause    error // the first sign that the backend is gone
}

func (p *pass) gotAnswer() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progress = true
}

// fail records why the backend is gone, the first time.
func (p *pass) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cause == nil {
		p.cause = err
	}
}

func (p *pass) gone() *goneError {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &goneError{answered: p.progress, cause: p.cause}
}

func (p *pass) add(r *jsonrpc.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight = append(p.inflight, r)
}

// done removes the call id answers and returns it (nil: not one of ours).
func (p *pass) done(id jsonrpc.ID) *jsonrpc.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := slices.IndexFunc(p.inflight, func(r *jsonrpc.Request) bool { return r.ID == id })
	if i < 0 {
		return nil
	}
	r := p.inflight[i]
	p.inflight = slices.Delete(p.inflight, i, i+1)
	if p.draining && len(p.inflight) == 0 {
		close(p.drained)
		p.draining = false
	}
	return r
}

// drain returns a channel closed once no call is left unanswered.
func (p *pass) drain() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drained = make(chan struct{})
	if len(p.inflight) == 0 {
		close(p.drained)
	} else {
		p.draining = true
	}
	return p.drained
}

func (p *pass) take() []*jsonrpc.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.inflight
	p.inflight = nil
	return out
}

func (p *pass) ask(id jsonrpc.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked[id] = true
}

// answered reports whether id is a call of this backend's to the agent,
// and forgets it.
func (p *pass) answered(id jsonrpc.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ok := p.asked[id]
	delete(p.asked, id)
	return ok
}

// pump copies the session between the agent and b until stdin ends (nil,
// after the drain), ctx ends (its error) or b goes away (a *goneError,
// which is errBackendGone: the calls b left unanswered are answered retry
// when they are tool calls, or queued again for the next backend, since
// every other MCP request may be sent twice). It closes b.
func (st *Stdio) pump(ctx context.Context, b sdkmcp.Connection) error {
	defer func() { _ = b.Close() }()
	p := &pass{asked: map[jsonrpc.ID]bool{}}
	if replayed, err := st.replay(ctx, b); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &goneError{cause: err}
	} else if replayed {
		p.gotAnswer()
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gone := make(chan struct{})
	var goneOnce sync.Once
	markGone := func() {
		goneOnce.Do(func() { close(gone) })
		cancel()
	}
	down := make(chan struct{})
	go func() {
		defer close(down)
		for {
			m, err := b.Read(pctx)
			if err != nil {
				if pctx.Err() == nil {
					p.fail(err)
				}
				markGone()
				return
			}
			if !st.fromBackend(p, m) {
				markGone()
				return
			}
		}
	}()

	var result error
	for {
		m, err := st.read(pctx)
		if err != nil {
			select {
			case <-gone:
				result = errBackendGone
			default:
				if ctx.Err() != nil {
					result = ctx.Err()
					break
				}
				// stdin ended: the answers still due get drainTimeout.
				timer := time.NewTimer(drainTimeout)
				select {
				case <-p.drain():
				case <-gone:
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
			}
			break
		}
		if !st.toBackend(pctx, p, b, m) {
			markGone()
			result = errBackendGone
			break
		}
	}
	cancel()
	<-down
	if errors.Is(result, errBackendGone) {
		result = p.gone()
		var again []jsonrpc.Message
		for _, r := range p.take() {
			if r.Method == "tools/call" {
				st.write(retryResult(r.ID))
				continue
			}
			again = append(again, r)
		}
		st.requeue(again...)
	}
	return result
}

// fromBackend handles a message b sent: false when it shows b is gone (a
// call it ended without an answer: the SDK's own error, not the server's).
func (st *Stdio) fromBackend(p *pass, m jsonrpc.Message) bool {
	switch m := m.(type) {
	case *jsonrpc.Response:
		var wire *jsonrpc.Error
		if m.Error != nil && !errors.As(m.Error, &wire) {
			p.fail(m.Error) // "request terminated without response": the stream broke
			return false
		}
		p.gotAnswer()
		if r := p.done(m.ID); r != nil && r.Method == "initialize" && m.Error == nil {
			st.settle(r, m.Result)
		}
	case *jsonrpc.Request:
		if m.IsCall() {
			p.ask(m.ID)
		}
	}
	st.write(m)
	return true
}

// toBackend sends the agent's m to b: false when b is gone. A message b
// never got is queued again for the next backend; a call b refused gets
// its error.
func (st *Stdio) toBackend(ctx context.Context, p *pass, b sdkmcp.Connection, m jsonrpc.Message) bool {
	switch m := m.(type) {
	case *jsonrpc.Request:
		if m.Method == "notifications/initialized" {
			st.mu.Lock()
			st.inited = true
			st.mu.Unlock()
		}
		if m.IsCall() {
			p.add(m)
		}
		err := b.Write(ctx, m)
		if err == nil {
			return true
		}
		switch wire := serverError(err); {
		case wire != nil && m.IsCall():
			p.done(m.ID)
			st.write(&jsonrpc.Response{ID: m.ID, Error: wire})
			return true
		case undelivered(err):
			if m.IsCall() {
				p.done(m.ID)
			}
			st.requeue(m)
		}
		p.fail(err)
		return false // delivered or not, b is gone: a call stays in flight
	case *jsonrpc.Response:
		if !p.answered(m.ID) {
			return true // the answer to a backend that is gone
		}
		if err := b.Write(ctx, m); err != nil && serverError(err) == nil {
			p.fail(err)
			return false
		}
	}
	return true
}

// codeRejected is the SDK transport's own "rejected by transport" error,
// which wraps every failed write, whatever the daemon said.
const codeRejected = -32005

// serverError is the JSON-RPC error the daemon answered a write with (a
// refused request), nil when err holds none. The SDK wraps the daemon's
// error before its own "rejected by transport" (refused request), and its
// own first when the request never got an answer (dial, broken
// connection): the first JSON-RPC error in err tells which.
func serverError(err error) *jsonrpc.Error {
	var w *jsonrpc.Error
	if errors.As(err, &w) && w.Code != codeRejected {
		return w
	}
	return nil
}

// settle records the agent's answered initialize and the version it
// settled, for a replay and for the relay's headers.
func (st *Stdio) settle(init *jsonrpc.Request, result json.RawMessage) {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(result, &res)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.init = init
	st.version = res.ProtocolVersion
}

// replay sends a new backend the session's handshake, when the agent made
// one with an earlier backend (replayed: true): its initialize under an ID
// of ours (the answer is dropped), then notifications/initialized if the
// agent sent it.
func (st *Stdio) replay(ctx context.Context, b sdkmcp.Connection) (replayed bool, err error) {
	st.mu.Lock()
	init, inited := st.init, st.inited
	st.replays++
	n := st.replays
	st.mu.Unlock()
	if init == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()
	id, _ := jsonrpc.MakeID(fmt.Sprintf("pcb-replay-%d", n))
	if err := b.Write(ctx, &jsonrpc.Request{ID: id, Method: "initialize", Params: init.Params}); err != nil {
		return false, err
	}
	for {
		m, err := b.Read(ctx)
		if err != nil {
			return false, err
		}
		if r, ok := m.(*jsonrpc.Response); ok && r.ID == id {
			if r.Error != nil {
				return false, r.Error
			}
			break
		}
	}
	if inited {
		if err := b.Write(ctx, &jsonrpc.Request{Method: "notifications/initialized", Params: json.RawMessage(`{}`)}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// retryResult is the answer to a tool call whose daemon ended before
// answering: a retry tool error.
func retryResult(id jsonrpc.ID) *jsonrpc.Response {
	te := &toolError{Class: classRetry,
		Message: "the pcb daemon ended before answering: the call may or may not have taken effect",
		Hint:    "call it again (pcb mcp reconnects); after a write, read the event first (event_get or events_list)"}
	res, _ := json.Marshal(&sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: te.Error()}}, IsError: true})
	return &jsonrpc.Response{ID: id, Result: res}
}
