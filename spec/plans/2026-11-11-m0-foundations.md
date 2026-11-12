# M0 Foundations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A buildable `pcb` skeleton (`version`, `doctor`), CI, repo conventions, a throwaway-safe experiment harness (`pcb-lab`), the M0 experiments written up (E03 as go/no-go gate, then E01, E02, E15), and a synthetic data set on the test account.

**Architecture:** One Go module. Product code under `cmd/pcb` + `internal/{cli,output,exitcode,doctor,otp}`; experiment code under `cmd/pcb-lab` + `internal/lab` (never shipped in a release or image). Live calls go only to the test account, with credentials injected by `pass-cli run` from the author's password manager.

**Tech Stack:** Go 1.27, cobra v1.10.2, go-proton-api `v0.0.0-20261022084531-3f8fbebac58d` (+ resty replace), golangci-lint v2, GitHub Actions, pass-cli 2.7.0, proton-cal fork binary (seeding tool only).

**Spec:** `spec/SPEC.md` draft v2 (commit 6f7681e), §2, §5.1–5.2, §7.3, §13, §16, §17, §18 M0; decisions 0001–0010.

## Global Constraints

- Module `github.com/Barben360/proton-calendar-bridge`, `go 1.27`; binary `pcb` (§4).
- CLI conventions (§13): every command has `--json` printing one envelope `{schema_version, command, ok, data | error}`; exit codes `0` done, `2` bad invocation, `10` you must act, `20` blocked by the machine, `30` failed and rolled back, `40` partly done and resumable.
- Secrets only from a terminal prompt, a file descriptor or the environment, never argv; never in logs, errors or recorded files (§5.2, §19).
- Live Proton calls only against the test account, its credentials from the author's password manager (decision 0005). Never the author's own account.
- Good citizen (§2): at most one login per lab run, a 60 s minimum between lab logins (enforced), honour `Retry-After`.
- Logic reused from other projects is audited, rewritten with our own tests, and credited in `README.md` (decision 0002). Never copy from ProtonMail/WebClients (GPL-3.0).
- Tests must fail when the behaviour they guard breaks; reviews check this by sabotage (§16).
- go-proton-api needs `replace github.com/go-resty/resty/v2 => github.com/ProtonMail/resty/v2 v2.0.0-20250929142426-e3dc6308c80b` [verified: proton-cal go.mod].
- Workflow files are pushed over SSH (`git@github.com:`): the local gh token lacks the `workflow` scope [verified 2026-11-10].
- Experiment write-ups: `spec/experiments/E##-<slug>.md` with Question, Method, Results (redacted), Conclusion, Feeds (§17); raw recordings in `spec/experiments/raw/E##/`, redacted by `internal/lab/redact` only.

## Review Focus

1. **A secret reaching a recording or log** (token, password, TOTP seed, key packet, salted passphrase, session UID) → `redact` must replace it; pinned by `TestRedactNeverLeaksKnownSecretFields` and `TestRecorderRefusesUnredacted` (Task 4).
2. **A lab run that logs in again and again** (crash loop, script loop) → refused within 60 s of the last login with exit 20; pinned by `TestLoginGuardRefusesWithin60s` (Task 5).
3. **A CAPTCHA mid-experiment** → the run stops cleanly with the URL and the token saved, exit 10, no retry storm; pinned by `TestLoginHVStopsWithResumeInfo` (Task 5).
4. **`pcb --json` on a failing command** → still exactly one valid envelope on stdout, nothing else; pinned by `TestJSONEnvelopeOnError` (Task 1).
5. **The seeder run twice** (after a crash or by mistake) → no duplicate events; pinned by `TestSeedPlanSkipsExisting` (Task 10).

---

### Task 1: Repository skeleton, `pcb version`, output envelope, exit codes, CI

**Files:**
- Create: `go.mod`, `cmd/pcb/main.go`, `internal/cli/root.go`, `internal/cli/version.go`, `internal/output/envelope.go`, `internal/exitcode/exitcode.go`, `internal/buildinfo/buildinfo.go`, `Makefile`, `.golangci.yml`, `.gitignore`, `LICENSE` (MIT, "Copyright (c) 2026 Benjamin Barrois"), `AGENTS.md`, `.github/workflows/ci.yml`
- Modify: `README.md` (add "Status", "Build", "Credits" sections)
- Test: `internal/output/envelope_test.go`, `internal/exitcode/exitcode_test.go`, `internal/cli/version_test.go`

**Interfaces:**
- Produces:
  - `exitcode.Code` (int) constants `OK=0, Usage=2, MustAct=10, Blocked=20, RolledBack=30, Partial=40`; `exitcode.Error{Code Code; Err error; Hint string}` implementing `error` + `Unwrap`; `exitcode.From(err error) Code` (nil→OK, `*Error`→its code, cobra usage errors→Usage, other→Blocked).
  - `output.Envelope{SchemaVersion int "schema_version"; Command string; OK bool; Data any "data,omitempty"; Error *output.ErrorBody "error,omitempty"}`, `output.ErrorBody{Code int; Message string; Hint string "hint,omitempty"}`, `output.SchemaVersion = 1`, `output.Write(w io.Writer, command string, data any, err error) error`.
  - `cli.NewRoot() *cobra.Command` with persistent flag `--json`; `cli.Execute(args []string, stdout, stderr io.Writer) exitcode.Code`.
  - `buildinfo.Version`, `buildinfo.Commit`, `buildinfo.Date` (set by ldflags; `"dev"` defaults).

- [ ] **Step 1: Write failing tests**
  - `TestJSONEnvelopeOnSuccess`: `output.Write(&buf, "version", map[string]string{"version":"dev"}, nil)` → JSON decodes to `schema_version=1, command="version", ok=true`, `data.version="dev"`, no `error` key.
  - `TestJSONEnvelopeOnError`: `cli.Execute([]string{"--json","nope"}, &out, &errb)` returns `exitcode.Usage`; `out` holds exactly one JSON object (decoder hits EOF after it) with `ok=false`, `error.code=2`; `errb` is empty.
  - `TestExitCodeFrom`: table over nil, `&exitcode.Error{Code: MustAct}`, wrapped `fmt.Errorf("x: %w", &exitcode.Error{Code: Partial})`, `errors.New("boom")` → `0,10,40,20`.
  - `TestVersionText` / `TestVersionJSON`: `pcb version` prints `pcb dev (commit dev, built dev)`; `--json` data has keys `version, commit, date, go, os, arch`.
- [ ] **Step 2: Run** `go test ./...` → FAIL (packages missing).
- [ ] **Step 3: Implement** the files above. `main.go` is `os.Exit(int(cli.Execute(os.Args[1:], os.Stdout, os.Stderr)))`. Cobra: `SilenceUsage`, `SilenceErrors`; text-mode errors go to stderr as `Error: <msg>` plus `Hint:` line; JSON mode writes only the envelope. `Makefile`: `build` (ldflags into buildinfo), `test` (`go test -race ./...`), `lint` (gofmt check, `go vet`, `golangci-lint run`). CI: checkout + setup-go (go-version-file) + golangci-lint v2.15.0 via `go install` + `make lint test`, on push to main and PRs. `AGENTS.md`: the Global Constraints above as repo rules (tests must bite, no secrets anywhere, test account only, credit reused logic, commit subject `<emoji> <imperative>`).
- [ ] **Step 4: Run** `make lint test` → PASS; `go run ./cmd/pcb version` prints the text line.
- [ ] **Step 5: Commit** (`✨ Add the pcb skeleton: version, JSON envelope, exit codes, CI`); push the workflow over SSH.

### Task 2: `pcb doctor` skeleton

**Files:**
- Create: `internal/doctor/doctor.go`, `internal/doctor/checks.go`, `internal/paths/paths.go`, `internal/cli/doctor.go`
- Test: `internal/doctor/doctor_test.go`, `internal/paths/paths_test.go`

**Interfaces:**
- Consumes: `output.Write`, `exitcode.Error` (Task 1).
- Produces:
  - `paths.Dirs{Config, State string}`; `paths.Resolve(env func(string) string) Dirs` — `$XDG_CONFIG_HOME/pcb` and `$XDG_STATE_HOME/pcb`, falling back to `~/.config/pcb` and `~/.local/state/pcb` (§7.1). System paths (`/etc/pcb`, `/var/lib/pcb`) come in M5.
  - `doctor.Status` = `"ok" | "warn" | "error" | "skip"`; `doctor.Result{Name, Status, Message, Fix string}`; `doctor.Check interface{ Name() string; Run(ctx context.Context) doctor.Result }`; `doctor.Run(ctx, []doctor.Check) []doctor.Result`; `doctor.Worst([]Result) Status`.
  - Checks: `PlatformCheck` (ok on linux/amd64, linux/arm64; warn elsewhere: "runs, unsupported"), `DirModeCheck{Label, Path string; Want fs.FileMode}` (skip if missing, error if group/other bits beyond `Want`, Fix = `chmod 700 <path>`).
- [ ] **Step 1: Failing tests**: `TestDirModeCheckFlagsGroupReadable` (dir 0750 → error, Fix contains `chmod 700`), `TestDirModeCheckSkipsMissing`, `TestDirModeCheckOK` (0700), `TestPlatformCheck` (inject GOOS/GOARCH: windows/amd64 → warn), `TestDoctorExitCode` (`pcb doctor` with an error result → exit 20, JSON data lists every result in order), `TestResolveXDG` (env set vs unset).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**; `pcb doctor` prints one line per check as it completes (`✓/!/✗/–  name: message`, then `   fix: …`), exit 20 if any error, else 0.
- [ ] **Step 4: Run** `make test` → PASS; `go run ./cmd/pcb doctor` on the workstation shows platform ok, dirs skipped.
- [ ] **Step 5: Commit** (`🩺 Add pcb doctor with platform and directory checks`).

### Task 3: TOTP (RFC 6238) in `internal/otp`

Fresh implementation (inspired by the fork's `pkg/auth/totp.go`; credit it).

**Files:** Create `internal/otp/totp.go`; Test `internal/otp/totp_test.go`

**Interfaces:**
- Produces: `otp.Parse(s string) (*otp.TOTP, error)` (a 6–8 digit code, or `otpauth://totp/…` with `secret`, optional `algorithm` SHA1/SHA256/SHA512, `digits` 6–8, `period` > 0; errors never contain the input); `(*TOTP).Code(t time.Time) string`; `(*TOTP).Wait(t time.Time) time.Duration` (time until a code with ≥ 5 s validity; 0 for a static code).
- [ ] **Step 1: Failing tests**: RFC 6238 appendix B vectors (8 digits, T = 59, 1111111109, 20000000000; SHA1/256/512 with the RFC's ASCII secrets base32-encoded); defaults (SHA1, 6 digits: `287082` at T=59); static code trimmed; rejects `""`, `12ab56`, missing secret, non-base32, `algorithm=MD5`, `digits=4`, `period=0`, `otpauth://hotp/…`; `Wait` at T=28 → 2 s, at T=10 → 0; error text never contains the secret.
- [ ] **Step 2–4:** run (FAIL) → implement (`crypto/hmac`, base32 no-padding, case-insensitive) → run (PASS).
- [ ] **Step 5: Commit** (`🔐 Add RFC 6238 TOTP`).

### Task 4: Lab redaction and recording

**Files:** Create `internal/lab/redact/redact.go`, `internal/lab/record/record.go`; Test `internal/lab/redact/redact_test.go`, `internal/lab/record/record_test.go`

**Interfaces:**
- Produces:
  - `redact.JSON(raw []byte) ([]byte, error)`: walks any JSON; replaces values of secret-bearing keys (case-insensitive: `AccessToken, RefreshToken, UID, ServerProof, ClientProof, ClientEphemeral, SRPSession, Salt, KeySalt, PrivateKey, Passphrase, Token, HumanVerificationToken, SessionKey, KeyPacket, SharedKeyPacket, CalendarKeyPacket, Data, Signature, Armored, TOTP, Password, Email, Address`) with `"<redacted:len=N>"`; every other string value matching a PGP armor header, a JWT shape, or base64 ≥ 40 chars is also redacted; numbers, booleans, IDs of calendars/events (keys ending in `ID` other than `UID`) and `Code`/`Error` stay.
  - `record.Recorder{Dir string}`; `(*Recorder).Save(name string, req record.Exchange) (path string, err error)` with `Exchange{Method, Path string; Query url.Values; Status int; Header http.Header; Body []byte; At time.Time}`; it redacts body and drops all headers except `Content-Type`, `Retry-After`, `X-Pm-Code`; it refuses (`error`) when the redacted body still contains any string registered with `(*Recorder).Forbid(secret string)`.
- [ ] **Step 1: Failing tests**: `TestRedactNeverLeaksKnownSecretFields` (a fixture shaped like a real `/auth/v4` and `/calendar/v1/{id}/events` answer: every listed key redacted, `Code` and calendar `ID` kept); `TestRedactCatchesArmorAnywhere` (armor under an unknown key); `TestRecorderRefusesUnredacted` (Forbid("hunter2"), body `{"Note":"hunter2"}` → error, no file written); `TestRecorderDropsAuthHeaders` (`Authorization`, `x-pm-uid` gone).
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** (`🧪 Add lab redaction and recording`).

### Task 5: Lab login (`pcb-lab login`)

**Files:** Create `cmd/pcb-lab/main.go`, `internal/lab/session/session.go`, `internal/lab/session/guard.go`, `scripts/lab.env.example`; Modify `go.mod` (go-proton-api pin + resty replace); Test `internal/lab/session/session_test.go`

**Interfaces:**
- Consumes: `otp.Parse`, `record.Recorder`, `exitcode.Error`.
- Produces:
  - Env: `PCB_LAB_USERNAME`, `PCB_LAB_PASSWORD`, `PCB_LAB_TOTP` (otpauth URI), `PCB_LAB_APPVERSION` (default `Other` [verified: proton-cal docs/crypto.md]).
  - `session.Login(ctx, session.Options{BaseURL, AppVersion string; Creds session.Creds; CaptchaToken string; Recorder *record.Recorder; StateDir string}) (*session.Session, error)`; on CAPTCHA returns `*exitcode.Error{Code: MustAct}` whose message holds the verify URL and whose hint is `pcb-lab login --captcha-token <token>`, and writes `StateDir/hv.json` `{token, url, at}`.
  - `session.Session{UID, Scopes []string; Client *proton.Client}` persisted to `StateDir/session.json` (0600; test account only) and reloadable with `session.Resume(ctx, opts)`.
  - Guard: `StateDir/last-login` timestamp; `session.Login` returns `*exitcode.Error{Code: Blocked}` if the previous login is < 60 s old.
  - `scripts/lab.env.example`: `PCB_LAB_USERNAME=pass://<vault>/<item>/<field>` etc.; field names confirmed in Step 1.
  - `pcb-lab` commands: `login [--captcha-token T]`, `whoami` (scopes, user ID redacted).
- [ ] **Step 1: Confirm the item's field names** with values never shown: `PROTON_PASS_PERSONAL_ACCESS_TOKEN=<the personal access token, from a file outside the repository> pass-cli login </dev/null`, then `pass-cli item view --vault-name <vault> --item-title <item> --output json | <print keys and types only>`; write the three references into `scripts/lab.env.example`.
- [ ] **Step 2: Failing tests** against an `httptest` front of go-proton-api's in-memory `server` (pattern from proton-cal `pkg/auth/hv_test.go`, rewritten): `TestLoginGuardRefusesWithin60s`; `TestLoginHVStopsWithResumeInfo` (front answers 9001 with token `T1` → exit code 10, hint contains `--captcha-token T1`, `hv.json` written, exactly one `/auth/v4` call); `TestLoginWithCaptchaTokenSendsIt` (header `x-pm-human-verification-token: T1` on the first attempt); `TestLoginTOTPFromURI` (front fakes `2FA.Enabled=1`, receives the code for the injected clock); `TestSessionFileMode0600`.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** → PASS.
- [ ] **Step 6: Live smoke** (one login): `pass-cli run --env-file ~/.config/pcb-live/lab.env -- go run ./cmd/pcb-lab login` then `pcb-lab whoami`; record nothing yet.
- [ ] **Step 7: Commit** (`🧪 Add pcb-lab login against the test account`).

### Task 6: E03 — client identity (go/no-go gate)

**Files:** Create `spec/experiments/E03-client-identity.md`, `spec/experiments/raw/E03/*.json`, `internal/lab/probe/scopes.go` (+ `pcb-lab probe calendar-access`), `spec/decisions/0011-client-identity.md`; Modify `spec/decisions/INDEX.md`, `spec/SPEC.md` §5.1 markers.

**Interfaces:** Consumes `session.Login`, `record.Recorder`. Produces `pcb-lab probe calendar-access`: GET `/calendar/v1`, `/calendar/v1/{first}/bootstrap`, `/settings/calendar`; prints and records status + Proton code for each.

- [ ] **Step 1: Collect candidates**, each with its source file and line: `Other` (proton-cal `docs/crypto.md:62`), the value carbonate uses, the values caloxide `FORK.md` reports (working and failing), and the current web-calendar client string (from WebClients' build config, read only). At most 4 candidates.
- [ ] **Step 2: Run** each candidate once (guard spaces logins ≥ 60 s): `PCB_LAB_APPVERSION=<v> pcb-lab login && pcb-lab probe calendar-access`; record scopes and results.
- [ ] **Step 3: Write E03** (question, candidates + sources, raw results, conclusion).
- [ ] **Step 4: Decision 0011**: the default identity (truthful if one works, else the official one per 0008); if none gets calendar access → stop M0 and report (gate).
- [ ] **Step 5: Update SPEC §5.1** marker to `[verified: E03]`; commit (`🧪 E03: client identity`).

### Task 7: E01 — session lifetime and refresh

**Files:** Create `spec/experiments/E01-session-lifetime.md`, `internal/lab/probe/session.go` (+ `pcb-lab probe refresh`, `pcb-lab probe alive`), `scripts/e01-timer/pcb-lab-e01.{service,timer}`

**Interfaces:** `pcb-lab probe refresh`: refreshes once via go-proton-api, records whether the refresh token changed and whether the old one still works (one retry with it); `pcb-lab probe alive`: one authenticated GET, appends `{at, ok, code}` to `StateDir/e01.jsonl`.

- [ ] **Step 1: Immediate results**: token TTL as returned at login (`ExpiresIn` if present), refresh rotation, old-refresh-token validity.
- [ ] **Step 2: Long probe**: install the user timer (every 6 h, 14 days) running `probe alive` without refreshing; uninstall command documented in the write-up.
- [ ] **Step 3: Write E01** with immediate results and "long probe running since <date>"; feeds C1 refresh policy (M1). Commit (`🧪 E01: session lifetime (immediate results, long probe started)`).

### Task 8: E02 — CAPTCHA token reuse

**Files:** Create `spec/experiments/E02-captcha-token.md`; Modify `internal/lab/session/session.go` (append every HV event to `StateDir/hv-log.jsonl`).

- [ ] **Step 1: Test** `TestHVEventsAreLogged` (two HV answers → two lines with `at`, token length, no token value). Run FAIL → implement → PASS.
- [ ] **Step 2: Protocol in the write-up**: when an HV happens during any lab login, the author solves it; the agent retries from a **new process** after 1 min; if a later HV happens, retry after 30 min. Record outcomes.
- [ ] **Step 3: Write E02** with what happened during M0 (if no HV occurred: "open, no CAPTCHA seen in N logins"; M1 keeps the single-process interactive fallback). Commit (`🧪 E02: captcha token protocol`).

### Task 9: E15 — secret sealing on headless Linux

**Files:** Create `spec/experiments/E15-secret-sealing.md`

- [ ] **Step 1: the desktop** (Arch Linux based): `systemd --version`, `systemd-creds has-tpm2`, `systemd-creds encrypt --user` round-trip of a dummy value (systemd ≥ 256), Secret Service on the session bus (`busctl --user list | grep org.freedesktop.secrets`).
- [ ] **Step 2: the server** (Debian 13, headless; read-only commands over SSH): same checks, as a normal user and as root (`systemd-creds encrypt` system scope with a dummy value in a temp dir, then removed).
- [ ] **Step 3: Write E15**: what is available where, the resulting order for §7.3, and what the container image can use (no systemd → key file + optional docker secret). Update SPEC §7.3 markers. Commit (`🧪 E15: secret sealing`).

### Task 10: Synthetic data set on the test account

Seeds through the proton-cal fork binary as a tool (its write path is verified live); our own write path comes in M4.

**Files:** Create `testdata/seed/dataset.yaml`, `internal/lab/seed/dataset.go`, `internal/lab/seed/plan.go` (+ `pcb-lab seed plan|apply`), `spec/experiments/DATASET.md`; Test `internal/lab/seed/plan_test.go`

**Interfaces:**
- `seed.Load(path) (seed.Dataset, error)`; `seed.Plan(ds seed.Dataset, existing []seed.Existing) []seed.Command`; `seed.Command{Args []string}` = proton-cal argv (no secrets ever in it); every summary is prefixed `pcb-seed <id> ` and `Existing` is parsed from `proton-cal events --all-calendars -o json` over the data set's range.
- Data set (deterministic, generator seed 42): calendars `My calendar` (default), `Work`, `Sport`; 300 timed singles 2021-01-01…2027-06-30 (15 min–3 h; tz Europe/Paris 70 %, America/New_York, Asia/Tokyo, UTC), 40 all-day (single and multi-day), 8 series (daily count 10; weekly until 2027-01-01; every 2 weeks; monthly BYMONTHDAY=15; monthly last Friday `FREQ=MONTHLY;BYSETPOS=-1;BYDAY=FR`; yearly all-day birthday; weekly crossing DST in Europe/Paris; daily in Asia/Tokyo), each with one edited and one deleted occurrence; text cases (emoji/unicode summary, 2 KB description, location with `,;\` and newlines); reminders (none, 15m, 1d, `email:1h`); colours.
- [ ] **Step 1: Failing tests**: `TestDatasetIsDeterministic` (two loads → identical plans); `TestSeedPlanSkipsExisting` (an `Existing` with id `s042` → no command for it); `TestSeedCommandsCarryNoSecrets` (no arg contains `PCB_LAB_` values or `pass://`); `TestDatasetCoverage` (counts: 300 timed, 40 all-day, 8 series, 8 edits, 8 deletions, ≥ 1 DST-crossing).
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Apply**: build the fork binary at its current `main`; run `pcb-lab seed apply` with the fork's own env-credential login (`PROTON_CAL_*` from the same pass references), 1 write per 2 s, stop on the first 429 and resume later.
- [ ] **Step 6: Write DATASET.md** (content, how to reseed, how to wipe: delete `pcb-seed`-tagged events). Commit (`🌱 Seed the test account with the synthetic data set`).

### Task 11: M0 wrap-up

- [ ] Update `spec/SPEC.md` status markers settled by E01/E02/E03/E15, `spec/OPEN.md`, `spec/decisions/INDEX.md`; `README.md` Credits lists every source used so far (proton-cal + fork for TOTP and login flow ideas, go-proton-api test server pattern).
- [ ] `make lint test` green in CI; commit (`📝 Close M0`).
