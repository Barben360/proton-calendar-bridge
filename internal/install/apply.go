package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// Hooks are what Apply cannot do on its own.
type Hooks struct {
	// Logout revokes the Proton session of dirs, the scope's directories
	// the plan resolved (a logout step). Its failure (no answer from the
	// service) becomes the step's note: the uninstall goes on (M5-P19).
	Logout func(ctx context.Context, dirs paths.Dirs) error
	// Keyring deletes the desktop keyring item of the state directory
	// stateDir (a keyring step, ruling R13). Its failure becomes the step's
	// note, with how to delete the item by hand: the uninstall goes on.
	Keyring func(ctx context.Context, stateDir string) error
}

// StepError is the step Apply stopped at. The steps before it are done and
// a new plan keeps them.
type StepError struct {
	Index int
	Step  Step
	Err   error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %d failed (%s): %v", e.Index+1, e.Step, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }

// Apply runs steps in order and stops at the first failure, as a
// *StepError. A failed logout does not stop it: it sets that step's Note in
// steps.
func Apply(ctx context.Context, h Host, steps []Step, hooks Hooks) error {
	for i := range steps {
		err := ctx.Err()
		if err == nil {
			err = apply(ctx, h, &steps[i], hooks)
		}
		if err != nil {
			return &StepError{Index: i, Step: steps[i], Err: err}
		}
	}
	return nil
}

func apply(ctx context.Context, h Host, s *Step, hooks Hooks) error {
	switch {
	case s.Action == ActionKeep || s.Action == ActionSkip:
		return nil
	case s.Action == ActionUnknown:
		return errors.New("the plan could not inspect it: plan again as root")
	case s.Action == ActionUpdate && s.Kind == KindFile && s.fixOnly:
		return fixFile(h, *s)
	case (s.Action == ActionCreate || s.Action == ActionUpdate) && s.Kind == KindDir:
		return makeDir(h, *s)
	case (s.Action == ActionCreate || s.Action == ActionUpdate) && s.Kind == KindFile:
		return writeFile(h, *s)
	case s.Action == ActionRun && (s.Kind == KindCommand || s.Kind == KindUser):
		return runCommand(ctx, h, s.Command)
	case s.Action == ActionRun && s.Kind == KindLogout:
		// The pointer is pcb logout's (service.SessionsPage).
		const end = "end the session in the Proton web app (Settings → Security → Sessions)"
		if hooks.Logout == nil {
			s.Note = "not logged out: " + end
			return nil
		}
		if err := hooks.Logout(ctx, s.dirs); err != nil {
			s.Note = "logout failed (" + err.Error() + "): " + end
		}
		return nil
	case s.Action == ActionRemove && s.Kind == KindKeyring:
		if hooks.Keyring == nil {
			s.Note = "not removed: " + keyringByHand(h.Path(s.Path))
			return nil
		}
		if err := hooks.Keyring(ctx, h.Path(s.Path)); err != nil {
			s.Note = "not removed (" + err.Error() + "): " + keyringByHand(h.Path(s.Path))
		}
		return nil
	case s.Action == ActionRemove && s.Kind == KindFile:
		if err := os.Remove(h.Path(s.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	case s.Action == ActionRemove && s.Kind == KindDir:
		return removeDir(h, s.Path)
	}
	return fmt.Errorf("unknown step %q %q", s.Action, s.Kind)
}

func parseMode(m string) (fs.FileMode, error) {
	v, err := strconv.ParseUint(m, 8, 32)
	return fs.FileMode(v), err
}

func makeDir(h Host, s Step) error {
	p := h.Path(s.Path)
	mode := fs.FileMode(0o755)
	if s.Mode != "" {
		var err error
		if mode, err = parseMode(s.Mode); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(p, mode); err != nil {
		return err
	}
	// Through the opened directory, so a symlinked one (an admin's
	// relocated /var/lib/pcb) gets its target put right. Following the
	// link is safe: the parents, /etc and /var/lib, are root's.
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // read-only
	if fi, err := d.Stat(); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", s.Path)
	}
	if s.Mode != "" {
		if err := d.Chmod(mode); err != nil { // MkdirAll leaves an existing one, and the umask applies
			return err
		}
	}
	return chown(h, d, s.Owner)
}

// writeFile writes the step's content to a temporary file beside the
// target, with its mode and owner, then links it in place: create never
// replaces a file (config.toml appearing meanwhile is kept), update renames
// over the old one, so a running binary is replaced, not rewritten.
func writeFile(h Host, s Step) error {
	p := h.Path(s.Path)
	mode, err := parseMode(s.Mode)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".pcb-install-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) //nolint:errcheck // gone after a successful link or rename
	_, err = f.Write(s.content)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		// Through the descriptor: pcb owns /etc/pcb and could swap the
		// temporary name for a link to another file.
		err = chown(h, f, s.Owner)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if s.Action == ActionCreate {
		return createFrom(tmp, p)
	}
	return os.Rename(tmp, p)
}

// link is os.Link; tests replace it to stand for a filesystem without
// hard links.
var link = os.Link

// createFrom puts tmp in place as p, never replacing an existing p: a hard
// link, or where the filesystem has none, a check then a rename.
func createFrom(tmp, p string) error {
	err := link(tmp, p)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err
	}
	if _, lerr := os.Lstat(p); !errors.Is(lerr, fs.ErrNotExist) {
		return &fs.PathError{Op: "create", Path: p, Err: fs.ErrExist}
	}
	return os.Rename(tmp, p)
}

// fixFile puts an existing file's mode and owner right without touching
// its content (M8). Both go through one descriptor, opened on what the
// path named and checked against its Lstat: a regular file, the same one,
// with a single link. That stops a symlink or a later swap; a hard link
// pcb planted to another file is refused by the link count (where
// fs.protected_hardlinks is off), never chowned.
func fixFile(h Host, s Step) error {
	p := h.Path(s.Path)
	mode, err := parseMode(s.Mode)
	if err != nil {
		return err
	}
	before, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", s.Path)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	now, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, now) {
		return fmt.Errorf("%s changed while being fixed", s.Path)
	}
	if err := singleLink(s.Path, now); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	return chown(h, f, s.Owner)
}

// singleLink refuses a file with more than one hard link: changing it
// would change the other names too.
func singleLink(path string, fi fs.FileInfo) error {
	if n, known := linkCount(fi); known && n > 1 {
		return fmt.Errorf("%s has %d hard links: pcb changes no file linked elsewhere", path, n)
	}
	return nil
}

// chown gives the open file f to owner ("user" or "user:group", the group
// being the user's own); "" leaves it.
func chown(h Host, f *os.File, owner string) error {
	if owner == "" {
		return nil
	}
	name, _, _ := strings.Cut(owner, ":")
	uid, gid, ok, err := h.LookupUser(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no user %s", name)
	}
	return h.Chown(f, uid, gid)
}

// removeDir removes a directory and everything in it. A relative path, or
// one not named exactly pcb (M5-R5), is refused here too, whatever the
// plan said.
func removeDir(h Host, dir string) error {
	if !filepath.IsAbs(dir) || filepath.Base(dir) != removableName {
		return fmt.Errorf("refusing to remove %s", dir)
	}
	return os.RemoveAll(h.Path(dir))
}

func runCommand(ctx context.Context, h Host, command string) error {
	argv := strings.Fields(command)
	var stderr bytes.Buffer
	code, err := h.Run.Stream(ctx, io.Discard, &stderr, argv[0], argv[1:]...)
	if err != nil {
		return err
	}
	if code != 0 {
		if line := LastLine(stderr.Bytes()); line != "" {
			return fmt.Errorf("%s exited with %d: %s", argv[0], code, line)
		}
		return fmt.Errorf("%s exited with %d", argv[0], code)
	}
	return nil
}

// LastLine is the last non-empty line of out.
func LastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
