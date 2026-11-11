package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestLdflagsStampReachesVersion builds pcb with the -X flags the Makefile
// uses and checks that `pcb version --json` reports them.
func TestLdflagsStampReachesVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const pkg = "github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	bin := filepath.Join(t.TempDir(), "pcb")
	flags := "-X " + pkg + ".Version=v9.9.9 -X " + pkg + ".Commit=abc1234 -X " + pkg + ".Date=2026-01-02T03:04:05Z"
	if out, err := exec.CommandContext(ctx, "go", "build", "-ldflags", flags, "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(ctx, bin, "version", "--json").Output()
	if err != nil {
		t.Fatalf("pcb version --json: %v", err)
	}
	var env struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if env.Data["version"] != "v9.9.9" || env.Data["commit"] != "abc1234" || env.Data["date"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("data = %v, want the stamped values", env.Data)
	}
}
