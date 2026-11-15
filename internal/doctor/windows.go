package doctor

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
	"github.com/Barben360/proton-calendar-bridge/internal/winzone"
)

// The checks pcb doctor runs on Windows only (list_windows.go). They call
// no Windows API themselves, so they are built and tested everywhere.

// ElevatedCheck warns when pcb runs elevated: the files it creates then
// belong to Administrators (its directories stay the user's, their owner
// set explicitly), which a later normal run accepts but which should not
// be the habit.
type ElevatedCheck struct{ Elevated bool }

// Name implements Check.
func (ElevatedCheck) Name() string { return "elevated" }

// Run implements Check.
func (c ElevatedCheck) Run(context.Context) Result {
	if !c.Elevated {
		return Result{Name: c.Name(), Status: StatusOK, Message: "not elevated"}
	}
	return Result{Name: c.Name(), Status: StatusWarn,
		Message: "pcb runs elevated (as administrator): the files it creates are owned by Administrators",
		Fix:     "run pcb from a normal (not elevated) terminal"}
}

// AutostartCheck checks pcb-ui's Start at login entry (the Run value
// runkey.Value): it must start an existing pcb-ui.exe, and it warns when
// that is not the one beside this pcb.exe.
type AutostartCheck struct {
	// Read is the Run value's data; ok is false when it is absent
	// (runkey.Store.Get).
	Read func() (data string, ok bool, err error)
	// Beside is the pcb-ui.exe beside this executable; "" when where this
	// executable lies is unknown.
	Beside string
	// Exists reports whether a file exists.
	Exists func(string) bool
}

// Name implements Check.
func (AutostartCheck) Name() string { return "start at login" }

// autostartFix is the fix for a Run value that starts nothing: pcb-ui
// rewrites it with its own path.
const autostartFix = "turn Start at login off and on again in pcb-ui"

// Run implements Check. With no pcb-ui.exe beside this pcb (pcb.exe alone,
// go run) there is nothing to compare: the entry is fine as long as its
// program exists.
func (c AutostartCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	data, ok, err := c.Read()
	switch {
	case err != nil:
		r.Status, r.Message = StatusWarn, "the Start at login entry cannot be read: "+err.Error()
		return r
	case !ok:
		r.Status, r.Message = StatusSkip, "Start at login is off"
		return r
	}
	exe, ok := runkey.Exe(data)
	switch {
	case !ok:
		r.Status, r.Message, r.Fix = StatusError, "Start at login runs no program: its entry is "+data, autostartFix
	case !c.Exists(exe):
		r.Status, r.Message, r.Fix = StatusError, "Start at login runs "+exe+", which does not exist", autostartFix
	case c.Beside == "":
		r.Status, r.Message = StatusWarn, "starts "+exe+"; where this pcb lies is unknown, so it cannot be compared with the pcb-ui beside it"
	case !samePath(exe, c.Beside) && !c.Exists(c.Beside):
		r.Status, r.Message = StatusOK, "starts "+exe+"; no pcb-ui.exe beside this pcb"
	case !samePath(exe, c.Beside):
		r.Status = StatusWarn
		r.Message = "starts " + exe + ", not " + c.Beside + " beside this pcb"
		r.Fix = "to start " + c.Beside + " at login, turn Start at login off and on again in it"
	default:
		r.Status, r.Message = StatusOK, "starts "+exe
	}
	return r
}

// samePath compares two Windows paths as the file system does: cleaned,
// case aside. Cleaning uses Windows' separators on every OS, so that the
// comparison is tested everywhere.
func samePath(a, b string) bool {
	clean := func(p string) string { return filepath.Clean(strings.ReplaceAll(p, `\`, "/")) }
	return strings.EqualFold(clean(a), clean(b))
}

// ZoneCheck reports the IANA zone the Windows zone key maps to (the MCP
// tools name times by it); a key Unicode CLDR's table does not list, or
// one that cannot be read, is a warning naming it, unless [mcp] timezone
// names the zone the MCP tools use (Configured).
type ZoneCheck struct {
	Key   string
	KeyOK bool // false: the key cannot be read
	// Configured is [mcp] timezone ("" when unset).
	Configured string
}

// Name implements Check.
func (ZoneCheck) Name() string { return "time zone" }

// Run implements Check.
func (c ZoneCheck) Run(context.Context) Result {
	r := Result{Name: c.Name()}
	if !c.KeyOK && c.Configured != "" {
		r.Status, r.Message = StatusOK, "the Windows time zone cannot be read; the MCP tools use [mcp] timezone = "+c.Configured
		return r
	}
	if !c.KeyOK {
		r.Status, r.Message = StatusWarn, "the Windows time zone cannot be read"
		r.Fix = "set [mcp] timezone in config.toml to your IANA zone, such as Europe/Paris"
		return r
	}
	name, ok := winzone.IANA(c.Key)
	if !ok && c.Configured != "" {
		r.Status, r.Message = StatusOK, c.Key+" has no IANA name in pcb's table; the MCP tools use [mcp] timezone = "+c.Configured
		return r
	}
	if !ok {
		r.Status = StatusWarn
		r.Message = "the Windows time zone " + c.Key + " has no IANA name in pcb's table (Unicode CLDR " + winzone.CLDRVersion +
			"): the MCP tools label times by their UTC offsets and write new times as UTC instants"
		r.Fix = "set [mcp] timezone in config.toml to your IANA zone, such as Europe/Paris"
		return r
	}
	r.Status, r.Message = StatusOK, c.Key+" → "+name
	return r
}

// DPAPIKeyCheck checks a dpapi state's key (seal.DPAPIKeyName in the state
// directory) as pcb reads it: its access list and its directory's (aclProblem),
// that it is a DPAPI blob at all (seal.HasDPAPIHeader), then that this
// Windows user can unprotect it to a 32-byte key protected for the user, not
// the machine (seal.DPAPI.Check). A missing key is skipped while no account
// has sealed secrets: the first login creates it.
type DPAPIKeyCheck struct {
	Path string
	// Sealed is whether an account holds secrets.sealed: a missing key is
	// then an error (the secrets check reports the loss).
	Sealed bool
	// StateDir is the directory the state dir check judges: when it holds
	// the key, its access list is not reported a second time here.
	StateDir string
}

// Name implements Check.
func (DPAPIKeyCheck) Name() string { return "dpapi key" }

// dpapiCheck is seal.DPAPI.Check; tests replace it.
var dpapiCheck = func(path string) error { return (&seal.DPAPI{Path: path}).Check() }

// dpapiCorruptFix is the fix for a DPAPI key that no Windows session opens.
const dpapiCorruptFix = "restore it from a backup; with no secrets.sealed, remove it and run pcb login"

// Run implements Check.
func (c DPAPIKeyCheck) Run(ctx context.Context) Result {
	r := FileModeCheck{Label: c.Name(), Path: c.Path, Want: 0o600}.Run(ctx)
	switch r.Status {
	case StatusOK:
	case StatusSkip: // missing
		if c.Sealed {
			r.Status, r.Message = StatusError, c.Path+" does not exist"
		} else {
			r.Message = "no key yet: the first login creates it"
		}
		return r
	default:
		return r
	}
	dir := filepath.Dir(c.Path)
	// The state dir check judges the state directory: one cause, one error.
	if c.StateDir == "" || !samePath(dir, c.StateDir) {
		if msg, fix, checked := aclProblem(dir, true); checked && msg != "" {
			r.Status, r.Message, r.Fix = StatusError, msg+" (the directory of "+c.Path+")", fix
			return r
		}
	}
	head, err := readHead(c.Path)
	if err == nil && !seal.HasDPAPIHeader(head) {
		r.Status, r.Message, r.Fix = StatusError, c.Path+" is not a DPAPI-protected key: it cannot be opened by this Windows user", dpapiCorruptFix
		return r
	}
	if err == nil {
		err = dpapiCheck(c.Path)
	}
	var pe *seal.ProtectError
	switch {
	case err == nil:
		r.Message = c.Path + ": protected for this Windows user, 32 bytes"
	case errors.As(err, &pe):
		// The key opens in a logon with the password: nothing is lost.
		r.Status, r.Message, r.Fix, r.Transient = StatusError, c.Path+": "+pe.Error(), service.DPAPIUnprotectFix, true
	case errors.Is(err, seal.ErrCorrupt):
		r.Status, r.Message, r.Fix = StatusError, c.Path+" cannot be opened by this Windows user: "+corruptCause(c.Path, err), dpapiCorruptFix
	case errors.Is(err, fs.ErrPermission):
		r.Status, r.Message, r.Fix = StatusError, c.Path+" is not readable by this user: "+causeOf(err), unreadableKeyFix(c.Path)
	default:
		r.Status, r.Message = StatusError, c.Path+" cannot be read: "+causeOf(err)
	}
	return r
}

// corruptCause is why seal called the key at path corrupt, without seal's
// lead for sealed data ("it is damaged or not pcb's: <Windows' error>",
// "it is protected for the whole machine, not this user", "it is not 32
// bytes"); the whole error when it reads otherwise.
func corruptCause(path string, err error) string {
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, seal.ErrCorrupt.Error()+": key file "+path+" "); ok {
		return "it " + rest
	}
	return msg
}

// readHead is the first bytes of path, enough for a DPAPI header.
func readHead(path string) ([]byte, error) {
	f, err := atomicfile.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	head := make([]byte, 20)
	n, err := io.ReadFull(f, head)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	return head[:n], err
}
