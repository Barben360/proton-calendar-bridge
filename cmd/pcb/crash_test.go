package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// crashRig is pcb and pcb-fake built into a private root, with every pcb
// directory there and the network fenced to loopback (as
// scripts/live/rehearse.sh steps 1 to 4 set them up).
type crashRig struct {
	t        *testing.T
	pcb      string
	root     string
	env      []string
	username string
	password string
	totp     string // the otpauth URI PCB_TOTP takes
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func randomText(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newCrashRig(t *testing.T) *crashRig {
	t.Helper()
	// Not t.TempDir: the control socket's path must fit the 108 bytes of
	// sun_path.
	root, err := os.MkdirTemp("", "pcbk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	bin := filepath.Join(root, "bin")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	for name, pkg := range map[string]string{"pcb": ".", "pcb-fake": "../pcb-fake"} {
		b := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bin, exe(name)), pkg)
		b.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", name, err, out)
		}
	}
	r := &crashRig{t: t, pcb: filepath.Join(bin, exe("pcb")), root: root, username: "crash",
		password: hex.EncodeToString(randomText(t, 16))}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomText(t, 20))
	r.totp = "otpauth://totp/" + r.username + "?secret=" + secret

	// No credential, directory or proxy of the caller's reaches pcb.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "PCB_") || strings.HasSuffix(strings.ToUpper(k), "_PROXY") || strings.HasPrefix(k, "XDG_") ||
			k == "DBUS_SESSION_BUS_ADDRESS" {
			continue
		}
		r.env = append(r.env, kv)
	}
	const fence = "http://127.0.0.1:9"
	cfgDir, stateDir := filepath.Join(root, "config"), filepath.Join(root, "state")
	r.env = append(r.env, "PCB_CONFIG_DIR="+cfgDir, "PCB_STATE_DIR="+stateDir,
		"HTTPS_PROXY="+fence, "HTTP_PROXY="+fence, "ALL_PROXY="+fence, "NO_PROXY=127.0.0.1,localhost")
	for _, d := range []string{cfgDir, stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	account := filepath.Join(root, "account.toml")
	body := fmt.Sprintf("username = %q\npassword = %q\ntotp_secret = %q\n", r.username, r.password, secret)
	if err := os.WriteFile(account, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	base := r.startFake(filepath.Join(bin, exe("pcb-fake")), account)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	conf := fmt.Sprintf("[proton]\nbase_url = %q\n\n[caldav]\nlisten = \"127.0.0.1:%d\"\n\n[secrets]\nbackend = \"keyfile\"\n", base, port)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// startFake runs pcb-fake serve seeded with the test data set and returns
// its base URL once it serves.
func (r *crashRig) startFake(fake, account string) string {
	r.t.Helper()
	cmd := exec.Command(fake, "serve", "--seed", filepath.Join("..", "..", "testdata", "seed", "dataset.yaml"), "--account", account)
	cmd.Env = r.env
	out, err := cmd.StdoutPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	base := make(chan string, 1)
	go func() {
		var url string
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "Base URL: "); ok {
				url = u
			}
			if sc.Text() == "Serving until SIGINT or SIGTERM" {
				base <- url
			}
		}
		close(base)
	}()
	select {
	case u, ok := <-base:
		if !ok || !strings.HasPrefix(u, "http://127.0.0.1:") {
			r.t.Fatalf("pcb-fake did not serve on loopback (base %q)\nstderr: %s", u, errb.String())
		}
		return u
	case <-time.After(2 * time.Minute):
		r.t.Fatalf("pcb-fake not ready within 2 min\nstderr: %s", errb.String())
	}
	return ""
}

// run runs pcb args with extra environment and returns its stdout and exit
// code.
func (r *crashRig) run(extra []string, args ...string) (stdout, stderr string, code int) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.pcb, args...)
	cmd.Env = append(append([]string{}, r.env...), extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			return out.String(), errb.String(), ee.ExitCode()
		}
		r.t.Fatalf("pcb %v: %v", args, err)
	}
	return out.String(), errb.String(), 0
}

// serve starts pcb serve --wait-for-login, its output in a log file.
func (r *crashRig) serve(name string) (*exec.Cmd, string) {
	r.t.Helper()
	logPath := filepath.Join(r.root, name+".log")
	f, err := os.Create(logPath)
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command(r.pcb, "serve", "--wait-for-login")
	cmd.Env = r.env
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = f.Close() })
	return cmd, logPath
}

// health is what the test reads of pcb status --json.
type health struct {
	Data struct {
		Daemon *struct {
			PID int `json:"pid"`
		} `json:"daemon"`
		Accounts []struct {
			Health struct {
				Serve     string `json:"serve"`
				Calendars []struct {
					SyncedAt *string `json:"synced_at"`
				} `json:"calendars"`
				LastPass *struct {
					OK bool `json:"ok"`
				} `json:"last_pass"`
			} `json:"health"`
		} `json:"accounts"`
	} `json:"data"`
}

func (r *crashRig) status() (health, bool) {
	out, _, code := r.run(nil, "status", "--json")
	var h health
	if code != 0 || json.Unmarshal([]byte(out), &h) != nil || len(h.Data.Accounts) != 1 {
		return h, false
	}
	return h, true
}

// synced is rehearse.sh's predicate: serving, its 4 calendars synced, the
// last pass ok.
func synced(h health) bool {
	a := h.Data.Accounts[0].Health
	if a.Serve != "serving" || len(a.Calendars) != 4 || a.LastPass == nil || !a.LastPass.OK {
		return false
	}
	for _, c := range a.Calendars {
		if c.SyncedAt == nil {
			return false
		}
	}
	return true
}

// until polls f every 50 ms for at most d.
func until(d time.Duration, f func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if f() {
			return true
		}
	}
	return false
}

func readLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// Spec §5.11: a serving process killed at once (TerminateProcess on
// Windows, SIGKILL elsewhere), mid-sync when the test catches it there,
// leaves a state the next pcb serve starts from cleanly: atomically
// replaced files, a SQLite cache in WAL mode, locks and the control
// channel released with the process.
func TestKilledServeRestartsClean(t *testing.T) {
	if testing.Short() {
		t.Skip("builds pcb and pcb-fake, runs a sync")
	}
	r := newCrashRig(t)

	first, firstLog := r.serve("serve1")
	if !until(30*time.Second, func() bool { return strings.Contains(readLog(firstLog), "Waiting for a login: run pcb login") }) {
		t.Fatalf("pcb serve not waiting for a login within 30 s:\n%s", readLog(firstLog))
	}
	if out, errs, code := r.run([]string{"PCB_USERNAME=" + r.username, "PCB_PASSWORD=" + r.password, "PCB_TOTP=" + r.totp},
		"login", "--non-interactive"); code != 0 {
		t.Fatalf("pcb login exited %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	// Kill as soon as the account serves and its calendars are in the
	// cache: its first pass is usually still writing their events then.
	var at health
	if !until(60*time.Second, func() bool {
		h, ok := r.status()
		if ok && h.Data.Accounts[0].Health.Serve == "serving" && len(h.Data.Accounts[0].Health.Calendars) > 0 {
			at = h
			return true
		}
		return false
	}) {
		t.Fatalf("the account never served within 60 s:\n%s", readLog(firstLog))
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	n, cals := 0, at.Data.Accounts[0].Health.Calendars
	for _, c := range cals {
		if c.SyncedAt != nil {
			n++
		}
	}
	if synced(at) {
		t.Logf("killed after the first pass: %d of %d calendars synced", n, len(cals))
	} else {
		t.Logf("killed mid-sync: %d of %d calendars synced", n, len(cals))
	}

	_, secondLog := r.serve("serve2")
	var last health
	if !until(60*time.Second, func() bool {
		h, ok := r.status()
		last = h
		return ok && synced(h)
	}) {
		t.Fatalf("the restarted pcb serve did not sync within 60 s (last status %+v):\n%s", last, readLog(secondLog))
	}
	out, errs, _ := r.run(nil, "doctor", "--json")
	var doc struct {
		Data struct {
			Results []struct {
				Name    string `json:"name"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("pcb doctor --json: %v\nstdout: %s\nstderr: %s", err, out, errs)
	}
	found := false
	for _, res := range doc.Data.Results {
		if strings.HasSuffix(res.Name, "cache") {
			found = true
			if res.Status != "ok" {
				t.Errorf("doctor %s = %s (%s), want ok", res.Name, res.Status, res.Message)
			}
		}
	}
	if !found {
		t.Errorf("pcb doctor --json has no cache result:\n%s", out)
	}
}
