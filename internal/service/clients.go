package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
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
	case errors.Is(err, access.ErrSeal):
		msg := "the client's password could not be sealed with the key file " + l.keyFile()
		if cause := keyCause(err); cause != "" {
			msg += " (" + cause + ")"
		}
		return blocked(msg+", so nothing was stored", keyFix)
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
	if err := l.sessionKeyLost(); err != nil {
		return ClientSecret{}, err
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

// defaultClientName is the client a login creates when there is none
// (decision 0013).
const defaultClientName = "default"

// defaultClientTimeout bounds the default client's wait for the clients
// lock: the login it follows is already done.
const defaultClientTimeout = 10 * time.Second

// createDefaultClient adds the client default when the list holds none,
// deciding under the clients lock (two logins completing at once add it
// once). It is best effort: a failure is logged, secret-free, and never
// fails the login. It reports whether it added the client.
func (l *Local) createDefaultClient(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultClientTimeout)
	defer cancel()
	_, _, added, err := l.clients.AddFirst(ctx, defaultClientName)
	switch {
	case err != nil:
		l.log().Warn(`the CalDAV client "default" could not be created: run pcb client add NAME`,
			"error", l.clientsError(err).Error())
	case added:
		l.log().Info(`created the CalDAV client "default": see pcb client show`)
	}
	return added
}

// ClientCredentials is what a CalDAV client needs (decision 0013): the
// URL, the username (the client's name: any username is accepted), the
// password and, over TLS, the certificate's SHA-256 fingerprint. Password
// is empty when not Recoverable (a client made before pcb kept sealed
// copies). The service leaves URL and Fingerprint empty: the caller that
// knows the listener fills them. It redacts itself in fmt and slog; only
// its JSON form carries the password.
type ClientCredentials struct {
	Client
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Recoverable bool   `json:"recoverable"`
}

const redactedCredentials = "service.ClientCredentials(redacted)"

// String hides the password from fmt.
func (ClientCredentials) String() string { return redactedCredentials }

// GoString hides the password from %#v.
func (ClientCredentials) GoString() string { return redactedCredentials }

// LogValue hides the password from slog.
func (ClientCredentials) LogValue() slog.Value { return slog.StringValue(redactedCredentials) }

// pickClient is the client idOrName names; "" names the client default,
// else the only client.
func (l *Local) pickClient(idOrName string) (access.Client, error) {
	if idOrName != "" {
		c, err := l.clients.Get(idOrName)
		return c, l.clientsError(err)
	}
	c, err := l.clients.Get(defaultClientName)
	if !errors.Is(err, access.ErrNotFound) {
		return c, l.clientsError(err)
	}
	list, err := l.clients.List()
	switch {
	case err != nil:
		return access.Client{}, l.clientsError(err)
	case len(list) == 1:
		return list[0], nil
	case len(list) == 0:
		return access.Client{}, mustAct("there is no CalDAV client yet",
			"run pcb login (it creates the client default), or pcb client add NAME")
	}
	return access.Client{}, mustAct("there are several CalDAV clients and none is named default",
		"name a client: pcb client show NAME (pcb client list)")
}

// ShowClient implements API.
func (l *Local) ShowClient(_ context.Context, idOrName string) (ClientCredentials, error) {
	if l.closed.Load() {
		return ClientCredentials{}, errClosed()
	}
	c, err := l.pickClient(idOrName)
	if err != nil {
		return ClientCredentials{}, err
	}
	out := ClientCredentials{Client: clientOf(c), Username: c.Name}
	pw, err := l.clients.Reveal(c.ID, l.o.Sealer.Open)
	switch {
	case err == nil:
		out.Password, out.Recoverable = pw, true
	case errors.Is(err, access.ErrNoSealedCopy):
	case errors.Is(err, access.ErrNotRecoverable):
		return ClientCredentials{}, l.unrevealable(c.Name, err)
	default:
		return ClientCredentials{}, l.clientsError(err)
	}
	return out, nil
}

// unrevealable is the MustAct of a sealed copy the key does not open. A
// key file that exists but cannot be used (unreadable, a loose mode) is
// Blocked with its cause: the key is not lost, so rotate is no way out.
func (l *Local) unrevealable(name string, err error) error {
	if cause := keyCause(err); cause != "" {
		return blocked(fmt.Sprintf("the password of the CalDAV client %q cannot be shown: the key file %s cannot be used (%s)", name, l.keyFile(), cause), keyFix)
	}
	why := "its sealed copy does not open with the key file " + l.keyFile()
	switch {
	case errors.Is(err, access.ErrStale):
		why = "its sealed copy does not match its password (clients.json was edited)"
	case errors.Is(err, seal.ErrNoKey):
		why = "the key file " + l.keyFile() + " is missing"
	}
	hint := "restore the key file from a backup, or run pcb client rotate " + shellquote.Quote(name) + " for a new password"
	if errors.Is(err, seal.ErrNoKey) && l.sessionSealed() {
		// rotate is refused meanwhile (sessionKeyLost); after the new
		// login, show offers it for a copy the new key does not open.
		hint = "restore the key file from a backup, or run pcb logout, then pcb login"
	}
	return mustAct(fmt.Sprintf("the password of the CalDAV client %q cannot be shown: %s", name, why), hint)
}

// sessionSealed reports whether secrets.sealed exists.
func (l *Local) sessionSealed() bool {
	_, err := os.Stat(l.secrets.Path)
	return err == nil
}

// sessionKeyLost refuses to seal a client's password when the key file is
// absent while secrets.sealed exists (ruling M5-R14): the sealer would mint
// a new key, turning the stored session from "key missing, restore it"
// into "corrupt", and the clients sealed meanwhile would not open once the
// old key is restored. With no secrets.sealed a new key is the way back
// (rotate). Only an absent key file counts: any other key error is the
// sealer's to report. A login's own sealing still creates the key.
func (l *Local) sessionKeyLost() error {
	if _, err := os.Stat(l.keyFile()); !errors.Is(err, fs.ErrNotExist) || !l.sessionSealed() {
		return nil
	}
	return mustAct("the key file "+l.keyFile()+" is missing while secrets.sealed exists: a new key would make the stored session unreadable",
		"restore the key file from a backup, or run pcb logout, then pcb login")
}

// RotateClient implements API.
func (l *Local) RotateClient(ctx context.Context, idOrName string) (ClientSecret, error) {
	if l.closed.Load() {
		return ClientSecret{}, errClosed()
	}
	c, err := l.pickClient(idOrName)
	if err != nil {
		return ClientSecret{}, err
	}
	if err := l.sessionKeyLost(); err != nil {
		return ClientSecret{}, err
	}
	c, pw, err := l.clients.Rotate(ctx, c.ID)
	if err != nil {
		return ClientSecret{}, l.clientsError(err)
	}
	return ClientSecret{ID: c.ID, Name: c.Name, Password: pw}, nil
}
