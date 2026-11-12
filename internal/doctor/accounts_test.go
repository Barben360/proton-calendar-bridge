package doctor_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// saveState writes dir's state.json naming address.
func saveState(t *testing.T, dir, address string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveState(dir, store.State{Phase: store.PhaseLoggedOut, Address: address}); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshot is every path under root with its mode, size, modification
// time and content.
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		line := p + " " + fi.Mode().String() + " " + fi.ModTime().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			line += " " + string(b)
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Design §4.2: doctor reads an M5 state (its one account at the root) and
// writes nothing: no migration.
func TestAccountsOfAnM5StateLeaveItAsItIs(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "state")
	saveState(t, root, "")
	writeFile(t, filepath.Join(root, "secrets.sealed"), "sealed")
	before := snapshot(t, root)
	layout, err := doctor.Accounts(root)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if !slices.Equal(layout.Accounts, []doctor.Account{{Dir: root, Label: ""}}) || layout.Interrupted != "" {
		t.Errorf("layout = %+v, want the root as the one account, no label", layout)
	}
	if after := snapshot(t, root); !slices.Equal(before, after) {
		t.Errorf("Accounts changed the state:\nbefore %q\nafter  %q", before, after)
	}
	// An M5 state that names its address (an M6 login wrote it) is
	// labelled by it.
	saveState(t, root, "a@x")
	if layout, _ := doctor.Accounts(root); len(layout.Accounts) != 1 || layout.Accounts[0].Label != "a@x" {
		t.Errorf("layout = %+v, want a@x", layout)
	}
}

func TestAccountsOfAStateWithNothing(t *testing.T) {
	t.Parallel()
	for _, root := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		layout, err := doctor.Accounts(root)
		if err != nil || len(layout.Accounts) != 0 || layout.Interrupted != "" {
			t.Errorf("%s: layout %+v, err %v; want nothing", root, layout, err)
		}
	}
}

// v2: one account per directory, named by its address, else its local ID.
func TestAccountsOfTheV2Layout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "layout.json"), `{"version": 2}`)
	saveState(t, store.AccountDir(root, "aaaaaaaa"), "a@x")
	saveState(t, store.AccountDir(root, "bbbbbbbb"), "")
	before := snapshot(t, root)
	layout, err := doctor.Accounts(root)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	want := []doctor.Account{{Dir: store.AccountDir(root, "aaaaaaaa"), Label: "a@x"}, {Dir: store.AccountDir(root, "bbbbbbbb"), Label: "bbbbbbbb"}}
	if !slices.Equal(layout.Accounts, want) || layout.Interrupted != "" {
		t.Errorf("layout = %+v, want %+v", layout, want)
	}
	if after := snapshot(t, root); !slices.Equal(before, after) {
		t.Errorf("Accounts changed the state")
	}
}

// Files of an older pcb beside layout.json: the accounts are listed, with
// an error naming the files once and the fix.
func TestAccountsWithLegacyFilesBesideTheLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "layout.json"), `{"version": 2}`)
	saveState(t, store.AccountDir(root, "aaaaaaaa"), "a@x")
	saveState(t, root, "")
	layout, err := doctor.Accounts(root)
	var lerr *doctor.LayoutError
	if !errors.As(err, &lerr) || !errors.Is(err, store.ErrLegacyBesideLayout) || lerr.Fix != "remove them or move the state directory aside: no downgrade after M6" {
		t.Fatalf("err = %v, want the legacy refusal with its fix", err)
	}
	if n := strings.Count(err.Error(), "state.json"); n != 1 {
		t.Errorf("message %q names state.json %d times, want once", err, n)
	}
	if len(layout.Accounts) != 1 || layout.Accounts[0].Label != "a@x" {
		t.Errorf("layout = %+v, want a's account", layout)
	}
}

func TestAccountsOfAnUnreadableLayout(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, layout, fix string
		is                error // the cause, when one is defined
	}{
		{"malformed", `{"version":`, "check ", nil},
		{"newer", `{"version": 3}`, "upgrade pcb", store.ErrNewerLayout},
	} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "layout.json"), c.layout)
		saveState(t, store.AccountDir(root, "aaaaaaaa"), "a@x")
		layout, err := doctor.Accounts(root)
		var lerr *doctor.LayoutError
		if !errors.As(err, &lerr) || !strings.HasPrefix(lerr.Fix, c.fix) || len(layout.Accounts) != 0 || (c.is != nil && !errors.Is(err, c.is)) {
			t.Errorf("%s: layout %+v, err %v; want no account and a fix starting %q", c.name, layout, err, c.fix)
		}
	}
}

// A move to the per-account layout that stopped midway is named, its
// accounts not listed (the next command finishes it), and left as it is.
func TestAccountsOfAnInterruptedMigration(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, dir string
	}{
		{"staged", "aaaaaaaa.migrating"}, // files staged, not renamed yet
		{"renamed", "bbbbbbbb"},          // renamed, layout.json not written yet
	} {
		root := t.TempDir()
		dir := filepath.Join(store.AccountsDir(root), c.dir)
		saveState(t, dir, "a@x")
		before := snapshot(t, root)
		layout, err := doctor.Accounts(root)
		if err != nil || layout.Interrupted != dir || len(layout.Accounts) != 0 {
			t.Errorf("%s: layout %+v, err %v; want %s interrupted, no account", c.name, layout, err, dir)
		}
		if after := snapshot(t, root); !slices.Equal(before, after) {
			t.Errorf("%s: Accounts changed the state", c.name)
		}
	}
}
