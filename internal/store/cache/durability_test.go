package cache

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// The cache commits with synchronous=FULL (state-I5): in WAL mode NORMAL
// does not fsync at commit, so a power loss could roll seq back inside one
// epoch and a token epoch:seq come back with other content.
func TestOpenSyncsEveryCommit(t *testing.T) {
	c, _ := openNew(t)
	// Every pooled connection gets the DSN's pragmas: hold two at once.
	conns := make([]int, 0, 2)
	tx1, err := c.db.BeginTx(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback() }()
	var mode int
	if err := tx1.QueryRowContext(bg, `PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	conns = append(conns, mode)
	if err := c.db.QueryRowContext(bg, `PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	conns = append(conns, mode)
	const full = 2
	for i, m := range conns {
		if m != full {
			t.Errorf("connection %d: synchronous = %d, want %d (FULL)", i, m, full)
		}
	}
}

// buildPinlessFile writes a version 1 file with stored rows and no pins
// table (an M3 file).
func buildPinlessFile(t *testing.T, path string) {
	t.Helper()
	c := openAt(t, path, "acct-1")
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{prow("a", "S1", "C1", "", 1), prow("b", "S2", "C2", "A2", 2)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE pins`, `DROP TABLE aliases`, `PRAGMA user_version = 1`} {
		if _, err := c.db.ExecContext(bg, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close()
}

// A migration that stops half way (a crash, SQLITE_BUSY, a full disk)
// leaves the file as it was, so the next open migrates it in full and the
// stored rows get their pins (state-I6): the schema, user_version and the
// backfill are one transaction.
func TestMigrationStoppedHalfWayIsRedone(t *testing.T) {
	for _, step := range []string{migrateSchema, migratePins} {
		t.Run(step, func(t *testing.T) {
			path := Path(t.TempDir())
			buildPinlessFile(t, path)
			crash := errors.New("crash")
			migrateStep = func(s string) error {
				if s == step {
					return crash
				}
				return nil
			}
			c, err := Open(path, "acct-1")
			migrateStep = nil
			if err == nil {
				_ = c.Close()
				t.Fatal("Open went through the simulated crash")
			}
			if info, err := Inspect(path); err != nil || info.Schema != 1 {
				t.Fatalf("after the crash: %+v, %v; want the version 1 file untouched", info, err)
			}
			c = openAt(t, path, "acct-1")
			if info, _ := Inspect(path); info.Schema != schemaVersion {
				t.Errorf("schema after the next open = %d, want %d", info.Schema, schemaVersion)
			}
			got := pins(t, c, "cal", "a", "b")
			if got["a"] != (Pin{"S1", "C1", ""}) || got["b"] != (Pin{"S2", "C2", "A2"}) {
				t.Errorf("pins after the next open = %+v, want the stored rows' packets", got)
			}
		})
	}
}

// A normal migration runs every step once, in order, and the file then
// opens with no migration at all.
func TestMigrationRunsItsStepsOnce(t *testing.T) {
	path := Path(t.TempDir())
	buildPinlessFile(t, path)
	var steps []string
	migrateStep = func(s string) error { steps = append(steps, s); return nil }
	defer func() { migrateStep = nil }()
	c := openAt(t, path, "acct-1")
	if want := []string{migrateSchema, migratePins}; !slices.Equal(steps, want) {
		t.Errorf("migration steps = %q, want %q", steps, want)
	}
	if got := pins(t, c, "cal", "a")["a"]; got != (Pin{"S1", "C1", ""}) {
		t.Errorf("pin after the migration = %+v", got)
	}
	_ = c.Close()
	steps = nil
	_ = openAt(t, path, "acct-1")
	if len(steps) != 0 {
		t.Errorf("reopening a migrated file ran %q", steps)
	}
}

// Open puts pcb.db and its -wal and -shm back to 0600 (state Minor): a
// restored or copied file keeps whatever mode it came with.
func TestOpenRepairsTheFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	c, path := openNew(t)
	if _, err := c.ReplaceCalendar(bg, "cal", "c1", []Row{row("a", 1)}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	files := []string{path, path + "-wal", path + "-shm"}
	for _, p := range files {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = openAt(t, path, "acct-1")
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := info.Mode().Perm(); m != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(p), m)
		}
	}
}

// Remove fsyncs the state directory (state Minor), as the secrets' wipe.
func TestRemoveSyncsTheDirectory(t *testing.T) {
	c, path := openNew(t)
	_ = c.Close()
	var synced []string
	old := syncDir
	syncDir = func(dir string) { synced = append(synced, dir); old(dir) }
	defer func() { syncDir = old }()
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, filepath.Dir(path)) {
		t.Errorf("directories synced = %q, want %s", synced, filepath.Dir(path))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pcb.db still there: %v", err)
	}
}
