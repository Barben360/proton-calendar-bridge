package doctor

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// SecretsCheck reports how the secrets are protected. Only the key-file
// sealer exists so far, so it always warns: a 0600 key file stops other
// local users and casual disclosure, not root or the user's own processes.
type SecretsCheck struct {
	StateDir string // holds state.json and secrets.sealed
	KeyFile  string
}

// Name implements Check.
func (SecretsCheck) Name() string { return "secrets" }

// Run implements Check.
func (c SecretsCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusWarn}
	state, err := store.LoadState(c.StateDir)
	if err != nil {
		r.Status, r.Message = StatusError, err.Error()
		r.Fix = "remove the damaged state.json, then log in again"
		return r
	}
	msgs := []string{"secrets sealed with a key file (headless fallback)"}
	if inside(c.StateDir, c.KeyFile) {
		msgs = append(msgs, "the key file is in the same directory tree as the sealed secrets, which defeats sealing; set [secrets] key_file to another path or volume")
	}
	if state.Unattended {
		msgs = append(msgs, "unattended relogin is on: whoever can read "+c.KeyFile+" and "+
			filepath.Join(c.StateDir, "secrets.sealed")+" owns the Proton account")
	}
	r.Message = strings.Join(msgs, "; ")
	return r
}

// inside reports whether path is dir or below it.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
