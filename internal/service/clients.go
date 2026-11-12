package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
)

// ClientSecret is a newly created CalDAV client: its password is returned
// this once and never stored. It redacts itself in fmt and slog; only its
// JSON form (the CLI's envelope) carries the password.
type ClientSecret struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

const redactedSecret = "service.ClientSecret(redacted)"

// String hides the password from fmt.
func (ClientSecret) String() string { return redactedSecret }

// GoString hides the password from %#v.
func (ClientSecret) GoString() string { return redactedSecret }

// LogValue hides the password from slog.
func (ClientSecret) LogValue() slog.Value { return slog.StringValue(redactedSecret) }

// Client is a CalDAV client as listed: no secret. LastUsed is nil until its
// first use was recorded (at most one record per minute).
type Client struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

func clientOf(c access.Client) Client {
	out := Client{ID: c.ID, Name: c.Name, Created: c.Created}
	if !c.LastUsed.IsZero() {
		t := c.LastUsed
		out.LastUsed = &t
	}
	return out
}

// clientsError maps a failure of the client store; text of an unknown
// failure is dropped (it may name paths or system details).
func (l *Local) clientsError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, access.ErrInvalidName), errors.Is(err, access.ErrDuplicate):
		return usage(err.Error(), "choose another name: 1 to 64 characters, unique, no control characters, no leading or trailing space")
	case errors.Is(err, access.ErrNotFound):
		return usage(err.Error(), "run pcb client list to see the clients")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return blocked("the operation was cancelled or timed out", "run the command again")
	default:
		return blocked("the CalDAV client list could not be read or written",
			fmt.Sprintf("check clients.json and its permissions in %s (it must be valid JSON in the format pcb wrote, mode 0600)", l.o.Dirs.State))
	}
}

// AddClient implements API.
func (l *Local) AddClient(ctx context.Context, name string) (ClientSecret, error) {
	if l.closed.Load() {
		return ClientSecret{}, errClosed()
	}
	id, pw, err := l.clients.Add(ctx, name)
	if err != nil {
		return ClientSecret{}, l.clientsError(err)
	}
	return ClientSecret{ID: id, Name: name, Password: pw}, nil
}

// Clients implements API.
func (l *Local) Clients(context.Context) ([]Client, error) {
	if l.closed.Load() {
		return nil, errClosed()
	}
	list, err := l.clients.List()
	if err != nil {
		return nil, l.clientsError(err)
	}
	out := make([]Client, len(list))
	for i, c := range list {
		out[i] = clientOf(c)
	}
	return out, nil
}

// RemoveClient implements API.
func (l *Local) RemoveClient(ctx context.Context, idOrName string) (Client, error) {
	if l.closed.Load() {
		return Client{}, errClosed()
	}
	c, err := l.clients.Remove(ctx, idOrName)
	if err != nil {
		return Client{}, l.clientsError(err)
	}
	return clientOf(c), nil
}

// CheckClient implements API. A remote that failed too often is refused
// before its password is looked at, with how long to wait.
func (l *Local) CheckClient(ctx context.Context, password, remote string) (ok bool, retryAfter time.Duration) {
	if l.closed.Load() {
		return false, 0
	}
	if allowed, wait := l.limiter.Allow(remote); !allowed {
		return false, wait
	}
	c, found := l.clients.Check(password)
	if !found {
		l.limiter.Fail(remote)
		return false, 0
	}
	l.touch(c.ID)
	l.noteActivity()
	return true, 0
}

// defaultTouchTimeout bounds a background touch waiting for the clients lock.
const defaultTouchTimeout = 10 * time.Second

// touch notes the client's last use off the request path (ruling M3-R21): a
// CLI holding clients.lock must not stall requests. At most one touch per
// client is in flight, so requests arriving meanwhile start none. A failed
// touch is dropped (a missed LastUsed must not refuse a valid client, and
// there is nothing secret-free worth logging); Close waits for the running
// ones after cancelling them.
func (l *Local) touch(id string) {
	l.tmu.Lock()
	defer l.tmu.Unlock()
	if l.closed.Load() || l.touching[id] {
		return
	}
	l.touching[id] = true
	l.twg.Add(1)
	go func() {
		defer l.twg.Done()
		if l.touchHook != nil {
			l.touchHook("start", id)
		}
		ctx, cancel := context.WithTimeout(l.tctx, l.touchTimeout)
		_ = l.clients.Touch(ctx, id)
		cancel()
		l.tmu.Lock()
		delete(l.touching, id)
		l.tmu.Unlock()
		if l.touchHook != nil {
			l.touchHook("done", id)
		}
	}()
}
