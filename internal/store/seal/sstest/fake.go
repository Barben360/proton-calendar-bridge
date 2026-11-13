// Package sstest is a fake freedesktop Secret Service on a private bus, for
// tests of the keyring sealer. It speaks the plain session only and records
// every call, so a test can assert that nothing prompted.
package sstest

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/Barben360/proton-calendar-bridge/internal/dbustest"
)

const (
	busName    = "org.freedesktop.secrets"
	servicePth = dbus.ObjectPath("/org/freedesktop/secrets")
	collPath   = dbus.ObjectPath("/org/freedesktop/secrets/collection/login")
	aliasPath  = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
)

// Item is one stored secret.
type Item struct {
	Label       string
	Attributes  map[string]string
	Secret      []byte
	ContentType string
}

type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// Fake is the service's state.
type Fake struct {
	mu       sync.Mutex
	locked   bool
	items    []Item
	calls    []string
	sessions int
}

// SetLocked locks or unlocks the default collection.
func (f *Fake) SetLocked(l bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locked = l
}

// Items returns a copy of the stored items.
func (f *Fake) Items() []Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Item, len(f.items))
	for i, it := range f.items {
		it.Secret = slices.Clone(it.Secret)
		out[i] = it
	}
	return out
}

// Calls returns the methods called so far, in order ("OpenSession:plain",
// "SearchItems", "GetSecrets", "CreateItem", "Unlock", "Prompt"...).
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *Fake) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *Fake) itemPath(i int) dbus.ObjectPath {
	return dbus.ObjectPath(fmt.Sprintf("%s/item%d", collPath, i))
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func dbusErr(name, msg string) *dbus.Error {
	return &dbus.Error{Name: name, Body: []any{msg}}
}

// service is org.freedesktop.Secret.Service.
type service struct{ f *Fake }

func (s service) OpenSession(algorithm string, _ dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
	s.f.record("OpenSession:" + algorithm)
	if algorithm != "plain" {
		return dbus.Variant{}, "/", dbusErr("org.freedesktop.DBus.Error.NotSupported", "plain only")
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.sessions++
	return dbus.MakeVariant(""), dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/secrets/session/s%d", s.f.sessions)), nil
}

func (s service) SearchItems(attrs map[string]string) ([]dbus.ObjectPath, []dbus.ObjectPath, *dbus.Error) {
	s.f.record("SearchItems")
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	found := []dbus.ObjectPath{}
	for i, it := range s.f.items {
		if matches(it.Attributes, attrs) {
			found = append(found, s.f.itemPath(i))
		}
	}
	if s.f.locked {
		return []dbus.ObjectPath{}, found, nil
	}
	return found, []dbus.ObjectPath{}, nil
}

func (s service) GetSecrets(items []dbus.ObjectPath, session dbus.ObjectPath) (map[dbus.ObjectPath]secret, *dbus.Error) {
	s.f.record("GetSecrets")
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.locked {
		return nil, dbusErr("org.freedesktop.Secret.Error.IsLocked", "locked")
	}
	out := map[dbus.ObjectPath]secret{}
	for _, p := range items {
		var i int
		if _, err := fmt.Sscanf(string(p), string(collPath)+"/item%d", &i); err != nil || i < 0 || i >= len(s.f.items) {
			return nil, dbusErr("org.freedesktop.Secret.Error.NoSuchObject", "no such item")
		}
		it := s.f.items[i]
		out[p] = secret{Session: session, Parameters: []byte{}, Value: slices.Clone(it.Secret), ContentType: it.ContentType}
	}
	return out, nil
}

func (s service) Unlock(objects []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	s.f.record("Unlock")
	return objects, "/", nil
}

// collection is org.freedesktop.Secret.Collection.
type collection struct{ f *Fake }

func (c collection) SearchItems(attrs map[string]string) ([]dbus.ObjectPath, *dbus.Error) {
	c.f.record("SearchItems")
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	found := []dbus.ObjectPath{}
	for i, it := range c.f.items {
		if matches(it.Attributes, attrs) {
			found = append(found, c.f.itemPath(i))
		}
	}
	return found, nil
}

func (c collection) CreateItem(props map[string]dbus.Variant, sec secret, replace bool) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	c.f.record("CreateItem")
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if c.f.locked {
		return "/", "/prompt/u1", nil // a real service prompts to unlock
	}
	label, _ := props["org.freedesktop.Secret.Item.Label"].Value().(string)
	attrs, _ := props["org.freedesktop.Secret.Item.Attributes"].Value().(map[string]string)
	it := Item{Label: label, Attributes: attrs, Secret: slices.Clone(sec.Value), ContentType: sec.ContentType}
	if replace {
		for i, old := range c.f.items {
			if maps(old.Attributes, attrs) {
				c.f.items[i] = it
				return c.f.itemPath(i), "/", nil
			}
		}
	}
	c.f.items = append(c.f.items, it)
	return c.f.itemPath(len(c.f.items) - 1), "/", nil
}

func maps(a, b map[string]string) bool { return len(a) == len(b) && matches(a, b) }

// props serves org.freedesktop.DBus.Properties for the collection.
type props struct{ f *Fake }

func (p props) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.f.record("Properties.Get:" + name)
	if iface != "org.freedesktop.Secret.Collection" || name != "Locked" {
		return dbus.Variant{}, dbusErr("org.freedesktop.DBus.Error.UnknownProperty", name)
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	return dbus.MakeVariant(p.f.locked), nil
}

// prompt is org.freedesktop.Secret.Prompt, exported only to record calls.
type prompt struct{ f *Fake }

func (p prompt) Prompt(string) *dbus.Error { p.f.record("Prompt"); return nil }

// session answers Close on every session object.
type session struct{ f *Fake }

func (s session) Close() *dbus.Error { s.f.record("Close"); return nil }

// Start serves a fake Secret Service, unlocked and empty, on a private bus
// (dbustest.Start) and returns the bus address.
func Start(t testing.TB) (string, *Fake) {
	t.Helper()
	addr := dbustest.Start(t)
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f := &Fake{}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(conn.Export(service{f}, servicePth, "org.freedesktop.Secret.Service"))
	for _, p := range []dbus.ObjectPath{collPath, aliasPath} {
		must(conn.Export(collection{f}, p, "org.freedesktop.Secret.Collection"))
		must(conn.Export(props{f}, p, "org.freedesktop.DBus.Properties"))
	}
	must(conn.ExportSubtree(session{f}, "/org/freedesktop/secrets/session", "org.freedesktop.Secret.Session"))
	must(conn.Export(prompt{f}, "/prompt/u1", "org.freedesktop.Secret.Prompt"))
	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("cannot own %s: %v, %v", busName, reply, err)
	}
	return addr, f
}
