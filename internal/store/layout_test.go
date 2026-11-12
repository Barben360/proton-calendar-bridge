package store_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// m5Files are an M5 account's files at the state root, the exact list
// Migrate moves.
var m5Files = []string{
	"state.json", "state.lock", "secrets.sealed", "secrets.lock", "clients.json",
	"clients.lock", "pcb.db", "pcb.db-wal", "pcb.db-shm", "sync.lock",
}

// m5DataFiles are those that hold data: LayoutInfo.Legacy lists them.
var m5DataFiles = []string{"state.json", "secrets.sealed", "clients.json", "pcb.db", "pcb.db-wal", "pcb.db-shm"}

// m5Clients is a clients.json in M5's format (version 2), written as a
// literal: store tests do not import internal/access (ruling R1).
const m5Clients = `{"version":2,"clients":[{"id":"c1","name":"thunderbird","salt":"c2FsdA==",` +
	`"hash":"aGFzaA==","sealed":"c2VhbGVk","created":"2026-11-01T10:00:00Z"}]}` + "\n"

// writeM5State writes an M5 state at root: state.json and secrets.sealed
// through the store's own functions, clients.json as m5Clients, a cache, the
// lock files, and the root files that stay. It returns the key file.
func writeM5State(t *testing.T, root string) (key string) {
	t.Helper()
	key = filepath.Join(t.TempDir(), "secrets.key")
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := store.SaveState(root, store.State{AccountID: "userA", Phase: store.PhaseReady, LastLogin: at}); err != nil {
		t.Fatal(err)
	}
	put(t, store.NewSecrets(root, &seal.KeyFile{Path: key}), fullAccount("userA"))
	writeFile(t, filepath.Join(root, "clients.json"), []byte(m5Clients))
	writeFile(t, filepath.Join(root, "pcb.db"), []byte("SQLite format 3\x00 main"))
	writeFile(t, filepath.Join(root, "pcb.db-wal"), []byte("wal frames"))
	writeFile(t, filepath.Join(root, "pcb.db-shm"), []byte("shm index"))
	for _, name := range m5Files { // the lock files (off Unix no lock creates them)
		if _, err := os.Lstat(filepath.Join(root, name)); errors.Is(err, fs.ErrNotExist) {
			writeFile(t, filepath.Join(root, name), nil)
		}
	}
	writeFile(t, filepath.Join(root, "login.lock"), nil)
	writeFile(t, filepath.Join(root, "last-login"), []byte("2026-11-01T10:00:00Z\n"))
	for _, dir := range []string{"tls", "logs"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "tls", "cert.pem"), []byte("CERT"))
	writeFile(t, filepath.Join(root, "logs", "pcb.log"), []byte("log line\n"))
	return key
}

const isDir = "<dir>"

// tree maps every path under root (slash-separated, relative) to the file's
// contents, or isDir.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			got[filepath.ToSlash(rel)] = isDir
			return nil
		}
		raw, err := os.ReadFile(path)
		got[filepath.ToSlash(rel)] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// sameTree reports each path whose presence or contents differ.
func sameTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			t.Errorf("%s missing", p)
		case g != w:
			t.Errorf("%s = %q, want %q", p, g, w)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected %s", p)
		}
	}
}

// layoutVersion is the version layout.json holds under root.
func layoutVersion(t *testing.T, root string) int {
	t.Helper()
	var l struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(readFile(t, filepath.Join(root, "layout.json")), &l); err != nil {
		t.Fatal(err)
	}
	return l.Version
}

// checkMigrated checks root against before, the tree of the M5 state it held:
// each account file moved to accounts/<id>/ with its bytes, every other path
// in place, accounts/ and accounts/<id> (0700), layout.json version 2, and
// nothing else.
func checkMigrated(t *testing.T, root string, before map[string]string, id string) {
	t.Helper()
	want := map[string]string{"accounts": isDir, "accounts/" + id: isDir}
	for p, v := range before {
		if slices.Contains(m5Files, p) {
			p = "accounts/" + id + "/" + p
		}
		want[p] = v
	}
	got := tree(t, root)
	if _, ok := got["layout.json"]; !ok {
		t.Fatal("no layout.json")
	}
	if v := layoutVersion(t, root); v != store.LayoutVersion {
		t.Errorf("layout.json version %d, want %d", v, store.LayoutVersion)
	}
	delete(got, "layout.json")
	sameTree(t, got, want)
	if runtime.GOOS != "windows" {
		for _, dir := range []string{store.AccountsDir(root), store.AccountDir(root, id)} {
			if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("%s: %v, %v; want mode 0700", dir, info, err)
			}
		}
	}
}

// fixedID is a newID that answers id.
func fixedID(id string) func() (string, error) { return func() (string, error) { return id, nil } }

// noNewID is a newID that must not be called.
func noNewID(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Error("a new local ID was asked for")
		return "", errors.New("no new ID")
	}
}

func TestFreshStateHasNoLayout(t *testing.T) {
	root := t.TempDir()
	info, err := store.ReadLayout(root)
	if err != nil || info.Version != 0 || len(info.Accounts) != 0 || len(info.Legacy) != 0 || info.Migrating != "" {
		t.Fatalf("ReadLayout(empty) = %+v, %v", info, err)
	}
	if got := tree(t, root); len(got) != 0 {
		t.Fatalf("ReadLayout wrote %v", got)
	}
	id, migrated, err := store.Migrate(root, noNewID(t))
	if err != nil || migrated || id != "" {
		t.Fatalf("Migrate = %q, %v, %v; want nothing migrated", id, migrated, err)
	}
	sameTree(t, tree(t, root), map[string]string{"accounts": isDir, "layout.json": "{\"version\":2}\n"})
	if v := layoutVersion(t, root); v != 2 {
		t.Errorf("layout.json version %d", v)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(store.AccountsDir(root)); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("accounts/: %v, %v; want mode 0700", info, err)
		}
	}
	if info, err := store.ReadLayout(root); err != nil || info.Version != 2 || len(info.Accounts) != 0 {
		t.Fatalf("ReadLayout after = %+v, %v", info, err)
	}
}

func TestMigrateMovesM5Files(t *testing.T) {
	root := t.TempDir()
	key := writeM5State(t, root)
	before := tree(t, root)
	info, err := store.ReadLayout(root)
	if err != nil || info.Version != 0 || !slices.Equal(info.Legacy, m5DataFiles) || len(info.Accounts) != 0 {
		t.Fatalf("ReadLayout(M5) = %+v, %v; want legacy %v", info, err, m5DataFiles)
	}
	id, migrated, err := store.Migrate(root, fixedID("0a1b2c3d"))
	if err != nil || !migrated || id != "0a1b2c3d" {
		t.Fatalf("Migrate = %q, %v, %v", id, migrated, err)
	}
	checkMigrated(t, root, before, id)
	if d := store.AccountDir(root, id); d != filepath.Join(root, "accounts", id) {
		t.Errorf("AccountDir = %s", d)
	}
	// The moved account still opens: the sealed store and its mirror together.
	dir := store.AccountDir(root, id)
	got, err := store.NewSecrets(dir, &seal.KeyFile{Path: key}).Load()
	if err != nil || got["userA"].RefreshToken != fullAccount("userA").RefreshToken {
		t.Fatalf("secrets after the move: %v, %v", got, err)
	}
	if st, err := store.LoadState(dir); err != nil || st.AccountID != "userA" || st.SecretsGeneration != 1 {
		t.Fatalf("state after the move: %+v, %v", st, err)
	}
	if info, err := store.ReadLayout(root); err != nil || info.Version != 2 || !slices.Equal(info.Accounts, []string{id}) ||
		len(info.Legacy) != 0 || info.Migrating != "" {
		t.Fatalf("ReadLayout after = %+v, %v", info, err)
	}
	// Once done it is idempotent: nothing migrated, nothing changed.
	after := tree(t, root)
	if id, migrated, err := store.Migrate(root, noNewID(t)); err != nil || migrated || id != "" {
		t.Fatalf("second Migrate = %q, %v, %v", id, migrated, err)
	}
	sameTree(t, tree(t, root), after)
}

var errCrash = errors.New("simulated crash")

// crashAfter makes Migrate stop right after its n-th rename, as a crash
// there would.
func crashAfter(n int) (restore func()) {
	calls := 0
	return store.SetRenameHook(func(_, _ string) error {
		if calls++; calls == n {
			return errCrash
		}
		return nil
	})
}

// whereIs counts the copies of each account file across the root, the
// staging directory and the account directory.
func whereIs(t *testing.T, root, id string) {
	t.Helper()
	for _, name := range m5Files {
		n := 0
		for _, dir := range []string{root, filepath.Join(store.AccountsDir(root), id+".migrating"), store.AccountDir(root, id)} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d copies after the crash, want 1", name, n)
		}
	}
}

func TestMigrationResumesAfterEachStep(t *testing.T) {
	const id = "deadbeef"
	// Renames 1 to 10 move the files, 11 renames the directory: a crash
	// after it is one before layout.json.
	for k := 1; k <= len(m5Files)+1; k++ {
		root := t.TempDir()
		writeM5State(t, root)
		before := tree(t, root)
		restore := crashAfter(k)
		_, _, err := store.Migrate(root, fixedID(id))
		restore()
		if !errors.Is(err, errCrash) {
			t.Fatalf("crash after rename %d: Migrate = %v", k, err)
		}
		whereIs(t, root, id)
		info, err := store.ReadLayout(root)
		switch {
		case err != nil || info.Version != 0:
			t.Fatalf("crash after rename %d: ReadLayout = %+v, %v", k, info, err)
		case k <= len(m5Files) && (info.Migrating != id || len(info.Accounts) != 0):
			t.Fatalf("crash after move %d: ReadLayout = %+v, want %s migrating", k, info, id)
		case k > len(m5Files) && (info.Migrating != "" || !slices.Equal(info.Accounts, []string{id})):
			t.Fatalf("crash after the directory rename: ReadLayout = %+v, want account %s", info, id)
		}
		got, migrated, err := store.Migrate(root, noNewID(t))
		if err != nil || !migrated || got != id {
			t.Fatalf("resume after rename %d: Migrate = %q, %v, %v; want %s", k, got, migrated, err, id)
		}
		checkMigrated(t, root, before, id)
	}
	// A crash after the staging directory is made, before any move.
	root := t.TempDir()
	writeM5State(t, root)
	before := tree(t, root)
	if err := os.MkdirAll(filepath.Join(store.AccountsDir(root), id+".migrating"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, migrated, err := store.Migrate(root, noNewID(t)); err != nil || !migrated || got != id {
		t.Fatalf("resume from an empty staging directory: %q, %v, %v", got, migrated, err)
	}
	checkMigrated(t, root, before, id)
}

func TestMigrationResumeRefusesASecondCopy(t *testing.T) {
	const id = "deadbeef"
	root := t.TempDir()
	writeM5State(t, root)
	before := tree(t, root)
	restore := crashAfter(len(m5Files)) // every file staged, the directory not renamed
	_, _, err := store.Migrate(root, fixedID(id))
	restore()
	if !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	// An older pcb ran meanwhile and logged in: a second state.json at the root.
	newer := filepath.Join(root, "state.json")
	writeFile(t, newer, []byte(`{"phase":"ready","account_id":"userB"}`))
	staged := tree(t, root)
	_, _, err = store.Migrate(root, noNewID(t))
	if !errors.Is(err, store.ErrLegacyBesideLayout) || !strings.Contains(err.Error(), "state.json") {
		t.Fatalf("Migrate = %v, want ErrLegacyBesideLayout naming state.json", err)
	}
	sameTree(t, tree(t, root), staged)
	// A lock file at both places holds no data: the resume goes on (pcb
	// itself recreates the root sync.lock when it takes it).
	if err := os.Remove(newer); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "sync.lock"), nil)
	if got, migrated, err := store.Migrate(root, noNewID(t)); err != nil || !migrated || got != id {
		t.Fatalf("resume with a second sync.lock: %q, %v, %v", got, migrated, err)
	}
	checkMigrated(t, root, before, id)
}

func TestLegacyBesideLayoutIsRefused(t *testing.T) {
	for _, name := range m5DataFiles {
		root := t.TempDir()
		writeM5State(t, root)
		if _, _, err := store.Migrate(root, fixedID("0a1b2c3d")); err != nil {
			t.Fatal(err)
		}
		// An older pcb wrote one of its account files beside layout.json.
		writeFile(t, filepath.Join(root, name), []byte("written by M5"))
		before := tree(t, root)
		info, err := store.ReadLayout(root)
		if !errors.Is(err, store.ErrLegacyBesideLayout) || info.Version != 2 || !slices.Equal(info.Legacy, []string{name}) {
			t.Fatalf("%s: ReadLayout = %+v, %v", name, info, err)
		}
		id, migrated, err := store.Migrate(root, noNewID(t))
		if !errors.Is(err, store.ErrLegacyBesideLayout) || !strings.Contains(err.Error(), name) || migrated || id != "" {
			t.Fatalf("%s: Migrate = %q, %v, %v", name, id, migrated, err)
		}
		sameTree(t, tree(t, root), before)
	}
}

func TestLayoutRootLockFilesAreNoAccount(t *testing.T) {
	locks := []string{"state.lock", "secrets.lock", "clients.lock", "sync.lock", "login.lock"}
	root := t.TempDir()
	for _, name := range locks {
		writeFile(t, filepath.Join(root, name), nil)
	}
	// A state with lock files only is new: no account is made of them.
	id, migrated, err := store.Migrate(root, noNewID(t))
	if err != nil || migrated || id != "" {
		t.Fatalf("Migrate = %q, %v, %v", id, migrated, err)
	}
	want := map[string]string{"accounts": isDir, "layout.json": "{\"version\":2}\n"}
	for _, name := range locks {
		want[name] = ""
	}
	sameTree(t, tree(t, root), want)
	// Beside layout.json they are not an older pcb's account either (the
	// serving process holds the root sync.lock).
	if info, err := store.ReadLayout(root); err != nil || len(info.Legacy) != 0 {
		t.Fatalf("ReadLayout = %+v, %v", info, err)
	}
	if _, _, err := store.Migrate(root, noNewID(t)); err != nil {
		t.Fatal(err)
	}
	sameTree(t, tree(t, root), want)
}

func TestLayoutUnknownVersionIsRefused(t *testing.T) {
	for name, content := range map[string]string{
		"newer":         `{"version":3}`,
		"older":         `{"version":1}`,
		"missing":       `{}`,
		"garbage":       `{`,
		"unknown field": `{"version":2,"x":1}`,
		"trailing data": `{"version":2} {}`,
	} {
		root := t.TempDir()
		writeM5State(t, root)
		writeFile(t, filepath.Join(root, "layout.json"), []byte(content))
		before := tree(t, root)
		if _, err := store.ReadLayout(root); err == nil {
			t.Errorf("%s: ReadLayout accepted %s", name, content)
		}
		if _, _, err := store.Migrate(root, noNewID(t)); err == nil {
			t.Errorf("%s: Migrate accepted %s", name, content)
		}
		sameTree(t, tree(t, root), before)
	}
}

func TestMigrateRefusesABadLocalID(t *testing.T) {
	for _, bad := range []string{"", "../../x", "0A1B2C3D", "0a1b2c3"} {
		root := t.TempDir()
		writeM5State(t, root)
		before := tree(t, root)
		if _, _, err := store.Migrate(root, fixedID(bad)); err == nil {
			t.Errorf("Migrate took local ID %q", bad)
		}
		sameTree(t, tree(t, root), before)
	}
	// An ID already in use is never reused.
	root := t.TempDir()
	writeM5State(t, root)
	if err := os.Mkdir(store.AccountsDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, store.AccountDir(root, "0a1b2c3d"), []byte("in the way"))
	before := tree(t, root)
	if _, _, err := store.Migrate(root, fixedID("0a1b2c3d")); err == nil {
		t.Error("Migrate reused a local ID")
	}
	sameTree(t, tree(t, root), before)
}

func TestLocalIDShape(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for range 1000 {
		id, err := store.NewLocalID(func(string) bool { return false })
		if err != nil || !shape.MatchString(id) || !store.ValidLocalID(id) {
			t.Fatalf("NewLocalID = %q, %v", id, err)
		}
		seen[id] = true
	}
	if len(seen) < 990 {
		t.Errorf("%d distinct IDs in 1000", len(seen))
	}
	var refused []string
	id, err := store.NewLocalID(func(id string) bool {
		if len(refused) < 2 {
			refused = append(refused, id)
			return true
		}
		return false
	})
	if err != nil || len(refused) != 2 || slices.Contains(refused, id) || !shape.MatchString(id) {
		t.Fatalf("NewLocalID = %q, %v after refusing %q", id, err, refused)
	}
	if _, err := store.NewLocalID(func(string) bool { return true }); err == nil {
		t.Error("NewLocalID answered an ID that exists")
	}
	for id, want := range map[string]bool{
		"0a1b2c3d": true, "00000000": true, "0A1B2C3D": false, "0a1b2c3": false,
		"0a1b2c3d0": false, "0a1b2c3g": false, "../x": false, "": false, "deadbeef.migrating": false,
	} {
		if got := store.ValidLocalID(id); got != want {
			t.Errorf("ValidLocalID(%q) = %v", id, got)
		}
	}
}

func TestStateKeepsAddressAndSwitch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	created := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	off := false
	want := store.State{Phase: store.PhaseReady, AccountID: "userA", Address: "alice@proton.me", CalDAV: &off, Created: created}
	if err := store.SaveState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadState(dir)
	if err != nil || got.Address != "alice@proton.me" || got.CalDAV == nil || *got.CalDAV || !got.Created.Equal(created) {
		t.Fatalf("got %+v, %v", got, err)
	}
	raw := string(readFile(t, filepath.Join(dir, "state.json")))
	for _, k := range []string{`"address": "alice@proton.me"`, `"caldav": false`, `"created": "2026-11-12T09:30:00Z"`} {
		if !strings.Contains(raw, k) {
			t.Errorf("missing %s in %s", k, raw)
		}
	}
	// Unset, they are not written; an M5 state.json loads with the switch unset (on).
	if err := store.SaveState(dir, store.State{Phase: store.PhaseLoggedOut}); err != nil {
		t.Fatal(err)
	}
	if raw := string(readFile(t, filepath.Join(dir, "state.json"))); strings.Contains(raw, "address") ||
		strings.Contains(raw, "caldav") || strings.Contains(raw, "created") {
		t.Errorf("unset fields written: %s", raw)
	}
	writeFile(t, filepath.Join(dir, "state.json"), []byte(`{"account_id":"userA","phase":"ready","secrets_generation":3}`))
	if got, err := store.LoadState(dir); err != nil || got.CalDAV != nil || got.Address != "" || !got.Created.IsZero() {
		t.Fatalf("M5 state.json: %+v, %v", got, err)
	}
}

func TestMigrateSyncsTheDirectories(t *testing.T) {
	root := t.TempDir()
	writeM5State(t, root)
	var synced []string
	defer store.OnSyncDir(func(dir string) { synced = append(synced, dir) })()
	if _, _, err := store.Migrate(root, fixedID("0a1b2c3d")); err != nil {
		t.Fatal(err)
	}
	accounts := store.AccountsDir(root)
	for _, dir := range []string{root, accounts, filepath.Join(accounts, "0a1b2c3d.migrating")} {
		if !slices.Contains(synced, dir) {
			t.Errorf("%s not synced; synced %q", dir, synced)
		}
	}
	// The directory rename is made durable last, before layout.json.
	if synced[len(synced)-1] != accounts {
		t.Errorf("last sync %s, want %s", synced[len(synced)-1], accounts)
	}
}

func TestMigrateRefusesAnInconsistentLayout(t *testing.T) {
	mkdir := func(t *testing.T, root string, names ...string) {
		t.Helper()
		for _, n := range names {
			if err := os.MkdirAll(filepath.Join(store.AccountsDir(root), n), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, setup := range map[string]func(t *testing.T, root string){
		"staging beside layout.json": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "layout.json"), []byte(`{"version":2}`))
			mkdir(t, root, "deadbeef.migrating")
		},
		"staging beside an account": func(t *testing.T, root string) { mkdir(t, root, "deadbeef.migrating", "0a1b2c3d") },
		"two stagings":              func(t *testing.T, root string) { mkdir(t, root, "deadbeef.migrating", "0a1b2c3d.migrating") },
		"two accounts, no layout":   func(t *testing.T, root string) { mkdir(t, root, "deadbeef", "0a1b2c3d") },
		"root data beside an account": func(t *testing.T, root string) {
			mkdir(t, root, "deadbeef")
			writeFile(t, filepath.Join(root, "secrets.sealed"), []byte("written by M5"))
		},
	} {
		root := t.TempDir()
		setup(t, root)
		before := tree(t, root)
		if id, migrated, err := store.Migrate(root, noNewID(t)); err == nil || migrated || id != "" {
			t.Errorf("%s: Migrate = %q, %v, %v", name, id, migrated, err)
		}
		sameTree(t, tree(t, root), before)
	}
}
