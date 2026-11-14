package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

const realDataset = "../../testdata/seed/dataset.yaml"

func TestSeedPlanIsOfflineAndSkipsExisting(t *testing.T) {
	t.Setenv("PATH", "") // a plan that tried to run proton-cal could not find it
	code, env := run(t, "seed", "plan", "--dataset", realDataset)
	if code != exitcode.OK || env["command"] != "seed plan" {
		t.Fatalf("code %d, envelope %v", code, env)
	}
	data := env["data"].(map[string]any)
	total := int(data["total"].(float64))
	if total < 350 || len(data["commands"].([]any)) != total {
		t.Fatalf("total %d, want every create plus the 16 series edits and deletes", total)
	}

	listing := filepath.Join(t.TempDir(), "events.json")
	if err := os.WriteFile(listing, []byte(`[{"id":"EV42","summary":"pcb-seed s042 Run","start":"2021-01-01T10:00:00Z"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, env = run(t, "seed", "plan", "--dataset", realDataset, "--existing", listing)
	if got := int(env["data"].(map[string]any)["total"].(float64)); got != total-1 {
		t.Errorf("with s042 existing: %d commands, want %d", got, total-1)
	}
}

func TestSeedPlanRejectsMissingDataset(t *testing.T) {
	code, env := run(t, "seed", "plan", "--dataset", filepath.Join(t.TempDir(), "nope.yaml"))
	if code != exitcode.Usage || env["ok"] != false {
		t.Fatalf("code %d, envelope %v, want a usage error", code, env)
	}
}

// fakeBin is a proton-cal stand-in that answers calendars, lists no events and
// fails every write with a 429.
func fakeBin(t *testing.T, writeFails bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake proton-cal is a shell script")
	}
	write := `echo '{"id":"EV1"}'`
	if writeFails {
		write = `echo "Error: API error 429: Too Many Requests" >&2; exit 1`
	}
	body := `case "$1" in
calendars) echo '[{"id":"C1","name":"Main","is_default":true},{"id":"C2","name":"Work","is_default":false},{"id":"C3","name":"Sport","is_default":false}]';;
events) echo '[]';;
*) ` + write + `;;
esac`
	p := filepath.Join(t.TempDir(), "proton-cal")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSeedApplyStopsOnRateLimitWithResumeHint(t *testing.T) {
	code, env := run(t, "seed", "apply", "--dataset", realDataset, "--bin", fakeBin(t, true), "--interval", "0s", "--config-dir", t.TempDir())
	if code != exitcode.Partial || env["ok"] != false {
		t.Fatalf("code %d, envelope %v, want 40 (partly done, resumable)", code, env)
	}
	if msg := env["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "rate limited") || !strings.Contains(msg, "s001") {
		t.Errorf("message %q should say rate limited and name the command it stopped on", msg)
	}
}

func TestSeedApplyWithoutLoginIsBlocked(t *testing.T) {
	code, _ := run(t, "seed", "apply", "--dataset", realDataset, "--bin", filepath.Join(t.TempDir(), "missing"), "--interval", "0s", "--config-dir", t.TempDir())
	if code != exitcode.Blocked {
		t.Fatalf("code %d, want Blocked when proton-cal cannot run", code)
	}
}

func TestSeedApplyListingRateLimitIsResumable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := filepath.Join(t.TempDir(), "proton-cal")
	script := "#!/bin/sh\necho 'Error: proton api error: status=429 code=0: Too Many Requests' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	code, env := run(t, "seed", "apply", "--dataset", realDataset, "--bin", bin, "--interval", "0s", "--config-dir", t.TempDir())
	if code != exitcode.Partial || env["ok"] != false {
		t.Fatalf("code %d, envelope %v, want 40 with a resume hint", code, env)
	}
}
