# M1 Proton Core (Read) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pcb login` (password, password + TOTP, CAPTCHA two-step and interactive wait, opt-in unattended relogin), `pcb logout`, `pcb status` and `pcb calendar list` working in-process against Proton, with sessions and the salted key passphrase sealed on disk; proven offline against a fake Proton, then once live against the test account.

**Architecture:** C1 is the `internal/proton/...` tree: `api` (our own authenticated JSON transport: identity headers, refresh on 401, rate limit, `Retry-After`), `auth` (SRP, human verification, 2FA, refresh, locked scope, logout through go-proton-api's `Manager`), `keys` (salts → salted key passphrase → user/address keyrings) and `calendar` (list, default calendar, bootstrap → member passphrase → calendar keyring). C2 is `internal/store` (sealed secrets behind a `seal.Sealer`, key-file backend; a plain login-state file). C3 is `internal/service`: a request/response `service.API` that the CLI calls in-process now (`service.Local`) and over the control socket in M5, unchanged. The CLI never imports C1 or C2.

**Tech Stack:** Go 1.27, cobra v1.10.2, go-proton-api `v0.0.0-20261022084531-3f8fbebac58d` (Manager for SRP/2FA/refresh, `server` for the fake, `Unlock`, `Salts`), gopenpgp v2, go-srp, pelletier/go-toml/v2 (strict TOML), golang.org/x/time/rate, golang.org/x/term.

**Spec:** `spec/SPEC.md` draft v2, §4, §5.1–5.4, §7.1, §7.3, §13, §14, §16, §18 M1, §19; decisions 0005, 0008, 0010, 0011 (being written in M0 Task 6). Reference (audit, never copy): proton-cal `docs/{overview,crypto,api,libraries}.md`, fork `pkg/{auth,papi,calendar,config}`.

**Out of scope (YAGNI, say so in review):** C0 — `calendar list` decodes no event card, so the JSCalendar model and the Proton card codec move to M2/M3 where events are read; the SQLite cache, sync, change feed (M2); the daemon and control socket (M5); the systemd-creds and Secret Service sealers (after E15); the circuit breaker (with sync, M2); `pcb login resume --totp CODE` (needs a process holding the half-authenticated SRP session across commands, i.e. the daemon, M5; in M1 TOTP comes from `PCB_TOTP` or a prompt in the same process); `pcb config get|set|validate`; the event read endpoints (windowed query, full listing, get by ID/UID: consumed first by sync, M2); any calendar-list cache (the fork's 7-day stale list is the anti-pattern).

## Global Constraints

- Module `github.com/Barben360/proton-calendar-bridge`, `go 1.27`; binary `pcb`; gate `make lint test` (AGENTS.md).
- CLI (§13): every command has `--json` printing one envelope `{schema_version, command, ok, data | error}`; exit codes `0` done, `2` bad invocation, `10` you must act, `20` blocked by the machine, `30` failed and rolled back, `40` partly done and resumable; `--non-interactive` never prompts and exits 10 naming what is missing.
- Secrets only from a terminal prompt, a file descriptor (`--password-fd`) or the environment (`PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`: a code or an otpauth URI); never argv; never in logs, errors or unsealed files (§5.2, §19).
- Base URL `https://mail-api.proton.me` [verified: proton-cal docs/crypto.md]; pass it explicitly to go-proton-api (its default host is `https://mail.proton.me/api`). `x-pm-appversion` default `Other`, configurable (decision 0011, provisional; 0008 allows an official value if needed). `User-Agent: pcb/<buildinfo.Version>`.
- Login modes (0010): password, password + TOTP. Two-password mode and FIDO2-only are refused with a clear message, and the session they opened is revoked.
- CAPTCHA (§5.2): `https://verify.proton.me/?methods=captcha&token=<url-escaped token>`; the retry carries `x-pm-human-verification-token: <token>` and `x-pm-human-verification-token-type: captcha`. Proton code `9001`.
- Locked scope (§5.2): salts may need it (403, code `9101`); gain it by SRP of the login password to `PUT /core/v4/users/unlock` `{ClientEphemeral, ClientProof, SRPSession}`, verify `ServerProof`, then `PUT /core/v4/users/lock`. Elevate on code 9101 only, never on a bare 403.
- Keys (§5.3): unlocked keys in memory only; the salted key passphrase is sealed in C2.
- Layout (§7.1): `$XDG_STATE_HOME/pcb/` (dirs 0700, files 0600): `secrets.sealed`, `state.json`, `last-login`, `login.lock`, `secrets.lock`; config `$XDG_CONFIG_HOME/pcb/config.toml`; key file `$XDG_CONFIG_HOME/pcb/secrets.key` (0600, separate path, §7.3).
- Config (§14): strict TOML, unknown keys refused, invalid values refused (never silently defaulted).
- Good citizen (§2): at most one login attempt per 60 s per state dir, checked and stamped under a lock before any network call; honour `Retry-After`; exponential backoff with jitter on 429/5xx; writes never retried.
- One account per process in v1 (§13); every stored record is keyed by account ID = Proton user ID.
- Adapters and the CLI call only `service.API` (§4): `internal/cli` never imports `internal/proton/...` or `internal/store` (enforced by a test in Task 10). Every error a `service.API` method returns is an `*exitcode.Error` with a secret-free message and a hint (socket-ready).
- go-proton-api is imported as `gpa` in product code (our C1 tree is `internal/proton`).
- Live Proton calls only against the test account (0005); every live step is a deferred **Live verification** step, run in the Task 11 live session after the author solves the CAPTCHA (`spec/HUMAN-CHECKS.md`); never a blocker. All logic is proven offline with `internal/proton/protontest`.
- Tests bite (AGENTS.md); reused logic audited, rewritten, credited in `README.md`; never copy ProtonMail/WebClients (GPL-3.0); commit subjects `<emoji> <imperative>`; workflow files pushed over SSH.

## Review Focus

1. **Two `pcb` commands at once with an expired access token** (cron + a human) → exactly one `POST /auth/v4/refresh`, both commands succeed, the store holds the newest tokens (refresh tokens are single-use [verified: proton-cal docs/overview.md]); pinned by `TestConcurrentRefreshAcrossProcessesRefreshesOnce` (Task 9).
2. **Proton down or flaky during a refresh** (5xx, timeout, DNS) → exit 20, session and state untouched, no unattended relogin, no logout; pinned by `TestTransientRefreshFailureKeepsSession` (Task 9).
3. **Key file deleted, replaced or loosened while `secrets.sealed` exists** → exit 20 naming the key file; the store is never re-sealed under a new key; `pcb logout` still wipes local state; pinned by `TestMissingKeyNeverOverwritesStore` (Task 3) and `TestLogoutUnreadableStoreWipesLocally` (Task 8).
4. **Unattended relogin answered by a CAPTCHA or a rejected password** → exactly one attempt, state `needs-captcha` / `needs-login`, no further automatic attempt until a human logs in; pinned by `TestReloginStopsAfterHumanNeeded` (Task 9).
5. **A password piped through `--password-fd` with a trailing newline or CRLF, or empty** → exactly one trailing `\n` or `\r\n` stripped, inner spaces kept, empty refused with exit 2 before any network call; pinned by `TestPasswordFDStripsOneNewline` (Task 10).

---

### Task 1: Shared file primitives extracted from the lab

The lab's atomic write, flock and 60 s guard become product packages; the lab uses them (behaviour unchanged).

**Files:**
- Create: `internal/atomicfile/atomicfile.go`, `internal/filelock/filelock_unix.go`, `internal/filelock/filelock_other.go`, `internal/loginguard/loginguard.go`
- Modify: `internal/lab/session/session.go` (use them); delete `internal/lab/session/{guard.go,lock_unix.go,lock_other.go}`; `internal/lab/session/session_test.go` (lock message)
- Test: `internal/atomicfile/atomicfile_test.go`, `internal/filelock/filelock_test.go`, `internal/loginguard/loginguard_test.go`

**Interfaces:**
- Produces:
  - `atomicfile.Write(path string, data []byte) error` — temp file in the same dir, mode 0600, fsync, rename, best-effort dir fsync (the lab's `writeFile`).
  - `filelock.TryLock(path string) (unlock func(), ok bool, err error)`; `filelock.Lock(ctx context.Context, path string) (unlock func(), err error)` (retries `TryLock` every 50 ms until ctx ends); off Unix both return `filelock.ErrUnsupported`.
  - `loginguard.Acquire(dir string, min time.Duration, now time.Time) (release func(), err error)`: `TryLock(dir/login.lock)` (busy → `*exitcode.Error{Code: Blocked, Err: "another login is running"}`); `dir/last-login` missing → allowed, malformed → Blocked (fail closed, hint names the file); younger than `min` → `*exitcode.Error{Code: Blocked, Err: &loginguard.TooSoon{Wait}, Hint: "retry in Ns"}` without restamping; else stamps `now` (RFC 3339 nano, before the caller's network call) and returns `release` (unlocks; the caller holds it for the whole attempt).
  - `loginguard.WaitFor(err error) (time.Duration, bool)` — the `TooSoon` wait in an error chain; `loginguard.MinInterval = 60 * time.Second` (lab and product).
- [ ] **Step 1: Failing tests**: `TestAcquireRefusesWithin` (stamp 59 s old → Blocked, file unchanged), `TestAcquireAllowsAfter` (61 s → stamp = now), `TestMalformedStampFailsClosed`, `TestConcurrentAcquireOneWins` (8 goroutines → 1 ok, 7 Blocked), `TestWaitForExtractsWait` (stamp 20 s old, min 60 s → 40 s); `TestWriteMode0600AndReplaces`, `TestWriteLeavesNoTempOnFailure` (read-only dir → error, no `.tmp-*`); `TestLockWaitsForRelease`, `TestLockHonoursContext` (held lock + 100 ms ctx → `context.DeadlineExceeded`).
- [ ] **Step 2: Run** `go test ./internal/atomicfile/ ./internal/filelock/ ./internal/loginguard/` → FAIL (packages missing).
- [ ] **Step 3: Implement**, then switch `lab/session` to them with `loginguard.MinInterval`; update `TestLoginRefusedWhileLockHeld` to expect "another login is running".
- [ ] **Step 4: Run** `make lint test` → PASS (all M0 lab tests still green).
- [ ] **Step 5: Commit** (`♻️ Extract atomic writes, file locks and the login guard from the lab`).

### Task 2: Configuration (`config.toml`)

**Files:**
- Create: `internal/config/config.go`
- Modify: `go.mod` (`github.com/pelletier/go-toml/v2` direct), `internal/doctor/checks.go` (`ConfigCheck`), `internal/cli/doctor.go` (add it)
- Test: `internal/config/config_test.go`, `internal/doctor/doctor_test.go`

**Interfaces:**
- Consumes: `paths.Dirs`, `exitcode.Error`.
- Produces:
  - `config.Config{Proton config.Proton "toml:proton"; Secrets config.Secrets "toml:secrets"}`, `config.Proton{BaseURL string "toml:base_url"; AppVersion string "toml:app_version"}`, `config.Secrets{KeyFile string "toml:key_file"}`.
  - `config.DefaultBaseURL = "https://mail-api.proton.me"`, `config.DefaultAppVersion = "Other"`.
  - `config.Defaults(d paths.Dirs) config.Config` (KeyFile `d.Config/secrets.key`); `config.Path(d paths.Dirs) string` (`d.Config/config.toml`).
  - `config.Load(path string, d paths.Dirs) (config.Config, error)`: missing file → `Defaults`; present keys override defaults; errors are `*exitcode.Error{Code: MustAct, Hint: "fix <path>"}`. Refused: unknown keys (`DisallowUnknownFields`), malformed TOML, `app_version` empty or with control characters, `base_url` not `https://` (plain `http://` only for hosts `127.0.0.1`, `::1`, `localhost`), relative `key_file`.
  - `doctor.ConfigCheck{Path string; Dirs paths.Dirs}`: ok (path or "defaults, no file"), error with the load message and `Fix: "edit <path>"`.
- [ ] **Step 1: Failing tests**: `TestLoadMissingFileGivesDefaults` (base URL and app version equal the constants); `TestLoadOverridesEachKey`; `TestLoadRefusesUnknownKey` (`[proton]\nappversion = "x"` → error naming `appversion`); `TestLoadRefusesEmptyAppVersion` (`app_version = ""` → error, not the default); `TestLoadRefusesPlainHTTPRemote` (`http://example.com` refused, `http://127.0.0.1:8080` accepted); `TestLoadRefusesRelativeKeyFile`; `TestDoctorConfigCheckReportsBadFile`.
- [ ] **Step 2–4:** run (FAIL) → implement → run `make lint test` (PASS).
- [ ] **Step 5: Commit** (`⚙️ Add strict config.toml with the Proton identity`).

### Task 3: C2 sealed secrets and login state

E15 has not run, so only the key-file sealer exists; systemd-creds and the Secret Service plug into `seal.Sealer` after E15.

**Files:**
- Create: `internal/store/seal/seal.go`, `internal/store/seal/keyfile.go`, `internal/store/secrets.go`, `internal/store/state.go`
- Modify: `internal/doctor/checks.go` (`FileModeCheck`, `SecretsCheck`), `internal/cli/doctor.go`
- Test: `internal/store/seal/keyfile_test.go`, `internal/store/secrets_test.go`, `internal/store/state_test.go`, `internal/doctor/doctor_test.go`

**Interfaces:**
- Consumes: `atomicfile.Write`, `filelock.Lock` (Task 1).
- Produces:
  - `seal.Sealer interface{ Name() string; Seal(plaintext, aad []byte) ([]byte, error); Open(sealed, aad []byte) ([]byte, error) }`; `seal.ErrNoKey`, `seal.ErrCorrupt`.
  - `seal.KeyFile{Path string}`: 32 random bytes; `Seal` creates the key (`O_EXCL`, 0600, parent 0700) only if absent; `Open` with no key → `ErrNoKey`; a key file with group/other bits → error with `Fix chmod 600 <path>` (fail closed); AES-256-GCM, 12-byte random nonce, output `nonce || ciphertext`; authentication failure → `ErrCorrupt`. `Name()` = `"keyfile"`.
  - `store.Account{ID, Username, UID, AccessToken, RefreshToken string; KeyPass []byte; Relogin *store.Relogin}`, `store.Relogin{Password, TOTPURI string}`; `Account` and `Relogin` implement `String`, `GoString`, `LogValue` printing `store.Account(id=<first 4>…, redacted)`.
  - `store.Secrets{Path string; LockPath string; Sealer seal.Sealer}`; `store.NewSecrets(stateDir string, s seal.Sealer) *store.Secrets` (`secrets.sealed`, `secrets.lock`).
  - `(*Secrets).Load() (map[string]store.Account, error)` (missing file → empty map); `(*Secrets).Update(ctx context.Context, fn func(accounts map[string]store.Account) error) error` — takes `filelock.Lock(LockPath)`, loads, runs `fn`, seals, `atomicfile.Write`; an empty map removes the file; if the file exists and cannot be opened, returns the open error and writes nothing. `(*Secrets).Wipe() error` removes the file (for an unreadable store).
  - Sealed file: JSON `{"version":1,"sealer":"keyfile","data":"<base64>"}`; AAD `pcb-secrets/v1/<sealer>`; a sealer mismatch is an error.
  - `store.Phase` = `"logged-out" | "ready" | "needs-captcha" | "needs-login"`; `store.State{AccountID string; Phase store.Phase; Reason string; Captcha *store.Captcha; Unattended bool; LastLogin, LastRelogin time.Time}` (JSON snake_case, zero times omitted), `store.Captcha{URL, Token string; At time.Time}`; `store.LoadState(stateDir string) (store.State, error)` (missing → `Phase: logged-out`; malformed → error, fail closed); `store.SaveState(stateDir string, s store.State) error` (0600). No username, token or password in `state.json`.
  - `doctor.FileModeCheck{Label, Path string; Want fs.FileMode}` (skip if missing, error + `chmod` fix beyond `Want`); `doctor.SecretsCheck{StateDir, KeyFile string}`: warn "secrets sealed with a key file (headless fallback)"; when `state.Unattended` also warn "unattended relogin is on: whoever can read <key file> and <state dir>/secrets.sealed owns the Proton account".
- [ ] **Step 1: Failing tests**:
  - seal: `TestKeyFileRoundTrip`; `TestKeyFileCreatesKeyOnce` (two `KeyFile` values on one path open each other's output; key mode 0600); `TestOpenWithoutKeyIsErrNoKey`; `TestKeyFileRefusesLooseMode` (0644 → error with `chmod 600`); `TestTamperedIsErrCorrupt` (flip one byte); `TestAADBindsContext` (sealed with aad A, opened with B → `ErrCorrupt`).
  - store: `TestSecretsRoundTripTwoAccounts`; `TestSealedFileHoldsNoPlaintext` (file bytes contain none of username, tokens, password, TOTP URI, key pass); `TestMissingKeyNeverOverwritesStore` (save, delete the key file, `Update` → `errors.Is(err, seal.ErrNoKey)`, sealed bytes unchanged, no new key file); `TestWipeRemovesUnreadableStore`; `TestConcurrentUpdatesSerialize` (two goroutines each add an account → both present); `TestAccountNeverPrintsSecrets` (`%v`, `%+v`, `%#v`, slog JSON of an `Account` contain no secret value); `TestStateMissingIsLoggedOut`, `TestStateMalformedFailsClosed`, `TestStateMode0600`.
  - doctor: `TestSecretsCheckWarnsKeyFile`, `TestSecretsCheckWarnsUnattended`, `TestFileModeCheckFlagsGroupReadable`.
- [ ] **Step 2–4:** run (FAIL) → implement (`pcb doctor` adds file-mode checks for the key file and `secrets.sealed`, and `SecretsCheck`) → run `make lint test` (PASS).
- [ ] **Step 5: Commit** (`🔐 Add sealed secrets with a key-file backend`).

### Task 4: C1 transport (`internal/proton/api`)

go-proton-api's client has no raw request path and stale calendar types [verified: proton-cal docs/libraries.md], so every authenticated call goes through this one client; go-proton-api is kept for SRP, 2FA and refresh only (Task 5), so there is a single owner of the tokens.

**Files:**
- Create: `internal/proton/api/client.go`, `internal/proton/api/errors.go`
- Modify: `go.mod` (`golang.org/x/time`)
- Test: `internal/proton/api/client_test.go` (plain `httptest`)

**Interfaces:**
- Produces:
  - `api.Tokens{UID, AccessToken, RefreshToken string}` with redacting `String`/`GoString`/`LogValue`.
  - `api.Options{BaseURL, AppVersion, UserAgent string; HTTP *http.Client; Refresh func(ctx context.Context, stale api.Tokens) (api.Tokens, error); Limiter *rate.Limiter; Sleep func(ctx context.Context, d time.Duration) error}`; defaults: 30 s timeout, limiter `rate.Every(250*time.Millisecond)` burst 8 [call; E06 tunes], real sleep.
  - `api.New(t api.Tokens, o api.Options) *api.Client`; `(*Client).Do(ctx, method, path string, query url.Values, body, out any) error`; `Get(ctx, path, query, out)`, `Put(ctx, path, body, out)`, `Delete(ctx, path, out)`; `(*Client).Tokens() api.Tokens`.
  - Headers: `x-pm-uid`, `Authorization: Bearer <access>`, `x-pm-appversion`, `User-Agent`, `Accept: application/vnd.protonmail.v1+json`, `Content-Type: application/json` only with a body.
  - `api.Error{Status, Code int; Message string; RetryAfter time.Duration}`; `Error()` = `Proton: <Message> (code <Code>, HTTP <Status>)`; built from `{"Code","Error"}` on any non-2xx, and on a 2xx whose `Code` is not 1000/1001. `api.IsCode(err error, code int) bool`. Constants `CodeOK=1000, CodeMulti=1001, CodeNotFound=2501, CodeHumanVerification=9001, CodeInsufficientScope=9101, CodeRefreshInvalid=10013`.
  - `api.ErrSessionEnded` — returned (wrapped) when a request is still 401 after one refresh, or when `Refresh` returns an error wrapping it.
  - Behaviour: on 401, one in-process refresh (mutex; a request whose tokens were already rotated by another goroutine just retries), then one retry. 429: wait `Retry-After` if ≤ 60 s, at most 3 attempts; longer → return `*api.Error` with `RetryAfter`, no wait. 5xx and network errors: GET only, 3 attempts, backoff 1 s then 2 s with ±20 % jitter; other methods never retried. Errors never include the request body, the query or any header value.
- [ ] **Step 1: Failing tests**: `TestSendsIdentityHeaders` (each header exact; no `Content-Type` on GET); `TestDecodesProtonError` (422 `{"Code":2501,"Error":"Calendar does not exist"}` → Status 422, Code 2501, `IsCode`; 200 `{"Code":2001}` → error); `TestRefreshOn401ThenRetryOnce` (`Refresh` gets the stale tokens, retry uses the new access token, `Tokens()` updated); `TestSecond401IsSessionEnded`; `TestConcurrent401sRefreshOnce` (10 goroutines → `Refresh` called once, all succeed); `TestRetryAfterHonoured` (429 `Retry-After: 3` then 200 → injected sleep got 3 s, 2 requests); `TestRetryAfterTooLongFails` (600 → 1 request, no sleep, `RetryAfter` = 600 s); `TestGET5xxBacksOff` (503, 503, 200 → 3 requests, sleeps in [0.8 s, 1.2 s] then [1.6 s, 2.4 s]); `TestPUTNeverRetried` (503 → 1 request); `TestErrorsCarryNoSecrets` (closed server; error text contains neither the access token nor a body field value `hunter2`); `TestLimiterSpacesRequests` (`rate.Every(20ms)` burst 1, 3 requests ≥ 40 ms).
- [ ] **Step 2–4:** run (FAIL) → implement → run (PASS).
- [ ] **Step 5: Commit** (`🌐 Add the Proton API transport`).

### Task 5: Fake Proton and C1 auth

The fake generalises the M0 lab front (`internal/lab/session/session_test.go`) into a reusable test package for every C1/C3/CLI test.

**Files:**
- Create: `internal/proton/protontest/fake.go`, `internal/proton/auth/login.go`, `internal/proton/auth/refresh.go`, `internal/proton/auth/scope.go`
- Modify: `go.mod` (gopenpgp, go-srp direct)
- Test: `internal/proton/auth/login_test.go`, `internal/proton/auth/scope_test.go`

**Interfaces:**
- Consumes: `api.Client`, `api.Tokens`, `api.ErrSessionEnded` (Task 4).
- Produces:
  - `protontest.New(t testing.TB, o protontest.Options) *protontest.Fake` over `server.New(server.WithTLS(false))` plus an `httptest` front; `Options{TwoFA, FIDO2Only, TwoPassword, LockedSalts, BadUnlockProof bool; Captcha string; CaptchaMethods []string /* default ["captcha"] */; Now func() time.Time}`; `Fake{URL, Username, Password, TOTPURI string}` (the fake's own throwaway values); `(*Fake).Calls(method, pathPrefix string) int`; `(*Fake).FailNext(method, path string, status, code int, retryAfter int)`; `(*Fake).ExpireAccessTokens()` (current access tokens answer 401 until refreshed); `(*Fake).RevokeSessions()` (every existing UID answers 401, refresh answers 422/10013); `(*Fake).AddUser(username, password string) (userID string)`; `(*Fake).UserID(username string) string`.
  - Fake rules: `Captcha` non-empty → `POST /auth/v4` answers 422/9001 with that token unless the request carries it in `x-pm-human-verification-token`; `TwoFA` → the auth answer has `2FA.Enabled = 1` and `/auth/v4/2fa` accepts the codes of the current and previous 30 s period of `TOTPURI` at `Now`, else 422/12060 [code assumed; message is what tests check]; `FIDO2Only` → `Enabled = 2`; `TwoPassword` → `PasswordMode = 2`; `LockedSalts` → `GET /core/v4/keys/salts` answers 403/9101 until a successful unlock on that UID; `PUT /core/v4/users/unlock` verifies the SRP proof by replaying it to the backend's `POST /auth/v4` and returns its `ServerProof` (wrong bytes with `BadUnlockProof`); `PUT /core/v4/users/lock` re-locks.
  - `auth.NewManager(baseURL, appVersion string) *gpa.Manager` (`WithHostURL`, `WithAppVersion`, silent logger).
  - `auth.Begin(ctx, m *gpa.Manager, username string, password []byte, captchaToken string) (*auth.Pending, error)`; errors: `*auth.CaptchaError{Token, URL string}`, `auth.ErrBadCredentials` (Proton 8002 or a 422 on `/auth/v4`), `auth.ErrTwoPassword`, `auth.ErrFIDO2`, `*auth.UnsupportedVerificationError{Methods []string}` (9001 without `captcha`); for two-password and FIDO2-only the opened session is revoked before returning.
  - `auth.Pending{Tokens api.Tokens; UserID string; NeedsTOTP bool}`; `(*Pending).SubmitTOTP(ctx, code string) error` (rejection → `auth.ErrTOTPRejected`); `(*Pending).Abort(ctx)` (best-effort `DELETE /auth/v4`, then close); `(*Pending).Close()`. Token rotations seen by the go-proton-api client are copied into `Pending.Tokens`.
  - `auth.VerifyURL(token string) string`.
  - `auth.Refresh(ctx, m *gpa.Manager, stale api.Tokens) (api.Tokens, error)` via `m.NewClientWithRefresh` (client closed at once); a `*gpa.APIError` with 4xx status other than 429 → wraps `api.ErrSessionEnded`; anything else is transient.
  - `auth.Scope{M *gpa.Manager; C *api.Client; Username string; Password []byte}` with `Unlock(ctx) error` (SRP proof to `/core/v4/users/unlock`, `ServerProof` mismatch → `auth.ErrServerProof`) and `Lock(ctx) error`.
  - `auth.Logout(ctx, c *api.Client) error` (`DELETE /auth/v4`).
- [ ] **Step 1: Failing tests**: `TestBeginPasswordOnly` (`NeedsTOTP` false, tokens set, one `POST /auth/v4`); `TestBeginCaptchaReturnsChallenge` (token `T1`, URL `https://verify.proton.me/?methods=captcha&token=T1`, one `/auth/v4`); `TestBeginWithCaptchaTokenSendsBothHeaders`; `TestBeginNonCaptchaMethodsUnsupported` (`["sms","email"]` listed in the error); `TestBeginWrongPassword` (`ErrBadCredentials`, text lacks the password); `TestBeginRefusesTwoPassword` and `TestBeginRefusesFIDO2Only` (error + exactly one `DELETE /auth/v4`); `TestSubmitTOTPAcceptsCurrentCode` / `TestSubmitTOTPRejectsWrongCode`; `TestRefreshRotates`; `TestRefreshAfterRevokeIsSessionEnded`; `TestRefreshNetworkErrorIsTransient` (closed server → not `ErrSessionEnded`); `TestScopeUnlockThenSaltsThenLock` (`LockedSalts`: salts 9101 before, 200 after `Unlock`, 9101 again after `Lock`); `TestScopeUnlockRejectsBadServerProof`; `TestLogoutRevokes` (same tokens → 401 afterwards).
- [ ] **Step 2–4:** run (FAIL) → implement → run (PASS).
- [ ] **Step 5: Commit** (`🔑 Add Proton login, 2FA, refresh and scope elevation`).
- [ ] **Step 6: Live verification (deferred to Task 11)**: the first login gets a CAPTCHA whose URL opens and solves; the resume from a new process is accepted (E02 data point: delay, process); TOTP accepted; scopes after 2FA noted.

### Task 6: C1 keys (salts, key passphrase, user and address keys)

**Files:**
- Create: `internal/proton/keys/keys.go`
- Test: `internal/proton/keys/keys_test.go`

**Interfaces:**
- Consumes: `api.Client`, `api.IsCode`, `api.CodeInsufficientScope` (Task 4); `protontest`, `auth.Begin`, `auth.Scope` (Task 5, tests and service wiring).
- Produces:
  - `keys.Elevator interface{ Unlock(ctx) error; Lock(ctx) error }` (satisfied by `auth.Scope`).
  - `keys.Fetch(ctx, c *api.Client) (gpa.User, []gpa.Address, error)` (`/core/v4/users`, `/core/v4/addresses`, addresses sorted by `Order`).
  - `keys.FetchSalts(ctx, c *api.Client, e keys.Elevator) (gpa.Salts, error)`: on code 9101 only, `e.Unlock`, one retry, then `e.Lock` (a lock failure is returned only if the fetch succeeded).
  - `keys.DeriveKeyPass(user gpa.User, addrs []gpa.Address, salts gpa.Salts, password []byte) ([]byte, error)`: salt of the primary user key (first key if none is primary); verified by `gpa.Unlock` yielding ≥ 1 address keyring; else `keys.ErrKeysLocked`.
  - `keys.Unlocked{User gpa.User; Addresses []gpa.Address; AddrKRs map[string]*crypto.KeyRing}`; `keys.Unlock(user gpa.User, addrs []gpa.Address, keyPass []byte) (*keys.Unlocked, error)` (`ErrKeysLocked` on failure); `(*Unlocked).Close()` clears private parameters.
- [ ] **Step 1: Failing tests**: `TestDeriveKeyPassUnlocksAddressKeys` (`AddrKRs` holds the fake user's address ID and decrypts a message encrypted to that address's public key); `TestDeriveKeyPassWrongPassword` (`ErrKeysLocked`, nil pass); `TestFetchSaltsElevatesOnlyOn9101` (counting elevator: `LockedSalts` → Unlock 1, Lock 1; default → 0, 0); `TestFetchSaltsOther403DoesNotElevate` (`FailNext` salts 403/2011 → error, Unlock 0); `TestUnlockWithStoredKeyPassAfterRefresh` (derive on the login session, unlock on a refreshed one).
- [ ] **Step 2–4:** run (FAIL) → implement → run (PASS).
- [ ] **Step 5: Commit** (`🗝️ Unlock user and address keys from the salted key passphrase`).
- [ ] **Step 6: Live verification (deferred to Task 11)**: whether a fresh session's salts answer 9101 (the fork says fresh sessions hold the scope); if so the unlock/lock dance succeeds; the derived pass unlocks the test account's address keys.

### Task 7: C1 calendars (list, default, bootstrap unlock)

**Files:**
- Create: `internal/proton/calendar/list.go`, `internal/proton/calendar/unlock.go`, `internal/proton/protontest/calendars.go`
- Test: `internal/proton/calendar/list_test.go`, `internal/proton/calendar/unlock_test.go`

**Interfaces:**
- Consumes: `api.Client` (Task 4), `keys.Unlocked` (Task 6), `protontest.Fake` (Task 5).
- Produces:
  - `protontest.Calendar{Name, Description, Color string; Type int; LegacyTopLevel bool; ToSecondAddress bool; BrokenPassphrase bool; ExtraLockedKey bool}`; `(*Fake).AddCalendar(username string, c protontest.Calendar) (id string)` — on first use per user the fake reads the user's address public keys from the backend with the request's auth headers, generates a calendar key (x25519) locked with a random base64 passphrase, encrypts that passphrase to the chosen address key (`BrokenPassphrase`: to an unrelated key), and serves `GET /calendar/v1` (metadata on `Members[0]`, or top-level with `LegacyTopLevel`), `GET /calendar/v2/{id}/bootstrap` (`Keys`, `Passphrase.MemberPassphrases`, `Members`, `CalendarSettings`), `GET /settings/calendar`; `(*Fake).SetDefaultCalendar(username, id string)`; `(*Fake).EncryptToCalendar(id string, plaintext []byte) string` (armored, for tests).
  - `calendar.Type int` with `Normal=0, Subscribed=1, Holidays=2`; `(Type).String()` → `normal|subscribed|holidays|type N`; `(Type).ReadOnly() bool` — true for every type except `Normal` (unknown types fail closed).
  - `calendar.Calendar{ID, Name, Description, Color string; Type calendar.Type; MemberID, AddressID, Email string; Permissions int}`.
  - `calendar.List(ctx, c *api.Client) ([]calendar.Calendar, error)` (API order; member metadata first, top-level fields as fallback, nothing invented); `calendar.DefaultID(ctx, c *api.Client) (string, error)` (`CalendarUserSettings.DefaultCalendarID`, `""` when unset).
  - `calendar.Settings{DefaultEventDuration, MakesUserBusy int; DefaultPartDayNotifications, DefaultFullDayNotifications []calendar.Notification}`, `calendar.Notification{Type int; Trigger string}`.
  - `calendar.Access{CalendarID, MemberID, AddressID string; KR *crypto.KeyRing; Settings calendar.Settings}`, `(*Access).Close()`; `calendar.Unlock(ctx, c *api.Client, u *keys.Unlocked, cal calendar.Calendar) (*calendar.Access, error)`: bootstrap (ID path-escaped); member = `cal.MemberID`, else the member whose email matches one of our addresses (case-insensitive); that member's passphrase entry (no fallback to another member's); decrypt with each address keyring in address order (`GetBinary`, never `GetString`); keep every calendar key that unlocks, error if none. The member passphrase signature is **not** verified in M1 (read only; E10 pending; M4 must verify before encrypting to a calendar key) — a comment says so.
- [ ] **Step 1: Failing tests**: `TestListResolvesMemberMetadata` (name, colour from the member; types 0/1/2 → `normal/subscribed/holidays`, `ReadOnly` false/true/true); `TestListLegacyTopLevelFallback`; `TestUnknownTypeIsReadOnly` (7 → `type 7`, read-only); `TestDefaultID` (set → ID; unset → `""`); `TestUnlockDecryptsWithCalendarKey` (`Access.KR` decrypts `EncryptToCalendar(id, "probe")` to `probe`); `TestUnlockPassphraseToSecondAddress`; `TestUnlockSkipsUndecryptableKey` (`ExtraLockedKey` → one key in `KR`, no error); `TestUnlockBrokenPassphraseErrors` (error names the calendar ID, contains no armor); `TestUnlockUnknownCalendar` (`FailNext` bootstrap 422/2501 → `IsCode(err, 2501)`).
- [ ] **Step 2–4:** run (FAIL) → implement → run (PASS).
- [ ] **Step 5: Commit** (`📅 List calendars and unlock calendar keys`).
- [ ] **Step 6: Live verification (deferred to Task 11)**: the test account's calendars, their types, the default; every calendar's keys unlock (incl. a holidays and a subscribed calendar the author adds); redacted fixtures of `/calendar/v1`, `/settings/calendar`, one bootstrap recorded and decoded by a contract test.

### Task 8: C3 service — login, logout, status

**Files:**
- Create: `internal/service/api.go`, `internal/service/local.go`, `internal/service/login.go`, `internal/service/errors.go`
- Modify: `internal/proton/protontest/fake.go` (only if a test needs a missing hook)
- Test: `internal/service/login_test.go`

**Interfaces:**
- Consumes: Tasks 1–7 (`loginguard.Acquire`, `config.Config`, `store.*`, `seal.KeyFile`, `auth.*`, `keys.*`).
- Produces:
  - `service.API interface{ Login(ctx, service.LoginRequest) (service.LoginResult, error); SubmitTOTP(ctx, code string) (service.LoginResult, error); Logout(ctx) (service.LogoutResult, error); Status(ctx) (service.Status, error); Calendars(ctx) ([]service.Calendar, error); Close() error }`. All request/result types are plain JSON structs (snake_case tags) — the M5 socket carries them unchanged.
  - `service.LoginRequest{Username, Password, TOTP, CaptchaToken string; UnattendedRelogin bool}` (redacting `String`/`GoString`/`LogValue`).
  - `service.LoginState` = `"ready" | "needs-totp" | "needs-captcha"`; `service.LoginResult{State service.LoginState; Username, AccountID, CaptchaURL, CaptchaToken string}`.
  - `service.LogoutResult{WasLoggedIn, Revoked bool}`.
  - `service.Status{Phase, Reason, Username, AccountID, Secrets, KeyFile string; Unattended bool; CaptchaURL string; LastLogin, LastRelogin *time.Time}`.
  - `service.Calendar{ID, Name, Description, Color, Type string; ReadOnly, Default bool; Keys string /* ok|error */; KeysError string}` (filled in Task 9).
  - `service.Options{Dirs paths.Dirs; Config config.Config; Sealer seal.Sealer /* nil: seal.KeyFile{Config.Secrets.KeyFile} */; HTTP *http.Client; Now func() time.Time; Sleep func(ctx context.Context, d time.Duration) error}`; `service.NewLocal(o service.Options) (*service.Local, error)`.
  - Login flow: validate (empty username/password → Usage; `TOTP` parsed with `otp.Parse`; `UnattendedRelogin` with a static code → Usage "unattended relogin needs the TOTP secret as an otpauth:// URI", before any network) → `loginguard.Acquire(state, 60 s)` (with a `CaptchaToken`, wait out a `TooSoon` via `Sleep` instead of refusing; without, refuse) → `auth.Begin` → on `CaptchaError` save state `needs-captcha` + captcha, return `needs-captcha` → if the user ID differs from a stored account: abort the new session, MustAct hint `pcb logout` → TOTP: `req.TOTP` given → submit; else keep the `Pending` in memory and return `needs-totp` (`SubmitTOTP` finishes it; a second SRP never happens) → `keys.FetchSalts` with `auth.Scope` → `keys.Fetch` → `keys.DeriveKeyPass` → `Secrets.Update` (tokens, key pass, username, `Relogin` only when opted in) → revoke the replaced session of the same account (best effort) → state `ready`, captcha cleared, `LastLogin`. Any failure after `Begin` aborts the pending session and saves nothing.
  - Error mapping (`errors.go`, the only place that builds `*exitcode.Error` in the service): bad credentials / TOTP rejected → MustAct; two-password / FIDO2 → MustAct with "not supported" text; unsupported verification → Blocked; guard → Blocked with its hint; transport/5xx/429 → Blocked; store unreadable → Blocked naming the key file.
  - `Logout`: revoke with a 10 s timeout (best effort), then always remove the account (or `Wipe` an unreadable store), state `logged-out`. Not logged in → `WasLoggedIn: false`, no error.
  - `Status`: no network; reads `state.json` and unseals for the username.
  - `Close`: aborts a pending (`needs-totp`) login (`DELETE /auth/v4`) and releases the manager.
- [ ] **Step 1: Failing tests** (each against `protontest`, injected `Now`/`Sleep`):
  - `TestLoginPasswordOnlyReady` (state ready; sealed account has tokens, key pass, username, `Relogin` nil; account ID = `fake.UserID`); `TestLoginWithTOTPURI` (one code received); `TestLoginNeedsTOTPThenSubmit` (`needs-totp`, nothing sealed; `SubmitTOTP` → ready; exactly one `POST /auth/v4`); `TestSubmitTOTPWithoutPendingIsMustAct`.
  - `TestLoginCaptchaRecordsState` (`needs-captcha`, URL + token in result and `state.json`, nothing sealed, one `/auth/v4`); `TestCaptchaResumeWaitsOutGuard` (resume 10 s later with the token → `Sleep` got 50 s, ready, captcha cleared); `TestLoginTooSoonIsBlocked` (no token, 10 s later → code 20, hint `retry in 50s`, no new `/auth/v4`).
  - `TestUnattendedStoresSeed` (`Relogin{Password, TOTPURI}` sealed, `state.Unattended`); `TestUnattendedRefusesStaticCode` (code 2, zero requests to the fake).
  - `TestSecondAccountRefused` (`AddUser` B; logged in as A → MustAct hint `pcb logout`, B's session revoked, store still A); `TestReloginSameAccountReplacesSession` (61 s later: new tokens sealed, old UID revoked).
  - `TestLogoutRevokesAndWipes`; `TestLogoutWhenLoggedOut`; `TestLogoutUnreadableStoreWipesLocally` (key file removed → `Revoked: false`, `secrets.sealed` gone, state logged-out).
  - `TestCloseAbortsPendingLogin` (`needs-totp`, then `Close` → one `DELETE /auth/v4`, nothing sealed).
  - `TestStatusMakesNoRequests` (phases ready / needs-captcha / logged-out, zero fake calls).
  - `TestEveryErrorIsAnExitError` (table: wrong password, TOTP rejected, two-password, FIDO2, guard, closed server, unreadable store → each `*exitcode.Error` with the mapped code and a non-empty hint; none contains the password, TOTP seed or tokens).
- [ ] **Step 2–4:** run (FAIL) → implement → run (PASS).
- [ ] **Step 5: Commit** (`🧭 Add the service layer: login, logout, status`).

### Task 9: C3 service — sessions, refresh, unattended relogin, calendars

**Files:**
- Create: `internal/service/session.go`, `internal/service/calendars.go`
- Test: `internal/service/session_test.go`, `internal/service/calendars_test.go`

**Interfaces:**
- Consumes: Task 8 (`Local`, error mapping), `api.New` + `api.ErrSessionEnded`, `auth.Refresh`, `keys.Unlock`, `calendar.*`.
- Produces:
  - `(*Local).session(ctx) (*api.Client, store.Account, error)` (unexported): state not `ready` → MustAct (`needs-captcha` hint `pcb login resume --captcha-token <token>`, else `pcb login`); builds `api.Client` whose `Refresh` runs inside `Secrets.Update`: if the sealed refresh token differs from the stale one, adopt it (another process refreshed) with no network; else `auth.Refresh` and store the result before returning.
  - Session ended (`api.ErrSessionEnded`): with `state.Unattended` and a `Relogin` → `relogin` once: the Task 8 login flow with the sealed password and the TOTP code from the sealed URI, guard enforced (too soon → Blocked, state unchanged); CAPTCHA → state `needs-captcha`, MustAct; bad credentials / TOTP rejected / keys locked → state `needs-login` with `Reason`, MustAct; transient → Blocked, state unchanged; success → `LastRelogin`, then the original operation retried once. Without relogin → state `needs-login`, MustAct hint `pcb login`.
  - `keys.Unlock` failing with the sealed key pass (password changed elsewhere) → state `needs-login`, `Reason` "account keys no longer unlock (password changed?)", MustAct.
  - `(*Local).Calendars(ctx)`: session → `keys.Unlock` → `calendar.List` → `calendar.DefaultID` → `calendar.Unlock` per calendar (`Keys: ok`, or `error` + `KeysError`; one failure never fails the list) → `service.Calendar` with `Type` string, `ReadOnly`, `Default` (only the calendar whose ID equals the default; none when unset or unlisted). Keyrings closed before returning.
- [ ] **Step 1: Failing tests**:
  - `TestCalendarsListsWithFlags` (3 calendars: normal default, subscribed, holidays → types, `ReadOnly` false/true/true, `Default` only on the first, all `Keys: ok`); `TestCalendarsReportsBrokenKeysPerCalendar`; `TestCalendarsNoDefaultWhenUnset`; `TestCalendarsWhenLoggedOut` (MustAct, hint `pcb login`, zero fake calls).
  - `TestExpiredAccessTokenRefreshesAndPersists` (`ExpireAccessTokens` → ok; a new `Local` on the same dirs works with the sealed rotated tokens without refreshing).
  - `TestConcurrentRefreshAcrossProcessesRefreshesOnce` (two `Local`s, same dirs, expired token, concurrent `Calendars` → both ok, one `POST /auth/v4/refresh`).
  - `TestTransientRefreshFailureKeepsSession` (expired token + `FailNext` refresh 503 → code 20; `state.json` and `secrets.sealed` bytes unchanged; zero `POST /auth/v4`).
  - `TestSessionEndedWithoutReloginNeedsLogin` (`RevokeSessions` → MustAct `pcb login`, phase `needs-login`).
  - `TestUnattendedReloginRepairsSession` (`RevokeSessions`, `Now` +61 s → ok; one new `POST /auth/v4`; its TOTP code is the seed's code at `Now`; `LastRelogin` set).
  - `TestReloginStopsAfterHumanNeeded` (unattended, `RevokeSessions`, fake starts answering CAPTCHA `T9` → MustAct with the resume hint, phase `needs-captcha`; second `Calendars` 2 min later → MustAct, zero new `/auth/v4`); same with a changed password (`needs-login`, one attempt total).
  - `TestReloginThrottled` (relogin due 30 s after the last login → code 20, zero `/auth/v4`).
  - `TestChangedPasswordNeedsLogin` (corrupt the sealed key pass → MustAct with the reason, phase `needs-login`).
- [ ] **Step 2–4:** run (FAIL) → implement → run (PASS).
- [ ] **Step 5: Commit** (`🔄 Restore sessions, refresh once across processes, relogin unattended`).
- [ ] **Step 6: Live verification (deferred to Task 11)**: after the author revokes the pcb session in the web app, `pcb calendar list` relogs in once unattended (or stops at a CAPTCHA with exit 10), `pcb status` shows the relogin time.

### Task 10: CLI — `pcb login`, `login resume`, `logout`, `status`, `calendar list`

**Files:**
- Create: `internal/cli/login.go`, `internal/cli/logout.go`, `internal/cli/status.go`, `internal/cli/calendar.go`, `internal/cli/creds.go`, `internal/cli/terminal.go`
- Modify: `internal/cli/root.go` (persistent `--non-interactive`; `Run`), `cmd/pcb/main.go` (signal context), `go.mod` (`golang.org/x/term`), `README.md` (Usage), create `scripts/pcb.env.example`
- Test: `internal/cli/login_test.go`, `internal/cli/creds_test.go`, `internal/cli/calendar_test.go`, `internal/cli/arch_test.go`

**Interfaces:**
- Consumes: `service.API`, `service.NewLocal`, `config.Load`, `paths.Resolve`, `output.Write`, `exitcode`.
- Produces:
  - `cli.Terminal interface{ Interactive() bool; ReadLine(prompt string) (string, error); ReadSecret(prompt string) (string, error) }` (prompts on stderr; the OS implementation is interactive when stdin is a terminal, `golang.org/x/term`).
  - `cli.IO{Stdout, Stderr io.Writer; Env func(string) string; Term cli.Terminal; NewService func(ctx context.Context, env func(string) string) (service.API, error)}`; `cli.Run(ctx context.Context, args []string, io cli.IO) exitcode.Code`; `cli.Execute(args, stdout, stderr)` = `Run` with OS defaults and `signal.NotifyContext(SIGINT, SIGTERM)`. The default `NewService` resolves `paths`, loads `config.toml`, returns `service.NewLocal` (M5 swaps in the socket client here only).
  - Interactive = `Term.Interactive()` and not `--non-interactive`.
  - Credentials: username `--username` > `PCB_USERNAME` > prompt; password `--password-fd N` > `PCB_PASSWORD` > secret prompt; TOTP `PCB_TOTP` at start, else a prompt only when the service answers `needs-totp`. Missing and not interactive → exit 10 naming the variable (and `--password-fd`), before any service call; `needs-totp` not interactive → `service.Close` (aborts the pending session), exit 10 naming `PCB_TOTP`. No flag ever takes a secret.
  - `readPasswordFD(fd int) (string, error)`: up to 4096 bytes to EOF; strips exactly one trailing `\n` or `\r\n`; empty → Usage.
  - `pcb login [--username U] [--password-fd N] [--unattended-relogin]`; `pcb login resume --captcha-token T` (same flags; `--captcha-token` required; the token is a challenge, not an account secret). On `needs-captcha`: interactive → print URL, "press Enter once solved", retry with the token, at most 3 rounds; else exit 10, message has the URL, hint `pcb login resume --captcha-token <T>`, JSON `data` = the `LoginResult` alongside `error`.
  - `pcb logout`; `pcb status` (text: one `key: value` line per field; exit 0, 20 only if local files are unreadable); `pcb calendar list` (text columns `NAME  TYPE  ACCESS  DEFAULT  KEYS  ID`, ACCESS `rw`/`ro`, DEFAULT `*`; JSON `data.calendars` = `[]service.Calendar`).
  - `scripts/pcb.env.example`: `PCB_USERNAME=pass://<vault>/<item>/<username field>`, `PCB_PASSWORD=…/<password field>`, `PCB_TOTP=…/<totp field>?totp=uri` [verified: M0 ruling R9].
- [ ] **Step 1: Failing tests**:
  - `TestNoFlagTakesASecret` (`pcb login --password x` and `--totp 1` → exit 2).
  - `TestPasswordFDStripsOneNewline` (pipe `"pa ss\n"` → `pa ss`; `"pass\r\n"` → `pass`; `"pass\n\n"` → `pass\n`; `""` → exit 2 and zero fake calls).
  - `TestNonInteractiveNamesMissing` (no `PCB_PASSWORD` → exit 10, message names `PCB_PASSWORD` and `--password-fd`, zero fake calls).
  - `TestNeedsTOTPNonInteractive` (TOTP account, no `PCB_TOTP` → exit 10 naming `PCB_TOTP`; one `DELETE /auth/v4`).
  - `TestInteractiveTOTPPrompt` (fake terminal answers the code → ready, one `/auth/v4`).
  - `TestCaptchaNonInteractiveExit10` (JSON: `ok=false`, `error.code=10`, `error.hint` = `pcb login resume --captcha-token T1`, `data.captcha_url`, `data.captcha_token`).
  - `TestCaptchaInteractiveRetries` (fake terminal Enter; second attempt carries `T1`; ready).
  - `TestEndToEndCalendarList` (TOTP fake, 3 calendars, `config.toml` `base_url` = fake URL, env credentials: `login --non-interactive --json` ok → `calendar list --json` lists 3 with types/read-only/default → `status --json` phase ready → `logout` → `calendar list` exit 10 hint `pcb login`).
  - `TestCLIImportsNeitherProtonNorStore` (`go/build.ImportDir` of `internal/cli`, non-test `Imports` only — tests may use `protontest`: no import path under `<module>/internal/proton` or `<module>/internal/store`; `internal/service` is the boundary).
- [ ] **Step 2–4:** run (FAIL) → implement → run `make lint test` (PASS).
- [ ] **Step 5: Commit** (`✨ Add pcb login, logout, status and calendar list`).
- [ ] **Step 6: Live verification (deferred to Task 11)**: the M1 done criterion, `pcb calendar list` against the test account.

### Task 11: M1 live session (deferred) and wrap-up

**Files:**
- Modify: `spec/HUMAN-CHECKS.md` (add "M1 live session"), `README.md` (Status, Credits), `spec/SPEC.md` (markers settled live), `spec/experiments/E02-captcha-token.md` (data point)
- Create (live part): `spec/experiments/M1-live.md`, `internal/proton/calendar/testdata/*.json` + `TestDecodersAcceptRecordedFixtures`

- [ ] **Step 1: Offline wrap-up now**: README Credits — proton-cal docs (key chain, locked scope and 9101, member-level calendar metadata, v2 bootstrap, default calendar setting) and the fork (two-step CAPTCHA, env/TOTP login, refresh-token race lesson), the go-proton-api server test pattern; README Status = M1. Write the live session below into `spec/HUMAN-CHECKS.md` as one ordered checklist. `make lint test` green in CI; commit (`📝 Close M1 offline, list the live session`).
- [ ] **Step 2: Live session** (when the author can solve a CAPTCHA; at most 3 logins, ≥ 60 s apart; isolated dirs `XDG_CONFIG_HOME=~/.local/share/pcb-m1-live/config XDG_STATE_HOME=~/.local/share/pcb-m1-live/state`):
  1. `make build`; `pass-cli run --env-file ~/.config/pcb-live/pcb.env -- bin/pcb login --non-interactive --unattended-relogin --json` → ready, or exit 10 with the URL (author solves it).
  2. ≥ 60 s later, new process: `pass-cli run … -- bin/pcb login resume --captcha-token <T> --non-interactive --unattended-relogin` → ready (E02: delay and cross-process result).
  3. `bin/pcb status`; `bin/pcb calendar list` → every calendar of the test account, types, read-only flags, default, `keys ok` (**M1 done**). Author adds one holidays and one subscribed calendar in the web app, rerun.
  4. `pcb-lab probe calendar-access` (M0 Task 6) records redacted `/calendar/v1`, `/settings/calendar`, bootstrap; copy the bodies to `internal/proton/calendar/testdata/`, add `TestDecodersAcceptRecordedFixtures` (each fixture decodes; ≥ 1 calendar; names non-empty), run, commit.
  5. Author revokes the pcb session (account.proton.me → Security → Sessions) → `bin/pcb calendar list` → one unattended relogin, or exit 10 with a CAPTCHA; `bin/pcb status` shows it.
  6. `bin/pcb logout` → revoked; `bin/pcb status` → logged-out; remove the isolated dirs.
- [ ] **Step 3: Write up** `spec/experiments/M1-live.md` (what each step showed, redacted), the E02 data point, SPEC markers (§5.2 locked scope on fresh sessions, §5.4 types and keys per type); commit (`🧪 M1 live session against the test account`).

## Controller rulings carried into M1 (from the M0 execution, 2026-11-11)

- R16: `pcb login resume --totp CODE` moves to M5 (needs the daemon to hold a half-authenticated SRP session); M1 takes TOTP from `PCB_TOTP` or a prompt in the same process. Update spec §13 accordingly in Task 11.
- R17: member-passphrase signature verification is out of M1 but must be settled (E10) before M4 encrypts to a calendar key.
- R23 (from the M0 final review): (a) credential and verification failures (wrong password, 2FA rejected, CAPTCHA not accepted) exit 10 ("you must act"), not 20 — classify them at the call sites in Tasks 8 and 10, never by matching message text; (b) the token store locks refresh and rotation across processes (Task 9's concurrent-refresh test pins it); (c) `internal/lab/record` writes through `atomicfile.Write` once Task 1 creates it.
- From the M0 final review: cobra's auto-added `completion` command is not covered by the usage contract — `cli.Root` disables the default completion command (or gives it `cli.NoArgs`), with a case added to `clitest.CheckUsageContract` (Task 10).
- M0 already provides the shared CLI runner (`internal/cli`: `Run`, `Root`, `Group`, `UsageError`, `NoArgs`, `CommandName`, `JSONMode`, `Finish`), `exitcode.HintOf`, `internal/shellquote` and `internal/cli/clitest.CheckUsageContract`; every new command uses them and is added to the usage-contract table.
