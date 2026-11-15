package service_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// On Windows a pcb sync of another process holds the sync lock and pcb.db
// open: the logout cannot remove the cache, yet it must finish (state reset,
// exit 0) and say a sync is running, as it does on Unix.
func TestLogoutWithAnotherProcessHoldingTheCacheOpen(t *testing.T) {
	e := newEnv(t, protontest.Options{})
	e.signIn(t)
	if _, err := e.svc.Sync(t.Context(), service.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	st, err := store.LoadState(e.dirs.State)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Lock(t.Context(), filepath.Join(e.dirs.State, "sync.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	db, err := cache.Open(cache.Path(e.dirs.State), st.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := cache.RemoveRetry
	cache.RemoveRetry = 100 * time.Millisecond
	defer func() { cache.RemoveRetry = old }()

	res, err := e.svc.Logout(t.Context(), service.LogoutRequest{})
	if err != nil || !res.WasLoggedIn || !res.SyncRunning {
		t.Fatalf("Logout = %+v, %v; want success with SyncRunning", res, err)
	}
	if after, err := store.LoadState(e.dirs.State); err != nil || after.Phase != store.PhaseLoggedOut {
		t.Fatalf("state after the logout: %+v, %v; want logged out", after, err)
	}
}
