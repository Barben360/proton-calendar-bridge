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
)

// Hooks are what Apply cannot do on its own.
type Hooks struct {
	// Logout revokes the Proton session of the scope's directories (a
	// logout step). Its failure becomes the step's note: the uninstall goes
	// on (M5-P19).
	Logout func(ctx context.Context) error
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
	case (s.Action == ActionCreate || s.Action == ActionUpdate) && s.Kind == KindDir:
		return makeDir(h, *s)
	case (s.Action == ActionCreate || s.Action == ActionUpdate) && s.Kind == KindFile:
		return writeFile(h, *s)
	case s.Action == ActionRun && (s.Kind == KindCommand || s.Kind == KindUser):
		return runCommand(ctx, h, s.Command)
	case s.Action == ActionRun && s.Kind == KindLogout:
		if hooks.Logout == nil {
			s.Note = "not logged out: revoke the session in the Proton account settings"
			return nil
		}
		if err := hooks.Logout(ctx); err != nil {
			s.Note = "logout failed (" + err.Error() + "): revoke the session in the Proton account settings"
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
	if s.Mode != "" {
		if err := os.Chmod(p, mode); err != nil { // MkdirAll leaves an existing one, and the umask applies
			return err
		}
	}
	return chown(h, p, s.Owner)
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
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = chown(h, tmp, s.Owner)
	}
	if err != nil {
		return err
	}
	if s.Action == ActionCreate {
		return os.Link(tmp, p)
	}
	return os.Rename(tmp, p)
}

// chown gives p to owner ("user" or "user:group", the group being the
// user's own); "" leaves it.
func chown(h Host, p, owner string) error {
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
	return h.Chown(p, uid, gid)
}

// removeDir removes a directory and everything in it. A relative path, or
// one without "pcb" in its name, is refused here too, whatever the plan
// said.
func removeDir(h Host, dir string) error {
	if !filepath.IsAbs(dir) || !strings.Contains(filepath.Base(dir), "pcb") {
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
