package update_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

var t0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func storePath(t *testing.T) string {
	t.Helper()
	return update.StorePath(t.TempDir())
}

func TestEditDoesNotCreateTheStateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := update.Edit(context.Background(), update.StorePath(dir), func(s *update.State) { s.Off = true }); err == nil {
		t.Error("Edit succeeded without the state directory")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state directory: %v, want it still absent", err)
	}
}

func TestStorePath(t *testing.T) {
	if got := update.StorePath("/s"); got != filepath.Join("/s", "update.json") {
		t.Errorf("StorePath = %q", got)
	}
}

func TestLoadMissingOrCorruptIsZero(t *testing.T) {
	p := storePath(t)
	if st := update.Load(p); st != (update.State{}) {
		t.Errorf("missing: %+v", st)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := update.Load(p); st != (update.State{}) {
		t.Errorf("corrupt: %+v", st)
	}
}

func TestEditRoundTrip(t *testing.T) {
	p := storePath(t)
	if err := update.Edit(context.Background(), p, func(s *update.State) { s.Off, s.Latest, s.CheckedAt = true, "v0.2.0", t0 }); err != nil {
		t.Fatal(err)
	}
	st := update.Load(p)
	if !st.Off || st.Latest != "v0.2.0" || !st.CheckedAt.Equal(t0) {
		t.Errorf("Load = %+v", st)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", info.Mode().Perm())
		}
	}
}

// A cache refresh racing a switch flip keeps the flip (review gap 4): the
// second Edit waits for the first, held open inside its function, instead
// of reading the file before the first writes it.
func TestEditWaitsForAnotherWriter(t *testing.T) {
	p := storePath(t)
	inside, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- update.Edit(context.Background(), p, func(s *update.State) {
			close(inside)
			<-release
			s.Latest, s.CheckedAt = "v0.2.0", t0
		})
	}()
	<-inside
	second := make(chan error, 1)
	go func() { second <- update.Edit(context.Background(), p, func(s *update.State) { s.Off = true }) }()
	select {
	case err := <-second:
		t.Fatalf("second Edit ended (%v) while the first held the file", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if st := update.Load(p); !st.Off || st.Latest != "v0.2.0" {
		t.Errorf("Load = %+v, want both writes kept", st)
	}
}

func TestEnabled(t *testing.T) {
	for _, c := range []struct {
		env  string
		off  bool
		want bool
	}{
		{"", false, true}, {"on", false, true}, {"off", false, false}, {"OFF", false, false}, {" off ", false, false},
		{"", true, false}, {"on", true, false},
	} {
		env := func(k string) string {
			if k == "PCB_UPDATE_CHECK" {
				return c.env
			}
			return ""
		}
		if got := update.Enabled(env, update.State{Off: c.off}); got != c.want {
			t.Errorf("Enabled(env %q, off %v) = %v, want %v", c.env, c.off, got, c.want)
		}
	}
}

func TestCachedKeepsTheAnswerForADay(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	c, p := r.checker("v0.1.0"), storePath(t)
	res := c.Cached(context.Background(), p, t0)
	if res.Err != nil || !res.Available || res.Latest.Version != "v0.2.0" || res.Latest.URL != r.srv.URL+tagPath+"v0.2.0" || !res.CheckedAt.Equal(t0) {
		t.Fatalf("first = %+v", res)
	}
	if res := c.Cached(context.Background(), p, t0.Add(24*time.Hour-time.Second)); !res.Available || r.n.Load() != 1 {
		t.Errorf("within 24 h: %+v, %d requests, want 1", res, r.n.Load())
	}
	c.Cached(context.Background(), p, t0.Add(24*time.Hour+time.Second))
	if r.n.Load() != 2 {
		t.Errorf("after 24 h: %d requests, want 2", r.n.Load())
	}
}

func TestCachedComparesWithTheRunningVersion(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	p := storePath(t)
	r.checker("v0.1.0").Cached(context.Background(), p, t0)
	if res := r.checker("v0.2.0").Cached(context.Background(), p, t0.Add(time.Minute)); res.Available {
		t.Errorf("after the update: %+v, want not available", res)
	}
}

func TestCachedRechecksAFutureCheckTime(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	c, p := r.checker("v0.1.0"), storePath(t)
	c.Cached(context.Background(), p, t0)
	c.Cached(context.Background(), p, t0.Add(-time.Hour)) // the clock went back
	if r.n.Load() != 2 {
		t.Errorf("%d requests, want 2", r.n.Load())
	}
}

func TestCachedDoesNotKeepAFailure(t *testing.T) {
	r := newReleases(t, http.StatusNotFound, "")
	c, p := r.checker("v0.1.0"), storePath(t)
	if res := c.Cached(context.Background(), p, t0); res.Err == nil || res.Available {
		t.Errorf("failed check: %+v", res)
	}
	c.Cached(context.Background(), p, t0.Add(time.Minute))
	if r.n.Load() != 2 {
		t.Errorf("%d requests, want 2 (a failure is retried)", r.n.Load())
	}
}

func TestCachedDevBuildSendsNothing(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	if res := r.checker("dev").Cached(context.Background(), storePath(t), t0); !errors.Is(res.Err, update.ErrDevBuild) || r.n.Load() != 0 {
		t.Errorf("dev: %+v, %d requests", res, r.n.Load())
	}
}

func TestCachedWithAnUnwritableStateStillAnswers(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test user cannot write")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	res := r.checker("v0.1.0").Cached(context.Background(), update.StorePath(dir), t0)
	if res.Err != nil || !res.Available {
		t.Errorf("unwritable state: %+v", res)
	}
}

// update.json is data from disk: a cached page on another host is not
// opened, the release is asked again (review finding 2).
func TestCachedDoesNotTrustAnotherHostOnDisk(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	c, p := r.checker("v0.1.0"), storePath(t)
	if err := update.Edit(context.Background(), p, func(s *update.State) {
		s.CheckedAt, s.Latest, s.URL = t0, "v0.2.0", "https://evil.example"+tagPath+"v0.2.0"
	}); err != nil {
		t.Fatal(err)
	}
	res := c.Cached(context.Background(), p, t0.Add(time.Minute))
	if res.Latest.URL != r.srv.URL+tagPath+"v0.2.0" || r.n.Load() != 1 {
		t.Errorf("Cached = %+v after %d requests, want GitHub asked again", res, r.n.Load())
	}
}

// A damaged cache field does not drop the switch: "off" stays off, and the
// next write keeps it (docs/security.md: off means no call).
func TestLoadKeepsTheSwitchWhenAnotherFieldIsBad(t *testing.T) {
	p := storePath(t)
	if err := os.WriteFile(p, []byte(`{"off":true,"checked_at":"not a time","latest":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := update.Load(p); !st.Off || !st.CheckedAt.IsZero() || st.Latest != "" {
		t.Errorf("Load = %+v, want off kept, the bad fields zero", st)
	}
	if err := update.Edit(context.Background(), p, func(s *update.State) { s.Latest, s.CheckedAt = "v0.2.0", t0 }); err != nil {
		t.Fatal(err)
	}
	if st := update.Load(p); !st.Off || st.Latest != "v0.2.0" {
		t.Errorf("after a cache write: %+v, want off kept", st)
	}
}
