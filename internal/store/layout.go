package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// LayoutVersion is the version layout.json holds: each account's files in
// accounts/<local-id>/ (M6). Its absence is M5's layout, the one account's
// files at the state root, or a new state.
const LayoutVersion = 2

const (
	layoutName      = "layout.json"
	accountsName    = "accounts"
	migratingSuffix = ".migrating"
	localIDLen      = 8
	// maxLocalIDTries bounds NewLocalID: among 2^32 IDs a handful of
	// accounts make a second try rare, so many failed tries mean exists
	// answers true for every ID.
	maxLocalIDTries = 64
)

// accountFiles are the account files Migrate moves from the state root (M5)
// into accounts/<local-id>/ (layout v2), in this order. M5's root sync.lock
// stays: it is layout v2's root lock (the serving process's, and the one the
// migrating caller holds); an account's own sync.lock is made in its
// directory when first taken.
var accountFiles = []string{
	"state.json", "state.lock", "secrets.sealed", "secrets.lock", "clients.json",
	"clients.lock", "pcb.db", "pcb.db-wal", "pcb.db-shm",
}

// legacyFiles are the account files that hold data. One of them at the root
// is an M5 account (LayoutInfo.Legacy); the lock files hold none, and in
// layout v2 the root sync.lock is the serving process's own.
var legacyFiles = []string{"state.json", "secrets.sealed", "clients.json", "pcb.db", "pcb.db-wal", "pcb.db-shm"}

// ErrLegacyBesideLayout means account files of an older pcb (M5's layout)
// are at the state root beside the per-account layout: an older pcb ran on
// the state after M6 had moved it. Which copy is current cannot be told, so
// nothing is moved; the person removes them or moves the state aside. The
// error names the files.
var ErrLegacyBesideLayout = errors.New("account files of an older pcb are beside the per-account layout")

// ErrNewerLayout is the cause of ReadLayout's (and Migrate's) error for a
// layout.json above LayoutVersion: a newer pcb wrote it. It stays refused
// (fail closed), but it is not damaged: upgrading pcb reads it.
var ErrNewerLayout = errors.New("layout.json was written by a newer pcb")

// ErrUnknownSealer is the cause of ReadLayout's (and Migrate's) error for a
// layout.json naming a sealer this pcb does not know (a newer pcb wrote it:
// upgrading pcb reads it), and of CreateLayout's for a sealer it cannot
// record. Like ErrNewerLayout it fails closed.
var ErrUnknownSealer = errors.New("layout.json names a sealer this pcb does not know")

// Sealer names layout.json records; they are seal.KeyFile's and
// seal.SecretService's Name (Task 0's Name tests pin the same strings).
const (
	sealerKeyFile       = "keyfile"
	sealerSecretService = "secret-service"
)

func knownSealer(s string) bool { return s == sealerKeyFile || s == sealerSecretService }

// renameHook, when set, runs after each rename Migrate makes; its error
// stops Migrate there, as a crash right after that rename would (tests).
var renameHook func(from, to string) error

// AccountsDir is the directory of the accounts' directories.
func AccountsDir(stateDir string) string { return filepath.Join(stateDir, accountsName) }

// AccountDir is the directory of the account localID.
func AccountDir(stateDir, localID string) string {
	return filepath.Join(AccountsDir(stateDir), localID)
}

// NewLocalID answers a new local account ID: 8 random lowercase hex
// characters, drawn again while exists reports the ID taken.
func NewLocalID(exists func(id string) bool) (string, error) {
	var b [localIDLen / 2]byte
	for range maxLocalIDTries {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		if id := hex.EncodeToString(b[:]); !exists(id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("no free local account ID in %d tries", maxLocalIDTries)
}

// ValidLocalID reports whether id has the shape of a local account ID, so
// it is safe as a directory name.
func ValidLocalID(id string) bool {
	if len(id) != localIDLen {
		return false
	}
	for _, c := range []byte(id) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LayoutInfo is what a state directory holds.
type LayoutInfo struct {
	Version   int      // layout.json's; 0 when there is none
	Accounts  []string // the local IDs under accounts/, sorted
	Legacy    []string // M5 account data files at the root (legacyFiles order)
	Migrating string   // the local ID of an accounts/<id>.migrating directory
	Sealer    string   // layout.json's sealer; "keyfile" when it names none (M6's layout, or no layout.json)
}

// ReadLayout reads what stateDir holds and writes nothing. A layout.json
// that is malformed or of another version is an error (fail closed). When
// the layout is v2 and Legacy is not empty it returns the full info with an
// error wrapping ErrLegacyBesideLayout.
func ReadLayout(stateDir string) (LayoutInfo, error) {
	var info LayoutInfo
	l, err := readLayoutFile(stateDir)
	if err != nil {
		return LayoutInfo{}, err
	}
	info.Version, info.Sealer = l.Version, l.Sealer
	entries, err := os.ReadDir(AccountsDir(stateDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return LayoutInfo{}, err
	}
	for _, e := range entries { // sorted by name
		if !e.IsDir() {
			continue
		}
		if id, ok := strings.CutSuffix(e.Name(), migratingSuffix); ok && ValidLocalID(id) {
			if info.Migrating != "" {
				return LayoutInfo{}, fmt.Errorf("%s holds two interrupted migrations, %s and %s",
					AccountsDir(stateDir), info.Migrating+migratingSuffix, e.Name())
			}
			info.Migrating = id
		} else if ValidLocalID(e.Name()) {
			info.Accounts = append(info.Accounts, e.Name())
		}
	}
	for _, name := range legacyFiles {
		switch _, err := os.Lstat(filepath.Join(stateDir, name)); {
		case err == nil:
			info.Legacy = append(info.Legacy, name)
		case !errors.Is(err, fs.ErrNotExist):
			return LayoutInfo{}, err
		}
	}
	if info.Version == LayoutVersion && len(info.Legacy) > 0 {
		return info, legacyError(stateDir, info.Legacy)
	}
	return info, nil
}

type layoutFile struct {
	Version int    `json:"version"`
	Sealer  string `json:"sealer,omitempty"`
}

// readLayoutFile is layout.json's content: version 0 and sealer "keyfile"
// when there is none, sealer "keyfile" when it names none.
func readLayoutFile(stateDir string) (layoutFile, error) {
	path := filepath.Join(stateDir, layoutName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return layoutFile{Sealer: sealerKeyFile}, nil
	}
	if err != nil {
		return layoutFile{}, err
	}
	var l layoutFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return layoutFile{}, fmt.Errorf("%s is malformed: %w", path, err)
	}
	if dec.More() {
		return layoutFile{}, fmt.Errorf("%s is malformed: data after the JSON value", path)
	}
	switch {
	case l.Version == LayoutVersion:
		switch {
		case l.Sealer == "" && !sealerPresent(raw):
			l.Sealer = sealerKeyFile
		case !knownSealer(l.Sealer):
			return layoutFile{}, fmt.Errorf("%s: sealer %q: %w", path, l.Sealer, ErrUnknownSealer)
		}
		return l, nil
	case l.Version > LayoutVersion:
		return layoutFile{}, fmt.Errorf("%s: version %d: %w", path, l.Version, ErrNewerLayout)
	}
	return layoutFile{}, fmt.Errorf("%s: unknown version %d", path, l.Version)
}

// sealerPresent reports whether raw (a valid layout.json) has a "sealer"
// key, so that an empty value is told from an absent one.
func sealerPresent(raw []byte) bool {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	_, ok := m["sealer"]
	return ok
}

func legacyError(stateDir string, names []string) error {
	return fmt.Errorf("%w: %s in %s", ErrLegacyBesideLayout, strings.Join(names, ", "), stateDir)
}

// Migrate brings stateDir to layout v2. An M5 account (its files at the
// root) moves into accounts/<id>/ with an ID from newID, crash-safe: the
// files go into accounts/<id>.migrating/, the directory is renamed, then
// layout.json is written. A run that finds the staging directory, or the
// account directory without layout.json, resumes with that ID. A state with
// no account gets an empty accounts/ and layout.json (migrated false); a v2
// state is left as it is (migrated false). Account data at the root beside a
// layout (layout.json, an account directory, or a staged copy of the same
// file) is never moved: ErrLegacyBesideLayout. The caller holds the locks.
func Migrate(stateDir string, newID func() (string, error)) (localID string, migrated bool, err error) {
	info, err := ReadLayout(stateDir)
	if err != nil {
		return "", false, err
	}
	if info.Version == LayoutVersion {
		if info.Migrating != "" {
			return "", false, fmt.Errorf("%s holds an interrupted migration beside %s",
				AccountDir(stateDir, info.Migrating+migratingSuffix), layoutName)
		}
		return "", false, nil
	}
	switch {
	case info.Migrating != "" && len(info.Accounts) > 0:
		return "", false, fmt.Errorf("%s holds an interrupted migration beside accounts and no %s",
			AccountDir(stateDir, info.Migrating+migratingSuffix), layoutName)
	case info.Migrating != "":
		localID = info.Migrating
	case len(info.Accounts) > 0 && len(info.Legacy) > 0:
		return "", false, legacyError(stateDir, info.Legacy)
	case len(info.Accounts) > 1:
		return "", false, fmt.Errorf("%s holds %d accounts but no %s", AccountsDir(stateDir), len(info.Accounts), layoutName)
	case len(info.Accounts) == 1: // moved; only layout.json is missing
		if err := writeLayout(stateDir, sealerKeyFile); err != nil {
			return "", false, err
		}
		return info.Accounts[0], true, nil
	case len(info.Legacy) == 0: // a new state
		if err := makeDir(stateDir, AccountsDir(stateDir)); err != nil {
			return "", false, err
		}
		return "", false, writeLayout(stateDir, sealerKeyFile)
	default:
		if localID, err = freshLocalID(stateDir, newID); err != nil {
			return "", false, err
		}
	}
	if err := moveAccount(stateDir, localID); err != nil {
		return "", false, err
	}
	return localID, true, nil
}

// freshLocalID asks newID for the ID of a migrated account: valid, and
// neither an account's nor a staging directory's.
func freshLocalID(stateDir string, newID func() (string, error)) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	if !ValidLocalID(id) {
		return "", fmt.Errorf("%q is not a local account ID", id)
	}
	for _, p := range []string{AccountDir(stateDir, id), AccountDir(stateDir, id+migratingSuffix)} {
		if exists(p) {
			return "", fmt.Errorf("local account ID %s is taken: %s exists", id, p)
		}
	}
	return id, nil
}

// moveAccount moves the account files at the root into
// accounts/<id>.migrating/, renames it accounts/<id>, then writes
// layout.json, each step made durable before the next. A data file both at
// the root and staged stops it before any move; a lock file holds no data,
// and the root one replaces the staged one.
func moveAccount(stateDir, id string) error {
	accounts := AccountsDir(stateDir)
	staging := AccountDir(stateDir, id+migratingSuffix)
	var both []string
	for _, name := range legacyFiles {
		if exists(filepath.Join(stateDir, name)) && exists(filepath.Join(staging, name)) {
			both = append(both, name)
		}
	}
	if len(both) > 0 {
		return fmt.Errorf("%w: %s both in %s and in %s", ErrLegacyBesideLayout, strings.Join(both, ", "), stateDir, staging)
	}
	if err := makeDir(stateDir, accounts); err != nil {
		return err
	}
	if err := makeDir(accounts, staging); err != nil {
		return err
	}
	for _, name := range accountFiles {
		from := filepath.Join(stateDir, name)
		switch _, err := os.Lstat(from); {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return err
		}
		if err := rename(from, filepath.Join(staging, name)); err != nil {
			return err
		}
	}
	syncDir(staging)
	syncDir(stateDir)
	if err := rename(staging, AccountDir(stateDir, id)); err != nil {
		return err
	}
	syncDir(accounts)
	return writeLayout(stateDir, sealerKeyFile)
}

// exists reports whether path exists; an error other than its absence
// counts as existing, so a doubt stops the move.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// makeDir makes dir (0700) unless it exists, and fsyncs parent after.
func makeDir(parent, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	syncDir(parent)
	return nil
}

// rename is os.Rename, then renameHook.
func rename(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if renameHook != nil {
		return renameHook(from, to)
	}
	return nil
}

// writeLayout replaces layout.json (Migrate's: the caller holds the locks).
func writeLayout(stateDir, sealer string) error {
	raw, err := json.Marshal(layoutFile{Version: LayoutVersion, Sealer: sealer})
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(stateDir, layoutName), append(raw, '\n'))
}

// CreateLayout makes the layout of a new state: accounts/ (0700), then
// layout.json recording sealer, "keyfile" or "secret-service" (otherwise
// ErrUnknownSealer, nothing written). layout.json is created exclusively
// and fsynced, with its directory: when one exists already, the first
// creator's stays and CreateLayout returns nil, so a second creator that
// raced the first is harmless. The caller holds the layout lock.
func CreateLayout(stateDir, sealer string) error {
	if !knownSealer(sealer) {
		return fmt.Errorf("sealer %q: %w", sealer, ErrUnknownSealer)
	}
	if err := makeDir(stateDir, AccountsDir(stateDir)); err != nil {
		return err
	}
	raw, err := json.Marshal(layoutFile{Version: LayoutVersion, Sealer: sealer})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, layoutName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name()) // no half-written layout.json stays
		return err
	}
	syncDir(stateDir)
	return nil
}
