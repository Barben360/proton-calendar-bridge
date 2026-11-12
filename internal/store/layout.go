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

// accountFiles are one account's files: at the state root in M5, in
// accounts/<local-id>/ since layout v2. Migrate moves them in this order.
var accountFiles = []string{
	"state.json", "state.lock", "secrets.sealed", "secrets.lock", "clients.json",
	"clients.lock", "pcb.db", "pcb.db-wal", "pcb.db-shm", "sync.lock",
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
}

// ReadLayout reads what stateDir holds and writes nothing. A layout.json
// that is malformed or of another version is an error (fail closed). When
// the layout is v2 and Legacy is not empty it returns the full info with an
// error wrapping ErrLegacyBesideLayout.
func ReadLayout(stateDir string) (LayoutInfo, error) {
	var info LayoutInfo
	v, err := readLayoutVersion(stateDir)
	if err != nil {
		return LayoutInfo{}, err
	}
	info.Version = v
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
	Version int `json:"version"`
}

// readLayoutVersion is layout.json's version, 0 when there is none.
func readLayoutVersion(stateDir string) (int, error) {
	path := filepath.Join(stateDir, layoutName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var l layoutFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return 0, fmt.Errorf("%s is malformed: %w", path, err)
	}
	if dec.More() {
		return 0, fmt.Errorf("%s is malformed: data after the JSON value", path)
	}
	switch {
	case l.Version == LayoutVersion:
		return l.Version, nil
	case l.Version > LayoutVersion:
		return 0, fmt.Errorf("%s: version %d was written by a newer pcb", path, l.Version)
	}
	return 0, fmt.Errorf("%s: unknown version %d", path, l.Version)
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
		if err := writeLayout(stateDir); err != nil {
			return "", false, err
		}
		return info.Accounts[0], true, nil
	case len(info.Legacy) == 0: // a new state
		if err := makeDir(stateDir, AccountsDir(stateDir)); err != nil {
			return "", false, err
		}
		return "", false, writeLayout(stateDir)
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
	return writeLayout(stateDir)
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

func writeLayout(stateDir string) error {
	raw, err := json.Marshal(layoutFile{Version: LayoutVersion})
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(stateDir, layoutName), append(raw, '\n'))
}
