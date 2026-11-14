package doctor_test

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

func runCache(dir string) doctor.Result {
	return doctor.CacheCheck{StateDir: dir}.Run(context.Background())
}

func TestCacheCheck(t *testing.T) {
	dir := t.TempDir()
	if r := runCache(dir); r.Status != doctor.StatusOK || r.Message != "no cache yet" {
		t.Errorf("absent: %+v", r)
	}
	path := cache.Path(dir)
	c, err := cache.Open(path, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyDelta(context.Background(), "cal", "c", nil, nil, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if r := runCache(dir); r.Status != doctor.StatusOK {
		t.Errorf("healthy: %+v", r)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if r := runCache(dir); r.Status != doctor.StatusError || !strings.Contains(r.Message, "644") || !strings.Contains(r.Fix, "chmod 600") {
			t.Errorf("mode: %+v", r)
		}
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		if r := runCache(dir); r.Status != doctor.StatusOK {
			t.Errorf("0400 is no wider than 0600: %+v", r)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(path, []byte(strings.Repeat("not a sqlite database ", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	if r := runCache(dir); r.Status != doctor.StatusError || !strings.Contains(r.Fix, "pcb sync --full") {
		t.Errorf("damaged: %+v", r)
	}
}

func TestCacheCheckUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	path := cache.Path(dir)
	c, err := cache.Open(path, "a")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 42`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if r := runCache(dir); r.Status != doctor.StatusError || !strings.Contains(r.Message, "42") || !strings.Contains(r.Fix, "upgrade pcb") {
		t.Errorf("schema: %+v", r)
	}
}

func setVersion(t *testing.T, path string, v int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`PRAGMA user_version = ` + strconv.Itoa(v)); err != nil {
		t.Fatal(err)
	}
}

func TestCacheCheckAcceptsAnM3File(t *testing.T) {
	dir := t.TempDir()
	path := cache.Path(dir)
	c, err := cache.Open(path, "a")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	setVersion(t, path, 1)
	if r := runCache(dir); r.Status != doctor.StatusOK || !strings.Contains(r.Message, "schema 1") {
		t.Errorf("version 1: %+v", r)
	}
}

func TestCacheCheckRefusesAVersionZeroFile(t *testing.T) {
	dir := t.TempDir()
	path := cache.Path(dir)
	c, err := cache.Open(path, "a")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	setVersion(t, path, 0)
	if r := runCache(dir); r.Status != doctor.StatusError || !strings.Contains(r.Message, "unknown schema version 0") {
		t.Errorf("version 0: %+v", r)
	}
}
