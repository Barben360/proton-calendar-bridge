package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

// AliveWindow is how long `probe alive` keeps sampling (E01's long probe).
const AliveWindow = 14 * 24 * time.Hour

const aliveFile = "e01.jsonl"

// Refresh is E01's immediate probe: refresh once, save the new tokens, retry
// once with the old refresh token. It records both exchanges when o.Recorder
// is set. Proton may end the whole session when an old refresh token is
// replayed, so run it after the other probes that need the session.
func Refresh(ctx context.Context, o session.Options) (session.RefreshResult, error) {
	res, err := session.Refresh(ctx, o)
	if err != nil {
		return res, err
	}
	if o.Recorder != nil {
		if err := save(o.Recorder, "refresh", "first", http.MethodPost, "/auth/v4/refresh", res.First); err != nil {
			return res, err
		}
		if res.Retried {
			err = save(o.Recorder, "refresh", "old-token", http.MethodPost, "/auth/v4/refresh", res.Retry)
		}
	}
	return res, err
}

// AliveResult is one `probe alive` sample, as appended to e01.jsonl.
type AliveResult struct {
	At        string `json:"at"`
	OK        bool   `json:"ok"`
	Code      int    `json:"code"`
	Refreshed bool   `json:"refreshed"`  // the access token was rejected and a refresh succeeded
	Rotated   bool   `json:"rotated"`    // that refresh returned a new refresh token
	ExpiresIn int    `json:"expires_in"` // as returned by that refresh, 0 when absent
	Over      bool   `json:"-"`          // the window is over: nothing was sampled
}

// Alive is E01's long probe, on its own dedicated session (its own state
// dir, used by nothing else). It makes one authenticated GET with the saved
// access token; on a 401 it refreshes once, saves the new tokens and repeats
// the GET. It appends {at, ok, code, refreshed, rotated, expires_in} to
// StateDir/e01.jsonl. A refused refresh is the sample (ok false, the refresh
// code): that is how the session ended. Once the first sample is older than
// AliveWindow it samples nothing and reports Over.
func Alive(ctx context.Context, o session.Options) (AliveResult, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	path := filepath.Join(o.StateDir, aliveFile)
	first, err := firstSample(path)
	if err != nil {
		return AliveResult{}, err
	}
	if !first.IsZero() && now().Sub(first) > AliveWindow {
		return AliveResult{Over: true}, nil
	}
	res := AliveResult{At: now().UTC().Format(time.RFC3339)}
	r, err := session.Get(ctx, o, "/core/v4/users")
	if err != nil {
		return res, err
	}
	if r.Status == http.StatusUnauthorized {
		rr, err := session.RefreshSaved(ctx, o)
		if err != nil {
			return res, err
		}
		if rr.First.Status != http.StatusOK {
			r = rr.First
		} else {
			res.Refreshed, res.Rotated, res.ExpiresIn = true, rr.RefreshRotated, rr.ExpiresIn
			if r, err = session.Get(ctx, o, "/core/v4/users"); err != nil {
				return res, err
			}
		}
	}
	res.OK, res.Code = r.OK(), r.Code
	if r.Code == 0 { // not a Proton answer: keep the HTTP status visible
		res.Code = r.Status
	}
	line, _ := json.Marshal(res)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return res, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return res, err
	}
	return res, f.Close()
}

// firstSample is the time of the first line of e01.jsonl, zero if none.
func firstSample(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	line, _, _ := bufio.NewReader(bytes.NewReader(b)).ReadLine()
	var s struct{ At string }
	if err := json.Unmarshal(line, &s); err != nil {
		return time.Time{}, fmt.Errorf("%s: first line is not a sample: %w", aliveFile, err)
	}
	return time.Parse(time.RFC3339, s.At)
}
