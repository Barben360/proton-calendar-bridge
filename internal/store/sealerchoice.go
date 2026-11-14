package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// sealerChoiceName is the file, beside layout.json, that says why a new
// state got the key file when [secrets] backend = auto could not use the
// desktop keyring (live 2026-11-13, F1). It is not in layout.json because
// the M7 reader refuses an unknown field there.
const sealerChoiceName = "sealer-choice.json"

// SealerChoice is why a new state was created with Sealer, and when.
type SealerChoice struct {
	Sealer string    `json:"sealer"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// WriteSealerChoice records c in stateDir (0600, atomically).
func WriteSealerChoice(stateDir string, c SealerChoice) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(stateDir, sealerChoiceName), append(raw, '\n'))
}

// ReadSealerChoice is the choice recorded in stateDir; false when there is
// none or it cannot be read (it only explains, nothing depends on it).
func ReadSealerChoice(stateDir string) (SealerChoice, bool) {
	raw, err := os.ReadFile(filepath.Join(stateDir, sealerChoiceName))
	if err != nil {
		return SealerChoice{}, false
	}
	var c SealerChoice
	if err := json.Unmarshal(raw, &c); err != nil || c.Sealer == "" {
		return SealerChoice{}, false
	}
	return c, true
}
