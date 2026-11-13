package seal_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/dbustest"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal/sstest"
)

func TestDBusKeyringStoresOneItemPerStateDir(t *testing.T) {
	addr, fake := sstest.Start(t)
	k := seal.DBusKeyring(addr)
	dirA, dirB := t.TempDir(), t.TempDir()
	a := &seal.SecretService{Keyring: k, StateDir: dirA}
	b := &seal.SecretService{Keyring: k, StateDir: dirB}
	sa, err := a.Seal([]byte("secret a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Seal([]byte("secret b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Seal([]byte("again"), nil); err != nil {
		t.Fatal(err)
	}
	items := fake.Items()
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (one per state dir, none duplicated)", len(items))
	}
	for _, it := range items {
		if it.Label != "pcb sealing key" || it.ContentType != "application/octet-stream" || len(it.Secret) != 32 {
			t.Errorf("item = label %q, type %q, %d secret bytes", it.Label, it.ContentType, len(it.Secret))
		}
		if len(it.Attributes) != 2 || it.Attributes["application"] != "pcb" {
			t.Errorf("attributes = %v", it.Attributes)
		}
	}
	states := []string{items[0].Attributes["state"], items[1].Attributes["state"]}
	slices.Sort(states)
	want := []string{dirA, dirB}
	slices.Sort(want)
	if !slices.Equal(states, want) {
		t.Errorf("state attributes = %v, want %v", states, want)
	}
	if got, err := a.Open(sa, nil); err != nil || string(got) != "secret a" {
		t.Errorf("a: %q, %v", got, err)
	}
	if got, err := b.Open(sb, nil); err != nil || string(got) != "secret b" {
		t.Errorf("b: %q, %v", got, err)
	}
	if _, err := a.Open(sb, nil); !errors.Is(err, seal.ErrCorrupt) {
		t.Errorf("a opening b's blob: err = %v, want ErrCorrupt", err)
	}
	if !slices.Contains(fake.Calls(), "OpenSession:plain") {
		t.Errorf("calls = %v, want a plain session", fake.Calls())
	}
}

func TestDBusKeyringMissingItemIsErrNoKey(t *testing.T) {
	addr, _ := sstest.Start(t)
	s := &seal.SecretService{Keyring: seal.DBusKeyring(addr), StateDir: t.TempDir()}
	if _, err := s.Open(make([]byte, 64), nil); !errors.Is(err, seal.ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

func TestDBusKeyringLockedNeverPrompts(t *testing.T) {
	addr, fake := sstest.Start(t)
	k := seal.DBusKeyring(addr)
	s := &seal.SecretService{Keyring: k, StateDir: t.TempDir()}
	sealed, err := s.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fake.SetLocked(true)
	ctx := context.Background()
	attrs := map[string]string{"application": "pcb", "state": s.StateDir}
	if locked, err := k.Locked(ctx); err != nil || !locked {
		t.Fatalf("Locked = %v, %v", locked, err)
	}
	if _, err := k.Get(ctx, attrs); !errors.Is(err, seal.ErrLocked) {
		t.Fatalf("Get err = %v, want ErrLocked", err)
	}
	if err := k.Set(ctx, "l", attrs, make([]byte, 32)); !errors.Is(err, seal.ErrLocked) {
		t.Fatalf("Set err = %v, want ErrLocked", err)
	}
	if _, err := s.Open(sealed, nil); !errors.Is(err, seal.ErrLocked) {
		t.Fatalf("Open err = %v, want ErrLocked", err)
	}
	if err := seal.Probe(ctx, k); !errors.Is(err, seal.ErrLocked) {
		t.Fatalf("Probe err = %v, want ErrLocked", err)
	}
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "Unlock") || strings.HasPrefix(c, "Prompt") {
			t.Fatalf("a prompt reached the service: %v", fake.Calls())
		}
	}
	// Unlocked again, the same item opens: nothing was replaced meanwhile.
	fake.SetLocked(false)
	if got, err := s.Open(sealed, nil); err != nil || string(got) != "x" {
		t.Fatalf("after unlock: %q, %v", got, err)
	}
}

func TestProbeSucceedsOnAnUnlockedService(t *testing.T) {
	addr, _ := sstest.Start(t)
	if err := seal.Probe(context.Background(), seal.DBusKeyring(addr)); err != nil {
		t.Fatal(err)
	}
}

func TestProbeFailsWithoutService(t *testing.T) {
	addr := dbustest.Start(t) // a bus with nobody on org.freedesktop.secrets
	start := time.Now()
	err := seal.Probe(context.Background(), seal.DBusKeyring(addr))
	if !errors.Is(err, seal.ErrNoService) {
		t.Fatalf("err = %v, want ErrNoService", err)
	}
	if errors.Is(err, seal.ErrLocked) {
		t.Fatal("no service must not read as locked")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v, want under 3 s", d)
	}
	err = seal.Probe(context.Background(), seal.DBusKeyring("unix:path="+t.TempDir()+"/nothing"))
	if !errors.Is(err, seal.ErrNoService) {
		t.Fatalf("dead address: err = %v, want ErrNoService", err)
	}
}

// replace=true: a second Set on the same attributes replaces the item.
func TestDBusKeyringSetReplacesTheSameAttributes(t *testing.T) {
	addr, fake := sstest.Start(t)
	k := seal.DBusKeyring(addr)
	ctx := context.Background()
	attrs := map[string]string{"application": "pcb", "state": "/x"}
	if err := k.Set(ctx, "l", attrs, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := k.Set(ctx, "l", attrs, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Items()); n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
	if got, err := k.Get(ctx, attrs); err != nil || string(got) != "second" {
		t.Fatalf("Get = %q, %v, want the second secret", got, err)
	}
}

// A service that stops answering reads as absent within the 2 s bound.
func TestDBusKeyringHungServiceIsErrNoServiceWithinTheBound(t *testing.T) {
	addr, fake := sstest.Start(t)
	fake.SetHang(true)
	start := time.Now()
	err := seal.Probe(context.Background(), seal.DBusKeyring(addr))
	d := time.Since(start)
	if !errors.Is(err, seal.ErrNoService) {
		t.Fatalf("err = %v, want ErrNoService", err)
	}
	if d < 1500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("took %v, want about 2 s", d)
	}
}
