package seal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// The freedesktop Secret Service API, written against its public
// specification (https://specifications.freedesktop.org/secret-service/).
const (
	ssBus        = "org.freedesktop.secrets"
	ssPath       = dbus.ObjectPath("/org/freedesktop/secrets")
	ssDefaultCol = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
	ssService    = "org.freedesktop.Secret.Service"
	ssCollection = "org.freedesktop.Secret.Collection"
	ssSession    = "org.freedesktop.Secret.Session"
	ssItem       = "org.freedesktop.Secret.Item"
	ssItemLabel  = "org.freedesktop.Secret.Item.Label"
	ssItemAttrs  = "org.freedesktop.Secret.Item.Attributes"
	ssNoPrompt   = dbus.ObjectPath("/")
	ssIsLocked   = "org.freedesktop.Secret.Error.IsLocked"
	ssNoObject   = "org.freedesktop.Secret.Error.NoSuchObject"

	secretContentType = "application/octet-stream"
	// dbusTimeout bounds one keyring call, connection included: a service
	// that does not answer reads as absent.
	dbusTimeout = 2 * time.Second
)

// secret is the Secret Service "secret" struct (oayays).
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// DBusKeyring returns the Keyring of the Secret Service on the D-Bus bus at
// addr; "" is the session bus (DBUS_SESSION_BUS_ADDRESS). It connects on
// each call, for at most 2 s, and never prompts: a locked collection is
// ErrLocked. Any failure to reach a service wraps ErrNoService.
func DBusKeyring(addr string) Keyring { return dbusKeyring{addr: addr} }

type dbusKeyring struct{ addr string }

// conn dials the bus; a hung daemon cannot outlive ctx.
func (k dbusKeyring) conn(ctx context.Context) (*dbus.Conn, error) {
	addr := k.addr
	if addr == "" {
		addr = os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	}
	if addr == "" {
		return nil, fmt.Errorf("%w: DBUS_SESSION_BUS_ADDRESS is not set", ErrNoService)
	}
	type result struct {
		c   *dbus.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dbus.Connect(addr, dbus.WithContext(ctx))
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoService, r.err)
		}
		return r.c, nil
	case <-ctx.Done():
		go func() { // close a late connection
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, fmt.Errorf("%w: the session bus did not answer", ErrNoService)
	}
}

// classify maps a D-Bus failure to the package's errors.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var de dbus.Error
	if errors.As(err, &de) {
		switch de.Name {
		case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
			return fmt.Errorf("%w: nothing owns %s", ErrNoService, ssBus)
		case ssIsLocked:
			return ErrLocked
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: the Secret Service did not answer", ErrNoService)
	}
	return fmt.Errorf("secret service: %w", err)
}

// run connects and runs fn under the 2 s bound.
func (k dbusKeyring) run(ctx context.Context, fn func(ctx context.Context, c *dbus.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, dbusTimeout)
	defer cancel()
	c, err := k.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck // nothing to recover
	return bounded(ctx, fn(ctx, c))
}

// bounded maps a call's failure once the bound has expired: godbus closes
// the connection at the deadline, so a hung call may fail with "use of
// closed network connection" rather than the deadline itself. Either way
// the service did not answer in time. A locked keyring stays ErrLocked.
func bounded(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || errors.Is(err, ErrNoService) || errors.Is(err, ErrLocked) {
		return err
	}
	return fmt.Errorf("%w: the Secret Service did not answer", ErrNoService)
}

func locked(ctx context.Context, c *dbus.Conn) (bool, error) {
	var v dbus.Variant
	call := c.Object(ssBus, ssDefaultCol).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, ssCollection, "Locked")
	if err := call.Store(&v); err != nil {
		return false, classify(err)
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("secret service: Locked is %T, not a boolean", v.Value())
	}
	return b, nil
}

// Locked implements Keyring.
func (k dbusKeyring) Locked(ctx context.Context) (l bool, err error) {
	err = k.run(ctx, func(ctx context.Context, c *dbus.Conn) error {
		l, err = locked(ctx, c)
		return err
	})
	return l, err
}

// session opens a plain session (no transport encryption: the bus is the
// user's private one) and returns it with its closer.
func session(ctx context.Context, c *dbus.Conn) (dbus.ObjectPath, func(), error) {
	var output dbus.Variant
	var path dbus.ObjectPath
	err := c.Object(ssBus, ssPath).CallWithContext(ctx, ssService+".OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &path)
	if err != nil {
		return "", nil, classify(err)
	}
	return path, func() {
		_ = c.Object(ssBus, path).CallWithContext(ctx, ssSession+".Close", 0).Err
	}, nil
}

// Get implements Keyring.
func (k dbusKeyring) Get(ctx context.Context, attrs map[string]string) (out []byte, err error) {
	err = k.run(ctx, func(ctx context.Context, c *dbus.Conn) error {
		l, err := locked(ctx, c)
		if err != nil {
			return err
		}
		if l {
			return ErrLocked
		}
		var items []dbus.ObjectPath
		call := c.Object(ssBus, ssDefaultCol).CallWithContext(ctx, ssCollection+".SearchItems", 0, attrs)
		if err := call.Store(&items); err != nil {
			return classify(err)
		}
		if len(items) == 0 {
			return ErrNoKey
		}
		sess, closeSession, err := session(ctx, c)
		if err != nil {
			return err
		}
		defer closeSession()
		var secrets map[dbus.ObjectPath]secret
		call = c.Object(ssBus, ssPath).CallWithContext(ctx, ssService+".GetSecrets", 0, items[:1], sess)
		if err := call.Store(&secrets); err != nil {
			var de dbus.Error
			if errors.As(err, &de) && de.Name == ssNoObject {
				return ErrNoKey
			}
			return classify(err)
		}
		s, ok := secrets[items[0]]
		if !ok {
			return ErrNoKey
		}
		out = s.Value
		return nil
	})
	return out, err
}

// Set implements Keyring.
func (k dbusKeyring) Set(ctx context.Context, label string, attrs map[string]string, value []byte) error {
	return k.run(ctx, func(ctx context.Context, c *dbus.Conn) error {
		l, err := locked(ctx, c)
		if err != nil {
			return err
		}
		if l {
			return ErrLocked
		}
		sess, closeSession, err := session(ctx, c)
		if err != nil {
			return err
		}
		defer closeSession()
		props := map[string]dbus.Variant{
			ssItemLabel: dbus.MakeVariant(label),
			ssItemAttrs: dbus.MakeVariant(attrs),
		}
		sec := secret{Session: sess, Parameters: []byte{}, Value: value, ContentType: secretContentType}
		var item, prompt dbus.ObjectPath
		call := c.Object(ssBus, ssDefaultCol).CallWithContext(ctx, ssCollection+".CreateItem", 0, props, sec, true)
		if err := call.Store(&item, &prompt); err != nil {
			return classify(err)
		}
		if prompt != ssNoPrompt {
			// The service wants the user to answer something: pcb does not
			// drive prompts, which would hang a headless process.
			return ErrLocked
		}
		return nil
	})
}

// Delete implements Keyring: Item.Delete on every item SearchItems finds.
func (k dbusKeyring) Delete(ctx context.Context, attrs map[string]string) error {
	return k.run(ctx, func(ctx context.Context, c *dbus.Conn) error {
		l, err := locked(ctx, c)
		if err != nil {
			return err
		}
		if l {
			return ErrLocked
		}
		var items []dbus.ObjectPath
		if err := c.Object(ssBus, ssDefaultCol).CallWithContext(ctx, ssCollection+".SearchItems", 0, attrs).Store(&items); err != nil {
			return classify(err)
		}
		for _, item := range items {
			var prompt dbus.ObjectPath
			if err := c.Object(ssBus, item).CallWithContext(ctx, ssItem+".Delete", 0).Store(&prompt); err != nil {
				var de dbus.Error
				if errors.As(err, &de) && de.Name == ssNoObject {
					continue // gone meanwhile
				}
				return classify(err)
			}
			if prompt != ssNoPrompt {
				return ErrLocked // pcb drives no prompt (see Set)
			}
		}
		return nil
	})
}
