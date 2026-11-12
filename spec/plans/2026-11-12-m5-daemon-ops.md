# M5 Daemon and Ops Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pcb serve` becomes a daemon that a fresh Debian host and a container can each set up from the terminal only. The CLI reaches it over a private control socket (CTL). `pcb status` and `pcb doctor` show its live state and alerts. Logins go through it, including a 2FA code given later. It waits for a login instead of exiting. It syncs every 30 s, or every 5 min when no client is around. `pcb install`, `pcb uninstall` and `pcb service` drive systemd. A distroless image with a smoke test is pushed to a private GHCR package. Three M1 carry-overs close: R3m (rollback of the sealed secrets), R8m (a TTL on the pending login) and R10m (a state-file lock). Everything is proven offline: the fake Proton, golden unit files, a container smoke test and a systemd smoke test in CI. The live run is a deferred human check on the test account.

**Architecture:**
- **C3 `internal/service`:** gains `Health` (live Serve state: cadence, per-calendar sync, error class, write gate and reason, render counters, human verification) and the idle cadence. Serve gains a wait-for-login mode. The pending 2FA login gets a 5-minute TTL. Logout stops Serve's loop first.
- **C2 `internal/store`:** gains the sealed generation counter and its mirror in `state.json` (R3m), and `UpdateState` under a leaf lock (R10m).
- **CTL `internal/ctl` (new):** a JSON-over-HTTP server on `<state>/pcb.sock` (0600, peer UID on Linux) and its client. It holds the wire types and the pure alert rules, and imports only `service` and the CLI helpers.
- **C6 `internal/cli` serve:** binds CTL and the CalDAV listener, publishes the listener info, takes `--wait-for-login` and `--log-level`, and lets a second signal force the exit.
- **Ops `internal/install` (new):** pure unit and config renderers (golden), a step planner and applier over an injectable `Host` (path root, command runner, euid, chown), and systemd scope detection. The CLI adds `install`, `uninstall` and `service`.
- **Packaging:** a `Dockerfile` (golang builder → distroless static nonroot), `scripts/container-smoke.sh`, `scripts/systemd-smoke.sh`, `.github/workflows/image.yml`, and a systemd job in `ci.yml`.

**Tech Stack:** as M4. Stdlib `syscall.GetsockoptUcred` for SO_PEERCRED (Linux). systemd ≥ 252 (Debian 12/13). Docker for the image and both smoke tests. No new Go dependency.

**Spec:** `spec/SPEC.md` §4 (C6, CTL), §5.2, §7.1, §7.3, §8.1, §13, §14, §15, §16, §18 ("M5 carry-overs", M5 row), §19; decisions 0001, 0009, 0010; `spec/OPEN.md` "For M5" (Parked by M3 and by M4); `spec/experiments/E08-thunderbird-caldav.md` §3.7 (Thunderbird's 30-minute refresh); `spec/experiments/raw/m2-feed-research.md` (caloxide's standby idea). No new Proton call.

**Out of scope (YAGNI, say so in review):**
- The §15 Prometheus endpoint (M5-Rpre11: status over CTL covers it).
- `pcb config get|set|validate` (§13): editing `config.toml` and `pcb doctor` suffice for the "done when".
- Plain HTTP on a non-loopback bind behind a reverse proxy (OPEN "For M5"): the image serves self-signed TLS.
- systemd-creds and Secret Service sealing (§7.3): the key file stays the only sealer.
- Placeholder reason categories, address-key rotation without a restart, and binding each part's author to its key (OPEN "For M5": they need E10 or the final audit).
- Persisting the write gate's last decision (M5-P13).
- Signed releases and `install.sh` (§13, "later, when public").
- Running more than one account, and any Windows service.
- Any change to how pcb talks to Proton.

## Global Constraints

- Everything in the M1 to M4 Global Constraints still holds:
  - the `--json` envelope `{schema_version, command, ok, data | error}`;
  - exit codes `0` done, `2` bad invocation, `10` you must act, `20` blocked, `30` rolled back, `40` partly done and resumable;
  - `--non-interactive` never prompts;
  - secrets never in argv, logs, errors, responses or unsealed files;
  - `internal/cli` and the adapters call only `service.API`, and never import `internal/proton`, `internal/store` or go-proton-api (`arch_test.go`);
  - every `service.API` error is an `*exitcode.Error` with a secret-free message and a hint;
  - nothing decrypted on disk;
  - tests bite: assert the observable outcome, and the reviewer checks by sabotage (no mutation testing);
  - live checks only on the test account and only as deferred human-check steps, never morpheus (production, read-only commands only) and never the author's own account;
  - `make lint test` is the gate, and `GOOS=windows go vet ./...` stays clean.
- **CTL (M5-Rpre1):**
  - The socket is `<state>/pcb.sock`, mode 0600. Its directory must grant group and other nothing, else `pcb serve` exits 20.
  - A path over 107 bytes is refused (exit 20, hint `set PCB_STATE_DIR to a shorter path`).
  - The API is HTTP/1.1 under `/v1/`, internal and versioned. Each answer is one `output.Envelope`: 200 for ok, 422 for a command error (its exit code, message and hint), 400, 403, 404, 405 or 413 for a transport error.
  - Request bodies are at most 64 KiB. Bodies are never logged.
  - Client timeouts: 2 s to dial, 10 s for status, 180 s for login.
  - On Linux the peer UID (SO_PEERCRED) must equal the daemon's. Other Unix rely on the file mode. Off Unix there is no CTL and the CLI is always offline.
  - Tests put sockets in `os.MkdirTemp("", "pcb")`: a `t.TempDir()` path can exceed the 108-byte `sun_path`.
- **Paths (M5-P1):** `PCB_CONFIG_DIR` and `PCB_STATE_DIR`, when set, are used verbatim (no `/pcb` suffix) and must be absolute; a relative value is refused, naming the variable. XDG rules apply otherwise. System install: `/etc/pcb`, `/var/lib/pcb`. Container: `/data/config`, `/data/state`.
- **Cadence (M5-Rpre9, M5-P6):**
  - `ActiveInterval` 30 s, `IdleInterval` 5 min, `IdleAfter` 35 min. Idle means no request authenticated by `CheckClient` for 35 min since Serve began serving. 35 min is longer than Thunderbird's default 30-minute refresh, so a running Thunderbird keeps the 30 s cadence (E08 §3.7).
  - The first authenticated request while idle starts a pass at once. A wake never shortens a back-off or a `Retry-After` wait.
  - Back-off is unchanged (×2 up to 10 min, ±10 %).
- **Alerts (M5-Rpre2, M5-P7):** `{code, severity: error|warning, message, fix, calendar?}`. Codes are stable: `needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`, `calendar-keys-failed`, `write-gate-undecided`, `tls-certificate-expiring`, `tls-certificate-expired`. The order is errors first, then code, then calendar ID. Exact rules are in Task 3.
- **Waiting for login (M5-Rpre3, M5-P8, M5-P9):**
  - While waiting, an authenticated CalDAV request gets 503 with `Retry-After: 30`. Authentication still comes first: no credentials get 401.
  - A login is detected by reading `state.json` every 5 s, and at once when it completes in the daemon.
- **Login through the daemon (M5-Rpre4):**
  - The pending 2FA login lives 5 min, after which it is revoked and its password zeroed.
  - The 2FA code reaches `pcb login resume --totp` from `PCB_TOTP` or the prompt, never argv (M5-P11).
- **Logging (M5-Rpre8):** `[log] level` and `pcb serve --log-level` take `debug|info|warn|error`. The default is `info`, and the flag overrides the config.
- **Units (M5-Rpre5, M5-P17, M5-P18):** contents are exactly as in Task 7, golden-tested. Every `systemctl`, `journalctl`, `useradd`, `userdel` and `loginctl` call goes through `install.Runner`.
- **Image (M5-Rpre7, M5-P20):**
  - The builder is `golang:1.27-trixie` and the runtime `gcr.io/distroless/static-debian13:nonroot` (UID 65532), both pinned by digest with the tag in a comment.
  - `CGO_ENABLED=0`.
  - `PCB_CONFIG_DIR=/data/config`, `PCB_STATE_DIR=/data/state`, `VOLUME /data`, `EXPOSE 1080`.
  - Entrypoint `pcb`, command `serve --wait-for-login --listen 0.0.0.0:1080`.
  - Exec-form HEALTHCHECK `pcb status --json --daemon`.
- Workflow files are pushed over SSH (`git@github.com:`): the gh token lacks the `workflow` scope. Pin actions by commit SHA, as `ci.yml` does, and add no third-party action: plain `docker` CLI.
- No new runtime dependency. Never copy code from the reference bridges, and never read or fetch ProtonMail/WebClients (GPL). Ideas taken get a credit in `README.md`: caloxide's standby slow-down, and the Proton Mail Bridge container model.
- Commits: `<gitmoji> <imperative>` subject, body if useful, and the trailers:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01XrqMmZwerq6VPogF95MuRx
  ```

## Rulings

From the controller (binding; not to re-ask):
- **M5-Rpre1 CTL:** JSON over HTTP on a unix socket, 0600 in a 0700 directory, SO_PEERCRED on Linux, `/v1/`, internal. `pcb serve` serves it; the CLI uses a client package; with no daemon, commands fall back offline and say so.
- **M5-Rpre2 status:** `pcb status` and `pcb doctor` show live state over CTL: login, per-calendar sync (last success, lag, error class), listeners and Thunderbird URL, render counters, per-calendar write gate with reason, and alerts. `--json` uses the envelope.
- **M5-Rpre3 waiting:** `pcb serve --wait-for-login`, the default of the units and the image. Without the flag, M3 behaviour stays (exit 10 logged out, Proton's code at session end).
- **M5-Rpre4 login via the daemon (R16, R8m):** the daemon holds the pending login for 5 min; TTL expiry is tested; secrets travel only in request bodies.
- **M5-Rpre5 install:**
  - `--user` is the default and needs no root. It prints the linger instruction.
  - `--system` needs root. It creates the `pcb` user and uses `/etc/pcb` and `/var/lib/pcb`.
  - Both units are hardened.
  - `--plan` changes nothing. The command is idempotent.
  - `uninstall [--delete-data] [--plan]`; `--delete-data` needs confirmation or `--yes`.
  - Golden tests; an injectable runner.
- **M5-Rpre6 service:** `pcb service start|stop|restart|status|logs [-f]` over systemctl and journalctl, user or system from what is installed.
- **M5-Rpre7 container:**
  - A multi-stage image, non-root, with one volume `/data`.
  - Self-signed TLS on 1080.
  - A HEALTHCHECK over CTL; login through `docker exec -it`.
  - A workflow builds on PRs and pushes to private GHCR on main.
  - A local and CI smoke test.
- **M5-Rpre8 logging:** `[log] level`, `--log-level`.
- **M5-Rpre9 idle slow-down:** 30 s, 5 min when idle.
- **M5-Rpre10 carry-overs:**
  - R3m: a sealed generation, mirrored in `state.json`.
  - R10m: a leaf lock on every `state.json` writer.
  - The M4 note on undecided gates.
  - The OPEN.md "For M5" items: gate-undecided naming, human-verification status, log level, and the per-process gate decision.
- **M5-Rpre11:** Prometheus deferred.
- **M5-Rpre12 done when:**
  - The container smoke test and the systemd smoke test (a Debian container with systemd as PID 1) run in CI.
  - The live run is a HUMAN-CHECKS step on the test account only.

Made in this plan (record them in the SDD ledger as such):
- **M5-P1** `PCB_CONFIG_DIR` and `PCB_STATE_DIR`: `internal/paths` had no way to reach `/etc/pcb`, `/var/lib/pcb` or `/data`.
- **M5-P2** The socket stays in the state directory, because `paths` defines no runtime directory. A stale socket is replaced without a probe: `pcb serve` holds `sync.lock` before binding, so no other daemon owns it.
- **M5-P3** Peer check:
  - Linux allows the same UID only, root included in the refusal (root uses its own state directory or `sudo -u`).
  - Other Unix rely on the mode.
  - Windows has no CTL (`pcb serve` cannot run there anyway: no file locks).
- **M5-P4** The status JSON keeps `service.Status`'s fields flat at the top level, so the M1–M4 consumers keep working. It adds `daemon` (null offline), `health`, `listener` and `alerts`.
- **M5-P5** `pcb status --daemon` exits 20 when no daemon answers. The HEALTHCHECK needs this, since plain `status` falls back offline. `status` exits 0 whatever the alerts.
- **M5-P6** Idle is defined as above. §8.1 names the 5 min but never defines idle.
- **M5-P7** Separate codes `needs-captcha` (login) and `human-verification` (API 9001), because their fixes differ. `sync-stalled` is per calendar: now − max(last success, serving since) > 5 × the current interval.
- **M5-P8** Authentication before the waiting 503: no state is revealed to an unauthenticated peer, and the CalDAV adapter's order is unchanged.
- **M5-P9** Login detection while waiting: a 5 s poll of `state.json` (another process's `pcb login`) plus an in-process wake.
- **M5-P10** `Logout` during Serve stops the sync loop and closes the cache before the wipe, so no pass writes to a removed `pcb.db`.
- **M5-P11** `pcb login resume --totp` is a boolean, and the code comes from `PCB_TOTP` or the prompt. Rpre4 and §13 write `--totp CODE`, but a code in argv breaks "secrets never in argv".
- **M5-P12** `login resume --captcha-token` through the daemon gathers the credentials again, as today. Only the post-SRP 2FA login is held.
- **M5-P13** The write gate's last decision is not persisted (M4-R40 stays). A stored verdict could advertise a revoked Write bit. The renderer's in-memory `decided` map already survives login waits within the process.
- **M5-P14** R3m format:
  - `secrets.sealed` version 2 seals `{generation, accounts}` under AAD `pcb-secrets/v2/<sealer>`. A v1 file reads as generation 0.
  - A sealed file ahead of the mirror is accepted (a crash between the two writes).
  - A wipe bumps the mirror, so a file removed by logout is refused if restored.
  - Restoring the whole state directory is undetectable, a documented residual.
- **M5-P15** R10m: `state.lock` through `filelock`. Off Unix it is an in-process mutex, as `clients.json` (M3-R18).
- **M5-P16** `--system` copies the running binary to `/usr/local/bin/pcb`, root 0755, rather than running a user-writable one as `pcb`. `uninstall` leaves that binary and prints the `rm` line. `--user` runs `os.Executable()`.
- **M5-P17** The user unit carries only the hardening that needs no user namespace, so it starts under any user manager. The system unit carries the full set (Task 7).
- **M5-P18** Each unit sets `PCB_CONFIG_DIR` and `PCB_STATE_DIR` explicitly. The user unit takes the directories resolved at install time, since the user manager does not see the shell's XDG variables.
- **M5-P19** `uninstall --delete-data` runs stop and disable, then logout (revoke, best effort), then the deletion. `userdel pcb` runs only with `--delete-data`. A key file outside the config directory is kept and named.
- **M5-P20** Distroless static nonroot:
  - The binary is pure Go (modernc SQLite).
  - tzdata is embedded through `time/tzdata` (M4).
  - distroless ships the CA roots.
  - No shell is needed: the HEALTHCHECK uses the exec form.
- **M5-P21** The systemd smoke test runs as a CI job: GitHub runners allow `--privileged --cgroupns=host`. It is also `make systemd-smoke`.
- **M5-P22** A second SIGINT or SIGTERM during the stop exits 20 at once, and the units set `TimeoutStopSec=30s`. This closes OPEN "unbounded wait after the drain".
- **M5-P23** `make install` installs to `$(PREFIX)/bin/pcb`, with `PREFIX ?= $(HOME)/.local`. §13 names `make install`, but the Makefile lacks it.

## Review Focus

1. **A container restarted after a crash or `docker kill`** leaves `pcb.sock` on the `/data` volume. The new daemon must replace it, and `pcb status --daemon` must answer; never "address already in use", never a restart loop. Pinned by `TestStaleSocketIsReplaced` (Task 3) and the restart step of `container-smoke.sh` (Task 8).
2. **Thunderbird polling a daemon that waits for a login** (a fresh container, or a session Proton ended):
   - every authenticated request answers 503 with `Retry-After: 30`, never 401, 403 or 404 (a 4xx disables the calendar);
   - after `pcb login` the same calendars answer 207 with no restart.
   Pinned by `TestServeWaitForLoginAnswers503ThenServes` (Task 4).
3. **`docker exec ctr pcb login` without `-it`**, with `PCB_USERNAME` and `PCB_PASSWORD` set and 2FA on:
   - it exits 10 naming `pcb login resume --totp` and the deadline;
   - `PCB_TOTP=… pcb login resume --totp` then finishes the login;
   - after 5 min the resume says "expired" and never hangs.
   Pinned by `TestNonInteractiveTOTPIsHeldByTheDaemon` and `TestPendingLoginExpiresAfterFiveMinutes` (Task 5).
4. **Running `pcb install --user` twice, or after editing `config.toml`:** the second run reports every file as kept, reloads nothing and never overwrites the config. Pinned by `TestInstallUserCreatesThenKeeps` and `TestInstallKeepsAnEditedConfig` (Task 7).
5. **`pcb uninstall --delete-data` while the daemon runs:** the order is stop the unit, then revoke the session, then delete. No daemon writes into a deleted directory, and the Proton session does not outlive the local data. Pinned by `TestUninstallDeleteDataStopsThenLogsOutThenDeletes` (Task 7).

---

### Task 1: Directory overrides and the log level

**Files:**
- Modify: `internal/paths/paths.go`, `internal/config/config.go`, `internal/cli/{serve.go,io.go,doctor.go}`
- Test: `internal/paths/paths_test.go`, `internal/config/config_test.go`, `internal/cli/serve_test.go`

**Interfaces:**
- Produces:
  - `paths.Resolve(env)` honours `PCB_CONFIG_DIR` and `PCB_STATE_DIR` (M5-P1). A relative value errors with `PCB_STATE_DIR must be an absolute path` (or the config variant).
  - `(paths.Dirs).Socket() string` returns `<State>/pcb.sock`.
  - `config.Log{Level string \`toml:"level"\`}` under `[log]`, default `"info"`, and `config.ParseLevel(s string) (slog.Level, error)`, lower-case names only. An invalid value is MustAct `log.level must be one of debug, info, warn, error`, with the value never echoed.
  - `pcb serve --log-level LEVEL`: overrides `[log] level`; an invalid value exits 2.
  - The CLI's directory hints (`loadConfig`, the doctor's failed checks) name `PCB_CONFIG_DIR`/`PCB_STATE_DIR` beside `XDG_*`.
- [ ] **Step 1: failing tests:**
  - `TestStateDirOverride`: `PCB_STATE_DIR=/srv/pcb` gives State `/srv/pcb`, and Config still comes from XDG.
  - `TestConfigDirOverride`
  - `TestRelativeOverrideIsRefused`
  - `TestSocketPath`
  - `TestLogLevelDefaultsToInfo`
  - `TestUnknownLogLevelIsRefused`: the message lacks the value.
  - `TestServeHonoursTheLogLevel`: with `--log-level warn`, stderr lacks `listener started`; with `[log] level = "warn"` too; config `debug` plus flag `warn` still lacks it; the default has it.
  - `TestServeRefusesABadLogLevel`: exit 2.
- [ ] **Step 2–4:** FAIL → implement → `make lint test` PASS.
- [ ] **Step 5: Commit** `🔧 Let services set the directories and the log level`.

### Task 2: Serve's live state: idle cadence and `Health`

**Files:**
- Modify: `internal/syncer/watch.go`; `internal/service/{api.go,serve.go,sync.go,errors.go,clients.go,write.go,render.go}`
- Create: `internal/service/health.go`
- Test: `internal/syncer/watch_test.go`, `internal/service/{health_test.go,serve_test.go}`

**Interfaces:**
- Produces:
  - `syncer.WatchOptions` gains:
    - `NextInterval func() time.Duration`: the wait after a successful pass; nil means `Interval`, and the value is raised to at least `MinInterval`;
    - `Wake <-chan struct{}`: a receive ends a sleep that follows a successful pass, and nothing else.
  - `service.ActiveInterval = 30 * time.Second`, `IdleInterval = 5 * time.Minute`, `IdleAfter = 35 * time.Minute`.
  - Serve's rule (M5-P6): `CheckClient` returning ok records activity and, while idle, sends a wake. A set `ServeOptions.Interval` replaces the active interval; the idle one is then `max(IdleInterval, Interval)`.
  - `API.Health(ctx) (Health, error)`:
    ```go
    type ServeState string // "stopped" | "serving" (Task 4 adds "waiting-for-login")
    type Health struct {
        Serve             ServeState          `json:"serve"`
        Since             *time.Time          `json:"since,omitempty"` // entered this state
        IntervalSeconds   int                 `json:"interval_seconds,omitempty"`
        Idle              bool                `json:"idle"`
        LastPass          *PassHealth         `json:"last_pass,omitempty"`
        Calendars         []CalendarHealth    `json:"calendars"`
        Render            RenderCounts        `json:"render"`
        HumanVerification []HumanVerification `json:"human_verification,omitempty"`
    }
    type PassHealth struct{ At time.Time; OK bool; ErrorClass, Error string }     // OK: nil or partial
    type CalendarHealth struct {
        ID, Name string; Events int; SyncedAt *time.Time; LagSeconds *int
        ErrorClass, Error, Gate, GateReason, KeysError string
    }
    type HumanVerification struct{ Source string; Since time.Time } // "write" | "sync"
    ```
    The JSON tags are snake_case (`synced_at`, `lag_seconds`, `error_class`, `gate`, `gate_reason`, `keys_error`).
  - Outside Serve, `Health` returns `{Serve: "stopped", Calendars: []}`.
  - `RenderCounts` gains snake_case tags: `undecryptable`, `unverified`, `duplicate_masters`, `keys_errors`.
  - `CalendarSync.ErrorClass` (`error_class`).
  - `errorClass(error) string` ∈ `rate-limited`, `proton-unavailable` (5xx), `network`, `session` (a person must act), `refused` (another Proton 4xx), `deferred` (M2-R11), `cache`, `other`.
  - Gate (`open|closed|undecided|unknown`) and `GateReason`, fixed texts in `Access.Gate`'s order:
    - closed: `not a normal calendar (subscribed or holidays)`, `our member lacks the Write permission`, `the member passphrase signature does not verify`;
    - undecided: `the calendar keys did not unlock`, `the member passphrase signature cannot be checked (none served, or our address key is locked)`, `no usable primary calendar key (none unlocked, or two flagged primary)`, `our member's address has no unlocked primary key`;
    - unknown: `the keys were not loaded yet`.
  - Human verification:
    - a 9001 answer to a write's call notes source `write`, and one to a pass's call notes `sync`;
    - the next confirmed write clears `write`, and the next pass that gets through clears `sync`.
- [ ] **Step 1: failing tests:**
  - syncer:
    - `TestNextIntervalIsUsedAfterASuccess`
    - `TestWakeCutsOnlyASuccessSleep`: a wake during a back-off changes no recorded wait.
  - service, fake clock and recording `Sleep`:
    - `TestIdleServeSlowsToFiveMinutes`: 35 min with no `CheckClient` makes the next `WatchPass.Next` 5m0s.
    - `TestAuthenticatedRequestWakesAnIdleLoop`
    - `TestRefusedPasswordIsNotActivity`
  - service, fake Proton:
    - `TestHealthReportsGatesWithReasons`:
      - open;
      - `SetPassphraseSignature` bad gives closed and the verify reason;
      - none served gives undecided and the check reason;
      - `SetPermissions(0)` gives closed and the Write reason.
    - `TestErrorClass`: a table with one constructed error per class (a 429, a 503 `api.Error`, a `net.OpError`, a MustAct, a deferred calendar, a cache error, a plain error). It is pure: no retry hides the case.
    - `TestHealthCarriesTheErrorClass`: a 4xx other than 429 (`FailNext`) on one new calendar's first event listing gives that calendar `refused`, and the others `""`.
    - `TestHumanVerificationIsHeldUntilAWriteLands`
    - `TestHealthOutsideServeIsStopped`
    - `TestRenderCountsJSONKeys`
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Slow the sync when idle and report Serve's health`.

### Task 3: The control socket, alerts, live `pcb status` and `pcb doctor`

**Files:**
- Create: `internal/ctl/{wire.go,alerts.go,server.go,client.go,listen_unix.go,listen_other.go,peer_linux.go,peer_other.go}`
- Create tests: `internal/ctl/{alerts_test.go,server_test.go,listen_test.go,arch_test.go}`
- Modify: `internal/cli/{io.go,serve.go,status.go,doctor.go,root.go}`
- Test: `internal/cli/{status_test.go,doctor_test.go,serve_test.go}`

**Interfaces:**
- Consumes: `API.Health`, `paths.Dirs.Socket()`.
- Produces:
  - `ctl.Listen(path string) (net.Listener, error)`:
    - errors `ErrPathTooLong` (> 107 bytes), `ErrUnsafeDir` (the directory has group or other bits) and `ErrNotASocket` (a non-socket file at path);
    - a stale socket is removed (M5-P2);
    - the socket is chmodded 0600 after the bind;
    - off Unix it returns `ErrUnsupported`.
  - `ctl.Server{API service.API; Daemon Daemon; Listener func() *Listener; Now func() time.Time; Logger *slog.Logger; PeerUID func(net.Conn) (uid int, ok bool)}`:
    - `(*Server).Handler() http.Handler`;
    - `PeerUID` defaults to SO_PEERCRED on Linux and to "unchecked" elsewhere;
    - a mismatch is 403, with a warning log naming the peer UID.
  - The endpoint is `GET /v1/status`; Task 5 adds the others.
  - Wire types:
    ```go
    type Status struct {
        service.Status                    // embedded: flat (M5-P4)
        Daemon   *Daemon         `json:"daemon"` // null: offline view
        Health   *service.Health `json:"health,omitempty"`
        Listener *Listener       `json:"listener,omitempty"`
        Alerts   []Alert         `json:"alerts"`
    }
    type Daemon struct{ PID int; Version string; StartedAt time.Time; LogLevel, Socket string }
    type Listener struct{ Addr, URL string; TLS bool; Fingerprint string; CertNotAfter *time.Time }
    type Alert struct{ Code, Severity, Message, Fix string; Calendar string `json:"calendar,omitempty"` }
    ```
    The JSON tags are snake_case.
  - `ctl.Alerts(in AlertInput) []Alert`, with `AlertInput{Login service.Status; Health *service.Health; Listener *Listener; Now time.Time}`. The rules:

    | code | sev. | when | message | fix |
    |---|---|---|---|---|
    | `needs-login` | error | phase `logged-out` or `needs-login` | the reason, else `not logged in to Proton` | `run pcb login` |
    | `needs-captcha` | error | phase `needs-captcha` | `Proton asks for a CAPTCHA: solve it at <captcha_url>` | `pcb login resume --captcha-token TOKEN (the token parameter of that URL)` |
    | `human-verification` | error | `HumanVerification` non-empty | `Proton asks for a human verification (code 9001) since <time> (<sources>): writes answer 503` | `open the Proton web app from this machine's network and complete the verification` |
    | `sync-stalled` | warning | serving ∧ now − max(SyncedAt, Since) > 5 × interval | `no successful sync of <name> for <dur>` | `see last_pass and the logs (pcb service logs)` |
    | `calendar-keys-failed` | error | `KeysError` set | `the keys of <name> did not unlock (<reason>): its events are placeholders` | `run pcb login again; if it stays, check the calendar in the Proton web app` |
    | `write-gate-undecided` | warning | gate `undecided` | `writes to <name> answer 503: <reason>` | `check the calendar's keys in the Proton web app, then pcb service restart` |
    | `tls-certificate-expiring` | warning | TLS ∧ 0 < NotAfter − now ≤ 30 days | `the CalDAV certificate expires on <date>` | `renew caldav.tls_cert; a generated one is renewed by restarting pcb` |
    | `tls-certificate-expired` | error | TLS ∧ NotAfter ≤ now | `the CalDAV certificate expired on <date>` | same |

  - `ctl.Dial(ctx, path) (*Client, error)`:
    - `ErrNoDaemon` when the path is missing or refuses (ENOENT, ECONNREFUSED), and always off Unix;
    - `(*Client).Status(ctx) (Status, error)`;
    - a 422 envelope comes back as `*exitcode.Error{Code, Err, Hint}`.
  - `internal/ctl` imports only the standard library, `service`, `output`, `exitcode` and `buildinfo` (`arch_test.go`).
  - CLI:
    - `IO.DialDaemon func(ctx, path string) (*ctl.Client, error)` (default `ctl.Dial`).
    - `pcb status`:
      - with a daemon, prints its Status;
      - without one, prints `ctl.Status{Status: local, Daemon: nil, Alerts: ctl.Alerts(login only)}`;
      - `--daemon` with none exits 20 with `no pcb daemon answers on <socket>` (hint `start it: pcb service start, or pcb serve`).
    - The status text adds lines after the M4 ones:
      - `daemon: serving since <t> (pid N, version V)`, `daemon: not running (offline view)` (offline), or `daemon: waiting for a login since <t>` (Task 4);
      - `listener: <url> (TLS, certificate until <date>, fingerprint <fp>)`;
      - one `calendar: <name> (<id>): synced <t|never>, lag <d>, write gate <gate>[ (<reason>)][, last error <class>: <error>]` per calendar;
      - `render: undecryptable N, unverified N, duplicate masters N`;
      - one `alert: <severity> <code>: <message> (fix: <fix>)` per alert.
    - `pcb doctor` (now an `app` method):
      - result `daemon`: OK `serving since …` / skip `not running: offline checks only`;
      - one result per alert, named `alert <code>[ <calendar>]`, error → ✗ and warning → !, with its fix;
      - exit 20 on any error, as before.
    - `pcb serve` binds CTL once `Ready` fires, before CalDAV:
      - a failure exits 20 with the `ctl.Listen` cause as hint;
      - it publishes `Listener` after the CalDAV bind;
      - it closes and removes the socket on stop.
- [ ] **Step 1: failing tests:**
  - socket and server:
    - `TestSocketIsPrivate`: mode 0600; a 0755 directory gives `ErrUnsafeDir`.
    - `TestStaleSocketIsReplaced`
    - `TestNonSocketFileIsRefused`
    - `TestLongSocketPathIsRefused`: 108 bytes.
    - `TestOtherUIDIsRefused`: `PeerUID` returns uid+1, giving 403 with no data.
    - `TestStatusOverTheSocket`
    - `TestUnknownPathAndMethod`: 404, 405.
    - `TestOversizedBodyIs413`
    - `TestErrorsCrossTheSocket`: a MustAct with its hint comes back with code 10, the same message and the same hint.
  - alerts:
    - `TestAlerts`: one table row per code that fires and one that does not, with boundaries at 30 days ± 1 min and 5 × interval ± 1 s.
    - `TestAlertsAreOrdered`
  - CLI:
    - `TestStatusFallsBackOffline`: `daemon` null, the text line, exit 0.
    - `TestStatusShowsTheDaemon`: a ctl server on a fake API whose Health has a closed gate shows the calendar line with its reason, and the alert lines.
    - `TestStatusDaemonFlagNeedsADaemon`: exit 20.
    - `TestDoctorListsAlerts`: `needs-login` gives a ✗ line with its fix, exit 20.
    - `TestServeServesTheControlSocket`: status over the socket while serving; the socket is gone after the stop.
- [ ] **Step 2–4:** FAIL → implement → PASS (`GOOS=windows go vet ./...` clean).
- [ ] **Step 5: Commit** `✨ Serve a control socket and show the daemon's live status`.

### Task 4: `pcb serve --wait-for-login`

**Files:**
- Modify: `internal/service/{api.go,serve.go,local.go,login.go,errors.go,objects.go,write.go}`, `internal/caldav/server.go`, `internal/cli/{serve.go,io.go}`
- Test: `internal/service/serve_test.go`, `internal/caldav/pending_test.go`, `internal/cli/serve_test.go`

**Interfaces:**
- Consumes: Task 2's `ServeState`, Task 3's CTL in serve.
- Produces:
  - `ServeOptions.WaitForLogin bool`, `OnState func(ServeState)`, `LoginPoll time.Duration` (0 means 5 s), and `ServeState` `"waiting-for-login"`.
  - With `WaitForLogin`, Serve:
    1. takes `sync.lock` (refused: exit 20, as today) and calls `Ready` at once;
    2. waits until `stored()` succeeds, polling every `LoginPoll` and woken at once by a `Login` or `SubmitTOTP` that completes in this process;
    3. opens the cache and serves;
    4. on a MustAct stop error, closes the cache, logs `waiting for a login` with the reason, and goes back to step 2;
    5. returns nil when ctx ends, and any other stop error as is.
  - Without the flag, M3 behaviour stays.
  - `ErrWaitingForLogin` (Blocked, hint `run pcb login`) is returned by `Collections`, `Objects`, `ObjectsByUID`, `ObjectsByName`, `Changes`, `PutObject` and `DeleteObject` while waiting. The CalDAV adapter answers it 503 with `Retry-After: 30` and **no** warning line, like `ErrCalendarPending`.
  - `Logout` during Serve (M5-P10) stops the loop and closes the handle, then wipes. Serve then waits with the flag, or returns MustAct `not logged in to Proton` without it.
  - CLI:
    - `pcb serve --wait-for-login`: the intro prints at bind, plus `Waiting for a login: run pcb login` while waiting;
    - CTL status reports `waiting-for-login`;
    - `IO.Signals func() (<-chan os.Signal, func())` (default `signal.Notify` on SIGINT, SIGTERM): a second signal during the stop exits 20, hint `the sync loop was cut; run pcb serve again` (M5-P22).
- [ ] **Step 1: failing tests:**
  - service:
    - `TestServeWaitsForALogin`: logged out with `WaitForLogin`:
      - `Ready` is called and `OnState` reports waiting;
      - `Collections` returns `ErrWaitingForLogin`;
      - an in-process `Login` on the fake brings serving and a first pass with no extra poll.
    - `TestServeNoticesALoginFromAnotherProcess`: a second `Local` on the same directories logs in, and serving starts within one `LoginPoll`.
    - `TestSessionEndWaitsInsteadOfExiting`: after `RevokeSessions`, with no unattended relogin:
      - the state is waiting and the phase `needs-login`;
      - Serve has not returned.
    - `TestServeWithoutTheFlagKeepsM3`: logged out gives exit 10 with `Ready` never called; a session end returns MustAct.
    - `TestLogoutDuringServeClosesTheCacheFirst`: after `Logout`, `pcb.db` is absent and `Health.Serve` is waiting.
  - caldav: `TestWaitingForLoginIs503WithoutAWarning`.
  - CLI:
    - `TestServeWaitForLoginAnswers503ThenServes` (Review Focus 2):
      - logged out: an authenticated PROPFIND on `/dav/calendars/me/` gets 503 with `Retry-After: 30`;
      - unauthenticated gets 401;
      - CTL status carries `needs-login`;
      - after a login on the fake, the same PROPFIND gets 207 within one poll and the process is unchanged.
    - `TestSecondSignalForcesExit`: exit 20.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Let pcb serve wait for a login`.

### Task 5: Login and logout through the daemon (R16, R8m)

**Files:**
- Modify: `internal/ctl/{server.go,client.go,wire.go}`, `internal/service/{api.go,local.go,login.go}`, `internal/cli/{login.go,logout.go}`
- Test: `internal/ctl/server_test.go`, `internal/service/login_test.go`, `internal/cli/login_test.go`

**Interfaces:**
- Consumes: Task 3's server and client, Task 4's in-process wake.
- Produces:
  - Endpoints, each answered by the matching `service.API` method:
    - `POST /v1/login`, body `service.LoginRequest`, gives `LoginResult`;
    - `POST /v1/login/totp`, body `{"code": "…"}`, gives `LoginResult`;
    - `POST /v1/logout` gives `LogoutResult`.
  - Logs carry the path and the exit code only.
  - Client methods: `Login(ctx, service.LoginRequest)`, `SubmitTOTP(ctx, code)`, `Logout(ctx)`.
  - service:
    - `pendingTTL = 5 * time.Minute`, and `Options.AfterFunc func(time.Duration, func()) (stop func() bool)` (default `time.AfterFunc`);
    - on expiry, the half session is revoked, the password zeroed and `waiting` cleared;
    - `SubmitTOTP` afterwards gives MustAct `the login waiting for its 2FA code expired after 5 minutes`, hint `run pcb login again`;
    - `Status.LoginExpires *time.Time` (`login_expires`) is set with needs-totp.
  - CLI, with a daemon (`DialDaemon` succeeds); the in-process path stays without one:
    - `pcb login` uses the client;
    - needs-totp, interactive: prompt, then `SubmitTOTP`, up to 3 tries;
    - needs-totp, non-interactive: exit 10 `the account needs a 2FA code`, hint `PCB_TOTP=<code> pcb login resume --totp before <HH:MM:SS> (the daemon holds the login)`.
  - `pcb login resume`:
    - takes exactly one of `--captcha-token T` and `--totp` (a boolean, M5-P11); else exit 2;
    - `--totp` reads the code from `PCB_TOTP`, else the prompt (interactive), else exit 10;
    - with no daemon, `--totp` exits 10 with `no pcb daemon holds a login waiting for its code`, hint `run pcb login with PCB_TOTP set`;
    - `--captcha-token` gathers the credentials as today and uses the daemon when one runs (M5-P12).
  - `pcb logout` uses the client with a daemon.
  - Text mode with no daemon: `pcb login` and `pcb logout` print `Note: no pcb daemon is running; this command runs on its own.` on stderr.
- [ ] **Step 1: failing tests:**
  - service:
    - `TestPendingLoginExpiresAfterFiveMinutes`: the captured `AfterFunc` fires; then:
      - the fake records the half session's logout;
      - `SubmitTOTP` gives MustAct `expired`;
      - `Status.Phase` is not `needs-totp`.
    - `TestPendingPasswordIsZeroedOnExpiry`: an internal test.
    - `TestSubmitBeforeExpiryStillWorks`
  - ctl:
    - `TestLoginOverTheSocketNeverLogsSecrets`: the log buffer lacks the password and the code.
    - `TestLoginBodyIsBounded`: 413.
  - CLI:
    - `TestLoginGoesThroughTheDaemon`: the in-process `NewService` fails the test if called.
    - `TestNonInteractiveTOTPIsHeldByTheDaemon` (Review Focus 3): exit 10 naming `resume --totp`, then `PCB_TOTP=<fake code> login resume --totp` gives ready.
    - `TestResumeTOTPWithoutADaemonIsExit10`
    - `TestResumeNeedsExactlyOneMode`: exit 2.
    - `TestLogoutGoesThroughTheDaemon`
    - `TestNoDaemonLoginSaysSo`
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Log in through the running daemon`.

### Task 6: Sealed-secrets generation and the state-file lock (R3m, R10m)

**Files:**
- Modify: `internal/store/{secrets.go,state.go}`, `internal/service/{login.go,local.go,session.go,errors.go}`, `internal/doctor/secrets.go`
- Test: `internal/store/{secrets_test.go,state_test.go}`, `internal/service/{login_test.go,session_test.go}`, `internal/doctor/doctor_test.go`

**Interfaces:**
- Produces:
  - Format (M5-P14):
    - `secretsVersion = 2` seals `{"generation": N, "accounts": {…}}` with AAD `pcb-secrets/v2/<sealer>`;
    - a v1 file opens with the v1 AAD as generation 0 and is rewritten as v2 at the next `Update`.
  - `Update`:
    - writes generation = loaded + 1, then mirrors `State.SecretsGeneration` (`secrets_generation`) as max(mirror, generation);
    - a failed mirror write still returns nil, because the sealed file is ahead, which is accepted.
  - An empty result, or `Wipe`, sets the mirror to last generation + 1.
  - `Load`: a sealed generation below the mirror is `store.ErrRolledBack`, the message naming both numbers.
  - `store.UpdateState(stateDir string, fn func(*State) error) error`: loads, applies fn and writes atomically, holding `state.lock` (`filelock.Lock`, 10 s; an in-process mutex off Unix, M5-P15). `fn` returning `store.ErrStateUnchanged` writes nothing.
  - `SaveState` keeps max(SecretsGeneration). Every writer uses one of the two: `needsCaptcha`, `complete`, `Logout`, and `humanNeeded`, whose `sameLogin` check runs inside fn.
  - service:
    - `ErrRolledBack` → MustAct `secrets.sealed is older than the last one pcb wrote (generation G < M): it was restored or replaced`, hint `if you restored it on purpose, run pcb logout, then pcb login`;
    - `Status.Secrets` becomes `rolled-back`, with phase `needs-login` and that reason;
    - `Logout` wipes a rolled-back store without revoking.
  - doctor: `SecretsCheck` reports it as an error with that hint.
- [ ] **Step 1: failing tests:**
  - store:
    - `TestGenerationIncrementsAndIsMirrored`
    - `TestRestoredOlderSecretsAreRefused`
    - `TestFileRemovedByLogoutIsRefusedIfRestored`
    - `TestV1FileIsReadAndUpgraded`
    - `TestSealedAheadOfMirrorIsAccepted`
    - `TestEditedVersionFieldDoesNotDowngrade`: v2 data under `version: 1` gives `seal.ErrCorrupt`.
    - `TestUpdateStateWaitsForTheLock`: the test holds `state.lock`, and the write lands only after release.
    - `TestSaveStateKeepsTheGeneration`
  - service:
    - `TestRolledBackSecretsNeedALogin`
    - `TestLogoutWipesRolledBackSecrets`
    - `TestHumanNeededDoesNotOverwriteAFinishedLogin`: `state.json` changes between the decision and the write, and is kept.
  - doctor: `TestSecretsCheckReportsRollback`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `🔒 Detect restored sealed secrets and lock state writes`.

### Task 7: `pcb install`, `pcb uninstall`, `pcb service`

**Files:**
- Create: `internal/install/{host.go,units.go,plan.go,apply.go,detect.go}`, `internal/install/testdata/{pcb-user.service,pcb-system.service,config.toml}.golden`, `internal/cli/{install.go,service.go}`
- Modify: `internal/cli/{root.go,io.go,doctor.go}`, `Makefile` (`install` target, M5-P23)
- Test: `internal/install/{units_test.go,plan_test.go,detect_test.go}`, `internal/cli/{install_test.go,service_test.go}`

**Interfaces:**
- Consumes: `paths.Resolve` (Task 1); the CLI's `service.API.Logout` for `--delete-data`.
- Produces:
  - `install.Host`:
    ```go
    type Host struct {
        Root       string // prefix of every absolute path ("" in production; a temp dir in tests)
        Run        Runner
        Euid       func() int
        Executable func() (string, error)
        LookupUser func(name string) (uid, gid int, ok bool, err error)
        Chown      func(path string, uid, gid int) error
        Env        func(string) string
    }
    ```
    `install.OSHost()` builds the production one.
  - `install.Runner` has two methods:
    - `Output(ctx, name string, args ...string) (stdout []byte, code int, err error)`;
    - `Stream(ctx, stdout, stderr io.Writer, name string, args ...string) (code int, err error)`.
  - `install.UnitParams{Exec, ConfigDir, StateDir string}`; `UserUnit(UnitParams) []byte`, `SystemUnit(UnitParams) []byte`, `DefaultConfig() []byte`.
  - `install.Step{Action, Kind, Path, Command, Mode, Owner, Note string}`:
    - Action ∈ `create|update|keep|run|remove|skip`;
    - Kind ∈ `dir|file|command|user|logout`.
  - `install.Plan(ctx, Host, Request{Scope Scope}) ([]Step, error)`, `install.PlanUninstall(ctx, Host, UninstallRequest{Scope Scope; DeleteData bool}) ([]Step, error)`, `install.Apply(ctx, Host, []Step, Hooks{Logout func(ctx) error}) error`. `Apply` fails with `*StepError{Index int; Step Step; Err error}`.
  - `install.Detect(Host) (Scope, unitPath string, err error)`:
    - user unit: `<XDG_CONFIG_HOME|~/.config>/systemd/user/pcb.service`;
    - system unit: `/etc/systemd/system/pcb.service`;
    - both: `ErrAmbiguous`;
    - neither: `ErrNotInstalled`.
  - **System unit** (golden; the user unit is the same `[Unit]` and `[Install] WantedBy=default.target` minus the marked lines):
    ```ini
    [Unit]
    Description=pcb, the Proton Calendar CalDAV bridge
    Documentation=https://github.com/Barben360/proton-calendar-bridge
    After=network-online.target
    Wants=network-online.target
    StartLimitIntervalSec=300
    StartLimitBurst=5

    [Service]
    Type=simple
    User=pcb                                           # system only
    Group=pcb                                          # system only
    Environment=PCB_CONFIG_DIR=<config>
    Environment=PCB_STATE_DIR=<state>
    ExecStart=<exec> serve --wait-for-login
    Restart=on-failure
    RestartSec=10s
    TimeoutStopSec=30s
    UMask=0077
    NoNewPrivileges=yes
    RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
    RestrictNamespaces=yes
    RestrictRealtime=yes
    RestrictSUIDSGID=yes
    LockPersonality=yes
    MemoryDenyWriteExecute=yes
    SystemCallArchitectures=native
    SystemCallFilter=@system-service
    SystemCallFilter=~@privileged
    SystemCallErrorNumber=EPERM
    ProtectSystem=strict                               # system only, from here
    ProtectHome=yes
    ReadWritePaths=/etc/pcb /var/lib/pcb
    PrivateTmp=yes
    PrivateDevices=yes
    ProtectKernelTunables=yes
    ProtectKernelModules=yes
    ProtectKernelLogs=yes
    ProtectControlGroups=yes
    ProtectClock=yes
    ProtectHostname=yes
    ProtectProc=invisible
    ProcSubset=pid
    CapabilityBoundingSet=
    AmbientCapabilities=

    [Install]
    WantedBy=multi-user.target                         # default.target for --user
    ```
    The `#` annotations are for this plan, not the file. Do not add `~@resources`: Go's runtime calls `setrlimit`.
  - **Default config** (golden; it must load equal to `config.Defaults`):
    ```toml
    # pcb configuration (SPEC §14). Unknown keys are refused; run pcb doctor after editing.
    [caldav]
    # listen = "127.0.0.1:1080"   # HOST:PORT; any non-loopback address serves TLS
    # hostnames = ["pcb.example"] # names clients use: certificate SANs, first one in the URL
    [log]
    # level = "info"              # debug, info, warn or error
    ```
  - Steps, `--user`:
    1. dir config 0700;
    2. dir state 0700;
    3. file `config.toml` 0600, create only, never updated;
    4. dir `…/systemd/user`;
    5. file unit 0644;
    6. `systemctl --user daemon-reload`, only when the unit was created or updated;
    7. `systemctl --user enable --now pcb.service`;
    8. `systemctl --user restart pcb.service`, only when the unit was updated.

    The CLI then prints the next steps:
    - `loginctl enable-linger <user>` when `loginctl show-user <user> -p Linger` says `no`;
    - `pcb login`;
    - `pcb client add NAME`.
  - Steps, `--system`:
    1. user `pcb` (`useradd --system --user-group --home-dir /var/lib/pcb --no-create-home --shell /usr/sbin/nologin pcb`), skipped when `LookupUser` finds it;
    2. dirs `/etc/pcb` and `/var/lib/pcb`, 0700 `pcb:pcb`;
    3. `config.toml` 0600 `pcb:pcb`, create only;
    4. file `/usr/local/bin/pcb` 0755 root, a copy of `Executable()`, kept when identical (M5-P16);
    5. unit `/etc/systemd/system/pcb.service` 0644;
    6. `systemctl daemon-reload`, `systemctl enable --now pcb.service`, and the restart, under the same conditions as `--user`.

    The CLI prints `sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb login`.
  - Uninstall steps (M5-P19):
    1. `systemctl [--user] disable --now pcb.service`, skipped when no unit;
    2. remove the unit;
    3. `daemon-reload`;
    4. without `--delete-data`, notes keeping the config and state directories;
    5. with `--delete-data`:
       - a `logout` step (the hook runs the in-process `Logout` on the scope's directories, `/etc/pcb` and `/var/lib/pcb` for system scope, never the caller's own; a failure becomes a note and the steps go on);
       - remove the config directory and the state directory (recursively: `clients.json`, `pcb.db`, `secrets.sealed`, `tls/`);
       - a `key_file` outside the config directory is kept, with a note naming it;
       - system scope: `userdel pcb`, then a note `kept /usr/local/bin/pcb: rm it if unused`.
  - CLI rules:
    - Flags: `pcb install [--user|--system] [--plan]`, `pcb uninstall [--user|--system] [--delete-data] [--yes] [--plan]`. Both scope flags at once is exit 2. `install --user` as root is exit 2 (`use --system`). `--system` as non-root is exit 10, hint `sudo pcb install --system`.
    - No `/run/systemd/system` (under `Root`): `--plan` prints the steps plus a note, and applying exits 20 `systemd is not running here`, hint `in a container, run pcb serve as the entrypoint (README)`.
    - A step failing exits 40 naming it, hint `fix it, then run the command again: finished steps are kept`.
    - Text `--plan` prints one line per step: `<action> <kind> <path|command> [<mode> <owner>] [— note]`, then `Plan only: nothing was changed.` JSON data is `{scope, plan, steps, next}`.
    - `--delete-data` confirmation:
      - interactive: `Delete <config> and <state> (Proton session, cache, CalDAV clients)? Type delete to confirm: `; any other answer exits 10;
      - `--yes` skips the prompt;
      - non-interactive without `--yes` exits 10 naming `--yes`;
      - `--plan` never asks.
    - `pcb service start|stop|restart`: `systemctl [--user] <verb> pcb.service`, adding `--no-ask-password` for system scope as non-root under `--non-interactive`. A non-zero exit is exit 20 with systemctl's last stderr line, hint `pcb service logs`.
    - `pcb service status`: `systemctl [--user] show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID`, parsed into `{scope, unit, active_state, sub_state, unit_file_state, main_pid}`. Exit 0 for any state.
    - `pcb service logs [-f]`: `journalctl [--user] -u pcb.service --no-pager -n 200 [-f]`, streamed; `--json` is exit 2.
    - Scope selection: `--user|--system` override `Detect`. Ambiguous is exit 2; not installed is exit 10, hint `pcb install`.
  - doctor: a `service` result: the scope and `ActiveState`; skip `not installed as a service (pcb install)` or `no systemd here`.
- [ ] **Step 1: failing tests:**
  - goldens:
    - `TestUserUnitGolden`
    - `TestSystemUnitGolden`
    - `TestDefaultConfigGolden`
    - `TestDefaultConfigLoadsAsDefaults`
  - install, temp `Root` and a fake runner:
    - `TestInstallUserPlanChangesNothing`: the tree is still empty, and the runner saw only read-only probes.
    - `TestInstallUserCreatesThenKeeps` (Review Focus 4): second run all `keep`, no `daemon-reload`.
    - `TestInstallKeepsAnEditedConfig`
    - `TestInstallUpdatesAChangedUnitAndRestarts`
    - `TestInstallSystemNeedsRoot`
    - `TestInstallSystemCreatesTheUserOnce`
    - `TestInstallSystemCopiesTheBinary`
    - `TestInstallUserAsRootIsRefused`
    - `TestInstallWithoutSystemdIsBlocked`
    - `TestInstallFailureMidwayIsPartial`: `enable` fails, giving exit 40 with the unit present.
  - uninstall:
    - `TestUninstallKeepsDataByDefault`
    - `TestUninstallDeleteDataNeedsConfirmation`
    - `TestUninstallDeleteDataStopsThenLogsOutThenDeletes` (Review Focus 5): the event order is `disable --now`, then the logout hook, then the removal.
    - `TestUninstallKeepsAKeyFileOutsideTheConfigDir`
  - service:
    - `TestServiceDetectsTheScope`
    - `TestServiceStatusParsesShow`
    - `TestServiceLogsFollow`
    - `TestServiceStartFailureIsBlocked`
- [ ] **Step 2–4:** FAIL → implement → PASS (`GOOS=windows go vet ./...` clean).
- [ ] **Step 5: Commit** `✨ Install pcb as a systemd service`.

### Task 7b: Viewable client credentials (decision 0013)

**Files:**
- Modify: `internal/access/clients.go` (format v2), `internal/service/{api.go,clients.go,login.go}`, `internal/cli/client.go`
- Test: `internal/access/clients_test.go`, `internal/service/clients_test.go`, `internal/cli/client_test.go`

**Interfaces:**
- Consumes: the sealer `internal/store` already uses for `secrets.sealed` (same key, its own AAD); Task 3's listener and TLS facts for the URL and fingerprint (offline: computed from config as `pcb serve`'s intro does).
- Produces:
  - `clients.json` version 2: each record keeps `salt` + `hash` (the check is unchanged, constant time) and gains `sealed` (base64 of the password sealed with AAD `pcb-clients/v2/<sealer>/<client id>`). A v1 file still loads; its clients have no `sealed` and stay valid. The file is rewritten as v2 at its next change.
  - `access.Store.Reveal(id string, open func(sealed, aad []byte) ([]byte, error)) (password string, err error)`; `ErrNotRecoverable` for a record with no `sealed`, or one the key cannot open.
  - `access.Store.Rotate(ctx, idOrName string) (Client, password string, error)`: a new salt, hash and sealed copy in one write; the old password fails the next check (the store already reloads on change, M3-R18).
  - `service.API`:
    - `ShowClient(ctx, idOrName string) (ClientCredentials, error)`, with `ClientCredentials{Client; Username, Password, URL, Fingerprint string; Recoverable bool}`;
    - `RotateClient(ctx, idOrName string) (ClientSecret, error)`.
    - An empty name means `default`, else the only client; several clients and no name → MustAct `name a client: pcb client show NAME (pcb client list)`.
  - Default client: after a login completes (any path: in process, through CTL, resume), if `clients.json` holds no client, the service adds `default` and logs `created the CalDAV client "default": see pcb client show` (no secret). It is never re-created once any client exists, and removing it is allowed.
  - CLI:
    - `pcb client show [NAME] [--reveal]`: the text prints `URL`, `Username`, `Password`, and `TLS certificate SHA-256 fingerprint` when TLS. `--json` omits `password` unless `--reveal` is given. An unrecoverable password prints `Password: not recoverable (made before pcb could show passwords): run pcb client rotate NAME` and exits 0.
    - `pcb client rotate NAME`: prints the new password and the hint `update the password in your CalDAV client`.
    - `pcb login` (text) ends with `CalDAV credentials: pcb client show` when it just created `default`.
- [ ] **Step 1: failing tests:**
  - `TestRevealReturnsTheAddedPassword`
  - `TestRevealRefusesAnotherClientsAAD` (a sealed blob moved to another record does not open)
  - `TestV1ClientsStillCheckAndAreNotRecoverable`
  - `TestRotateInvalidatesTheOldPassword`
  - `TestFirstLoginCreatesTheDefaultClient`
  - `TestDefaultIsNotRecreatedOnceAnyClientExists`
  - `TestClientShowPrintsWhatThunderbirdNeeds`
  - `TestClientShowJSONHidesThePasswordWithoutReveal`
  - `TestNoPasswordInLogs` (login, add, rotate and show, with the log buffer scanned for every password)
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Show CalDAV credentials again and create a default client`.

### Task 8: Container image, smoke tests, workflows

**Files:**
- Create: `Dockerfile`, `.dockerignore` (`.git`, `bin`, `spec`, `*.md` except `README.md`), `scripts/container-smoke.sh`, `scripts/systemd-smoke.sh`, `testdata/systemd/Dockerfile`, `.github/workflows/image.yml`
- Modify: `.github/workflows/ci.yml` (job `systemd-smoke`), `Makefile` (`image`, `container-smoke`, `systemd-smoke`)

**Interfaces:**
- Consumes: Tasks 1, 3, 4, 7.
- Produces:
  - Image (Global Constraints):
    - build args `VERSION COMMIT DATE` passed to `make build OUT=/out/pcb`;
    - `/data/{config,state}` 0700, owned 65532;
    - HEALTHCHECK `--interval=30s --timeout=10s --start-period=20s --retries=3`.
  - `make image` builds `pcb:local` with `VERSION=$(git describe …)`.
  - `scripts/container-smoke.sh` (needs docker, curl and jq; prints `PASS` last; cleans up in a trap):
    1. build;
    2. run with a fresh named volume and `-p 127.0.0.1:18443:1080`;
    3. within 60 s, `docker inspect` health is `healthy`;
    4. `docker exec … pcb status --json --daemon` gives ok, phase `logged-out`, an alert `needs-login`, and a `daemon.version` other than `dev`;
    5. `docker exec … pcb client add smoke --json` gives a password;
    6. `curl -sk` with Basic on `/dav/calendars/me/` gives 503 with `Retry-After: 30`, and without auth 401;
    7. `Config.User` is 65532;
    8. no `secrets.sealed` exists;
    9. `docker kill`, then `docker start` the same container: healthy again within 60 s (Review Focus 1).
  - `scripts/systemd-smoke.sh`:
    - `testdata/systemd/Dockerfile` is `debian:13` pinned by digest, plus `systemd systemd-sysv dbus-user-session curl jq passwd ca-certificates`.
    - It runs `docker run -d --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup:rw … /sbin/init`, then waits for `systemctl is-system-running --wait` (`running` or `degraded`).
    - As user `tester` (linger enabled; `runuser -u tester -- env XDG_RUNTIME_DIR=/run/user/<uid> …`):
      1. `pcb install --user --plan` leaves no unit;
      2. `pcb install --user`;
      3. `systemctl --user is-enabled` gives `enabled` and `is-active` gives `active`;
      4. `pcb service status --json` gives `active_state` `active`;
      5. `pcb status --json --daemon` carries `needs-login`;
      6. curl 127.0.0.1:1080 with a client gives 503;
      7. `systemd-analyze --user verify` passes;
      8. a second `install --user --json` gives every file step `keep`;
      9. `pcb uninstall --user`.
    - As root:
      1. `install --system --plan` leaves no unit;
      2. `install --system`;
      3. `systemctl is-active pcb` gives `active`;
      4. `runuser -u pcb -- env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb pcb status --json --daemon` carries `needs-login`;
      5. `systemd-analyze verify` passes, and `systemd-analyze security pcb.service` is printed, not asserted;
      6. `pcb uninstall --system --delete-data --yes --non-interactive` leaves no unit, no `/var/lib/pcb` and no user `pcb`.
    - It prints `PASS`.
  - `ci.yml`: job `systemd-smoke` (ubuntu-latest, 15 min) runs `make systemd-smoke`.
  - `image.yml`:
    - `pull_request`: build plus container smoke, no push;
    - `push` to `main`: the same, then `docker login ghcr.io` with `GITHUB_TOKEN` through `--password-stdin`, then push `ghcr.io/barben360/proton-calendar-bridge:main` and `:sha-<12 hex>`;
    - `permissions: contents: read` everywhere, plus `packages: write` on the push job only;
    - only `actions/checkout` at the SHA `ci.yml` pins.
- [ ] **Step 1:** write the Dockerfile, the scripts and the Makefile targets.
- [ ] **Step 2:** run `make container-smoke` and `make systemd-smoke`. Expected: both print `PASS`, locally if Docker runs here; otherwise the PR's CI run is the gate, and the report says so.
- [ ] **Step 3:** sabotage check. Swap `--wait-for-login` out of the image command: the container smoke test must fail at the health step. Restore it.
- [ ] **Step 4: Commit** `🐳 Build the container image and smoke-test it and the units`. Push the workflow changes over SSH.

### Task 9: Docs, live checklist, open items

**Files:**
- Modify:
  - `spec/SPEC.md`: §7.1 (`PCB_*`, `pcb.sock`, `state.lock`), §7.3 (generation, residual), §8.1 (cadence, idle), §13 (tree, `--daemon`, `--wait-for-login`, `--log-level`, `resume --totp`, install/uninstall/service), §14 (`[log]`), §15 (CTL status, alerts, Prometheus deferred), §17, §18 (M5 delivered, rulings, deferred), §19 (CTL peer check, units, image user);
  - `spec/OPEN.md`: close the "For M5" items done, and move the deferred ones to "Later" with their reasons;
  - `spec/HUMAN-CHECKS.md`: new step 7, "M5 live session";
  - `spec/experiments/E15-secret-sealing.md`: what the smoke tests now show (a systemd service, a container);
  - `README.md`: Status, Usage, "Install as a service", "Run in a container", Credits.

**Interfaces:** none (docs only). Every claim is marked as the spec does: [verified: test name] or [assumed → step 7].
- [ ] **Step 1: README:**
  - **Install as a service:** `make install`, `pcb install`, linger, login, client and Thunderbird; `--system`; `pcb service`; uninstall.
  - **Run in a container:** a compose example (one `/data` volume; the key file as a compose secret at `/run/secrets/pcb_key` with `[secrets] key_file`, in the format `seal.KeyFile` reads; `hostnames` for the certificate; `docker exec -it pcb pcb login`; `pcb client add`). It also notes that `/data` must be writable by UID 65532 on a bind mount.
  - **Credits:** caloxide's standby idea, and the Proton Mail Bridge container model.
- [ ] **Step 2: HUMAN-CHECKS step 7.** Run on the test account only, on blackwater or a fresh Debian VM; never morpheus, never the author's account. Exact commands and expected outputs:
  - 7.1 a fresh Debian host from source: install, linger, `pcb login` (TOTP held across `resume --totp`), Thunderbird, a reboot that keeps it serving;
  - 7.2 the container: `docker compose up`, `docker exec -it … pcb login`, healthy, Thunderbird over TLS with fingerprint pinning;
  - 7.3 a session ended by Proton: `needs-login`, 503, then a re-login with no restart;
  - 7.4 idle: Thunderbird closed for over 35 min gives `interval_seconds: 300`, then reopening it brings a pass within seconds;
  - 7.5 `pcb status` and `pcb doctor` live, as the author reads them;
  - 7.6 `uninstall --delete-data` revokes the session (the web app's session list).
- [ ] **Step 3:** the SPEC, OPEN and E15 edits.
- [ ] **Step 4: Commit** `📝 Document M5: daemon, install, container, live checklist`.
