# M6 Multi-account Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One process (`pcb serve` today, `pcb-ui` in M7) serves N Proton accounts through one CalDAV listener and one control socket; every CLI command that concerns one account can name it; an M5 install upgrades with no action (state migrated, Thunderbird unchanged).

**Architecture:**
- **C2 `internal/store`:** a v2 layout (`<state>/accounts/<local-id>/`, `layout.json`) with a crash-safe migration from M5's flat layout and downgrade detection; `State` gains the address, the CalDAV switch and a creation time.
- **C3 `internal/service`:** `Local` stays single-account on its account directory; the login guard directory and the CalDAV failure limiter are injected. A new `Hub` owns every `Local`: open (migrate, lock), select, add by login, sign out, remove, switch CalDAV, restart, route CalDAV passwords, and `Serve` (one supervised loop per account, the exit rule).
- **A1 `internal/caldav`:** per-account URLs `/dav/<local-id>/...` with realm `pcb <local-id>`, compatibility URLs routed by password, through a `service.Router`.
- **CTL `internal/ctl`:** `/v2/`, account-aware routes and status, `Daemon.Kind`, alert `account-stopped`.
- **C6 `internal/daemon` (new):** the wiring `internal/cli/serve.go` does today (TLS, listener, CTL, drain), reused by `pcb-ui` in M7.
- **`internal/cli`:** `--account`/`PCB_ACCOUNT`, `pcb account list|caldav|restart|remove`, every command on the `Hub`, `--json` schema 2.

**Tech Stack:** as M5. Go 1.27, stdlib `net/http` `ServeMux` patterns for CTL v2. No new dependency.

**Spec:** `spec/designs/2026-11-12-multi-account-and-ui-design.md` §1 to §4 and §7 (M6 part); `spec/SPEC.md` §4, §5.2, §7.1, §7.3, §9, §13, §15; decisions 0001, 0013; `AGENTS.md`.

**Out of scope (YAGNI, say so in review):** `pcb-ui`, Fyne, Secret Service (M7); several CalDAV listeners; per-calendar selection; any change to how pcb talks to Proton; Windows file locks and control socket; a live second account (author, 2026-11-12: no second test account, multi-account proven on the fake only).

## Global Constraints

- Everything in the M1 to M5 Global Constraints still holds (`spec/plans/2026-11-12-m5-daemon-ops.md`): the `--json` envelope; exit codes `0`, `2`, `10`, `20`, `30`, `40`; `--non-interactive` never prompts; secrets never in argv, logs, errors, responses or unsealed files; `internal/cli` and the adapters call only `internal/service` (`arch_test.go`); every `service` error is an `*exitcode.Error` with a secret-free message and a hint; tests bite, the reviewer checks by sabotage (no mutation testing); live checks only on the test account, as deferred human-check steps; `make lint test` is the gate; `GOOS=windows go vet ./...` stays clean.
- Import rules: `internal/caldav` imports only stdlib, `internal/service`, `internal/exitcode`; `internal/ctl` only stdlib, `service`, `output`, `exitcode`, `buildinfo`; `internal/daemon` follows the `cli` rule (never `internal/proton/...`, `internal/store/...`, go-proton-api). Add `internal/daemon/arch_test.go` with that rule.
- `pcb` stays `CGO_ENABLED=0`.
- Layout v2 (design §4.2), verbatim:
  - root: `serve.lock`, `pcb.sock`, `tls/`, `logs/`, `login.lock`, `last-login`, `layout.json` `{"version": 2}`, `accounts/`;
  - `accounts/<local-id>/`: `state.json`, `state.lock`, `secrets.sealed`, `secrets.lock`, `clients.json`, `clients.lock`, `pcb.db` (+ `-wal`, `-shm`), `sync.lock`;
  - `<local-id>`: 8 random lowercase hex characters, never reused or renamed.
- Account selector: `--account A` or `PCB_ACCOUNT`; `A` is the address (case-insensitive) or the local ID; optional when exactly one non-pending account exists; with several and none given: exit 2, message `several accounts: name one with --account`, `--json` `data.accounts` listing them.
- CalDAV: per-account base `/dav/<local-id>/`, principal `/dav/<local-id>/principals/me/`, home `/dav/<local-id>/calendars/me/`, realm `pcb <local-id>`; compatibility paths `/`, `/.well-known/caldav`, `/dav/principals/me/`, `/dav/calendars/me/...`, realm `pcb`. The printed URL is `<scheme>://<host>:<port>/dav/<local-id>/`; the username shown is the account's address.
- Pending account (CAPTCHA during an add): expires at the first Hub open more than 24 h after `State.Created`.
- `output.SchemaVersion` = 2. CTL prefix `/v2/`.
- Alert `account-stopped` (severity `error`), message the stop error's message, fix its hint.

## Review Focus

1. **Upgrade with Thunderbird running**: an M5 state migrated while Thunderbird keeps polling the compatibility URLs must keep syncing, same ETags and sync tokens (Task 9 `TestMigratedThunderbirdKeepsItsTokens`).
2. **A password from account B on account A's URLs**: 401, never B's data and never A's (Task 5 `TestOtherAccountsPasswordIsRefusedOnPrefixedURL`).
3. **Interrupted migration**: a crash between any two moves resumes, never duplicates or loses `secrets.sealed` (Task 1 `TestMigrationResumesAfterEachStep`).
4. **Logging in the same address twice in a row while the first add waits for its TOTP**: the second goes to the pending account, no second directory (Task 3 `TestLoginSameAddressReusesPendingAccount`).
5. **Several accounts relogin after an outage**: the shared login guard spaces them, never two Proton logins inside `loginguard.MinInterval` (Task 4 `TestReloginsOfTwoAccountsAreSpaced`).

---

### Task 0: Fake Proton failure injection per user

**Files:**
- Modify: `internal/proton/protontest/fake.go` (`FailNext`, `FailNextRetryAt`, `StallNext`, `SetCaptcha`, `Options.TwoFA` handling at `twoFA`)
- Test: `internal/proton/protontest/fake_test.go`

**Interfaces:**
- Produces: `func (f *Fake) FailNextFor(username, method, path string, status, code, retryAfter int)`, `func (f *Fake) StallNextFor(username, method, path string, bound time.Duration)`, `func (f *Fake) SetCaptchaFor(username, token string)`, `func (f *Fake) SetTwoFA(username string, on bool) (totpURI string)`. The existing fake-wide methods keep their behaviour (they apply to every user).
- The user of a request is the session's user (`sessionUser`) or, on `POST /auth/v4` and `/auth/v4/info`, the request's `Username`.

- [ ] **Step 1: Write the failing tests**: `TestFailNextForHitsOnlyThatUser` (two users; `FailNextFor("b", GET, /calendar/v1, 500, 0, 0)`; a call as `a` gets 200, the next as `b` gets 500, the one after 200); `TestCaptchaForChallengesOnlyThatUser` (`SetCaptchaFor("b","tok")`: `/auth/v4` for `a` succeeds, for `b` answers 422/9001 with token `tok`); `TestTwoFAPerUser` (`SetTwoFA("b", true)`: `a`'s auth has `2FA.Enabled = 0`, `b`'s `1`, and `b`'s codes come from the returned URI).
- [ ] **Step 2: Run** `go test ./internal/proton/protontest -run 'For|PerUser'` → FAIL (undefined).
- [ ] **Step 3: Implement**: failures, stalls, captcha tokens and 2FA keyed by username (`""` = every user), looked up user first, then `""`.
- [ ] **Step 4: Run** the package tests → PASS, existing tests unchanged.
- [ ] **Step 5: Commit** `🧪 Inject fake Proton failures per user`

### Task 1: Store layout v2 and migration

**Files:**
- Create: `internal/store/layout.go`, `internal/store/layout_test.go`
- Modify: `internal/store/state.go` (`State` fields)

**Interfaces:**
- Produces:
  - `const LayoutVersion = 2`
  - `func AccountsDir(stateDir string) string` → `<state>/accounts`
  - `func AccountDir(stateDir, localID string) string`
  - `func NewLocalID(exists func(id string) bool) (string, error)`: 8 lowercase hex, retried while `exists`.
  - `func ValidLocalID(id string) bool`
  - `type LayoutInfo struct { Version int; Accounts []string; Legacy []string; Migrating string }`: `Legacy` lists M5 account files found at the root, `Migrating` a `<id>.migrating` directory.
  - `func ReadLayout(stateDir string) (LayoutInfo, error)`: no write.
  - `func Migrate(stateDir string, newID func() (string, error)) (localID string, migrated bool, err error)`: the caller holds the locks (Task 3).
  - `var ErrLegacyBesideLayout = errors.New(...)`: `Migrate` and `ReadLayout`'s caller get it when `Version == 2` and `Legacy` is not empty.
  - `State` gains `Address string \`json:"address,omitempty"\``, `CalDAV *bool \`json:"caldav,omitempty"\`` (nil = on), `Created time.Time \`json:"created,omitzero"\``. The comment "never holds a username" becomes "the address is not a secret; never a token or password".
- Account files moved by `Migrate` (exact list): `state.json`, `state.lock`, `secrets.sealed`, `secrets.lock`, `clients.json`, `clients.lock`, `pcb.db`, `pcb.db-wal`, `pcb.db-shm`, `sync.lock`. Root files that stay: `login.lock`, `last-login`, `pcb.sock`, `tls/`, `logs/`.

- [ ] **Step 1: Write the failing tests**:
  - `TestFreshStateHasNoLayout`: `ReadLayout(empty)` → `Version 0`, no accounts; `Migrate` → `migrated false`, writes `layout.json` version 2 and an empty `accounts/` (0700).
  - `TestMigrateMovesM5Files`: an M5 state written with the dir-based store functions at the root (`SaveState`, `NewSecrets(root).Update`, `access.NewStore(root)` via a test helper in `layout_test.go` that only writes files with the real formats, plus a `pcb.db` file) → after `Migrate`: each listed file at `accounts/<id>/` with identical bytes, none left at the root, `login.lock` and `tls/` still at the root, `layout.json` version 2.
  - `TestMigrationResumesAfterEachStep`: for each crash point k (after k file moves, after the directory rename, before `layout.json`), simulate by injecting a failing `rename` hook (`var renameHook func(from, to string) error`, tests only) then run `Migrate` again → final tree equals `TestMigrateMovesM5Files`'s, same `<id>` reused (from the `.migrating` name), no file lost or duplicated.
  - `TestLegacyBesideLayoutIsRefused`: layout v2 plus a root `state.json` → `errors.Is(err, ErrLegacyBesideLayout)`, nothing moved.
  - `TestLocalIDShape`: 1000 IDs match `^[0-9a-f]{8}$`; `NewLocalID` with an `exists` that refuses the first two answers a third distinct one.
  - `TestStateKeepsAddressAndSwitch`: `SaveState` then `LoadState` round-trips `Address`, `CalDAV=false`, `Created`; an M5 `state.json` without them loads `CalDAV == nil`.
- [ ] **Step 2: Run** `go test ./internal/store -run 'Layout|Migrat|LocalID|Address'` → FAIL.
- [ ] **Step 3: Implement** `layout.go`: moves by `os.Rename` into `accounts/<id>.migrating/`, rename to `accounts/<id>`, write `layout.json` with `atomicfile`, fsync the directories.
- [ ] **Step 4: Run** `go test ./internal/store/...` → PASS.
- [ ] **Step 5: Commit** `🗂️ Add the per-account state layout and its migration`

### Task 2: `Local` on an account directory

**Files:**
- Modify: `internal/service/local.go` (`Options`, `NewLocal`), `internal/service/login.go:212-232` (`acquire`), `internal/service/clients.go:123-149` (`CheckClient`), `internal/service/login.go` (`complete`, `needsCaptcha`: write `Address`, `Created`), `internal/service/api.go` (`Status`)
- Test: `internal/service/login_test.go`, `internal/service/clients_test.go`

**Interfaces:**
- Produces:
  - `Options` gains `GuardDir string` (login guard directory; empty = `Dirs.State`) and `Limiter *access.Limiter` (nil = a new one).
  - `func (l *Local) VerifyClient(ctx context.Context, password string) (found bool, err error)`: no limiter; touches and notes activity on a match; `err` set when `clients.json` cannot be loaded.
  - `CheckClient` keeps its contract, now `Limiter.Allow` then `VerifyClient` then `Limiter.Fail`.
  - `Status` gains `Address string \`json:"address,omitempty"\`` read from `state.json` (shown logged out).
  - Every `state.json` write after a login keeps `Address`, `CalDAV`, `Created` (they are not login fields).

- [ ] **Step 1: Write the failing tests**: `TestGuardDirIsShared` (two `Local`s on two account dirs with the same `GuardDir`: the second login within `loginguard.MinInterval` gets `TooSoon`, exit 20); `TestSharedLimiterCountsAcrossLocals` (a limiter shared by two `Local`s: five wrong passwords on one, the sixth `CheckClient` on the other is refused with a wait); `TestVerifyClientDoesNotChargeTheLimiter`; `TestAddressSurvivesLogout` (login, logout → `Status.Address` is the username, `Phase` logged-out); `TestLoginKeepsTheCalDAVSwitch` (state with `CalDAV=false`, login → still false).
- [ ] **Step 2: Run** `go test ./internal/service -run 'GuardDir|SharedLimiter|VerifyClient|AddressSurvives|KeepsTheCalDAV'` → FAIL.
- [ ] **Step 3: Implement**; `logout` resets the login fields only.
- [ ] **Step 4: Run** `go test ./internal/service` → PASS.
- [ ] **Step 5: Commit** `♻️ Run Local on an account directory with a shared guard and limiter`

### Task 3: `Hub`: open, select, add, sign out, remove, switch

**Files:**
- Create: `internal/service/hub.go`, `internal/service/hub_test.go`
- Modify: `internal/service/api.go` (types below)

**Interfaces:**
- Consumes: Task 1 layout functions; Task 2 `Options.GuardDir`, `Options.Limiter`, `VerifyClient`.
- Produces:
  - `type AccountInfo struct { ID, Address string; Phase string; Pending bool; CalDAV bool; Stopped *StopInfo }` with JSON `id, address, phase, pending, caldav, stopped`.
  - `type StopInfo struct { Code int; Message, Hint string; At time.Time }`.
  - `type HubOptions struct { Options }` (the shared `Options`; `Dirs.State` is the root).
  - `func OpenHub(o HubOptions) (*Hub, error)`: migrates (Task 1) under a `TryLock` of the root `sync.lock` and of `<state>/layout.lock`; root `sync.lock` held by another process → exit 20 `another pcb holds the state` hint `stop the running pcb first`; `ErrLegacyBesideLayout` → exit 20 naming the files, hint `remove them or move the state directory aside: no downgrade after M6`; expires pending accounts older than 24 h; opens one `Local` per account dir with `GuardDir` = root and one shared `Limiter`.
  - `func (h *Hub) Accounts() []AccountInfo` (sorted by address).
  - `func (h *Hub) Account(selector string) (API, AccountInfo, error)`: the selector rule of the Global Constraints; unknown → exit 2 `no account <selector>`.
  - `func (h *Hub) Login(ctx context.Context, req LoginRequest) (LoginResult, error)`: by `req.Username` (case-insensitive) an existing or pending account, else a new directory; cleanup rules of design §4.3; `LoginResult` gains `Account string \`json:"account"\`` (the local ID).
  - `func (h *Hub) SignOut(ctx context.Context, selector string, req LogoutRequest) (LogoutResult, error)`.
  - `func (h *Hub) Remove(ctx context.Context, selector string, req LogoutRequest) (AccountInfo, error)`: sign out, then delete the directory.
  - `func (h *Hub) SetCalDAV(ctx context.Context, selector string, on bool) (AccountInfo, error)`.
  - `func (h *Hub) Close() error`.
  - `func (h *Hub) OnChange(f func())`: called after any account is added, removed or switched (Task 4, M7).

- [ ] **Step 1: Write the failing tests** (fake Proton with users `a@x`, `b@x`):
  - `TestOpenHubMigratesM5State`: M5 state at the root (as Task 1's helper, plus a real login done by a `Local` on the root before migration) → one account, its `Status.Phase` ready, its CalDAV password still verifies.
  - `TestOpenHubRefusesWhileM5SyncLockHeld`: root `sync.lock` held by `filelock.Lock` in the test → exit 20.
  - `TestLoginAddsSecondAccount`: login `a`, login `b` → two accounts, distinct IDs, each `Status.Address` its own.
  - `TestLoginExistingAddressRelogsThatAccount`: login `a`, sign out `a`, login `A@X` → same ID, no new directory.
  - `TestFailedAddLeavesNothing`: wrong password for `c@x` → exit 10, `accounts/` holds only `a`'s directory.
  - `TestCaptchaAddIsPending`: `SetCaptchaFor("b@x")` → login `b` returns needs-captcha, `Accounts()` lists `b` `Pending: true`; `Account("")` with `a` and pending `b` selects `a`.
  - `TestLoginSameAddressReusesPendingAccount`: TOTP pending for `b` (`SetTwoFA`), a second `Login` for `b` → same ID, one directory.
  - `TestPendingExpiresAfter24h`: pending `b` created at t; `OpenHub` at t+24h+1s → gone; at t+23h → kept.
  - `TestSameUserOtherAddressIsRefused`: login `a`, login with `a`'s second address → exit 10 naming `a@x`, no new directory.
  - `TestSelectorRules`: zero accounts → exit 2 `no account`; one → `""` selects it; two → `""` exit 2 listing both; by ID and by address in another case → found.
  - `TestRemoveDeletesOnlyThatAccount`: remove `a` → its directory gone, `b` untouched and ready.
  - `TestSetCalDAVPersists`: off → reopen Hub → `CalDAV false`.
- [ ] **Step 2: Run** `go test ./internal/service -run 'Hub|Login(Adds|Existing|SameAddress)|FailedAdd|CaptchaAdd|Pending|SameUser|Selector|RemoveDeletes|SetCalDAV'` → FAIL.
- [ ] **Step 3: Implement** `hub.go`; a login in flight holds one `Hub` mutex (as `ctl`'s `oneLogin`).
- [ ] **Step 4: Run** `go test ./internal/service` → PASS.
- [ ] **Step 5: Commit** `✨ Hold several Proton accounts in one Hub`

### Task 4: `Hub.Serve`: one supervised loop per account

**Files:**
- Modify: `internal/service/hub.go`, `internal/service/health.go`
- Test: `internal/service/hub_serve_test.go`

**Interfaces:**
- Consumes: `Local.Serve(ctx, ServeOptions)`.
- Produces:
  - `type HubServeOptions struct { ServeOptions; OnState func(accountID string, s ServeState); OnPass func(accountID string, p WatchPass) }` (the per-account callbacks replace `ServeOptions.OnState`/`OnPass`).
  - `func (h *Hub) Serve(ctx context.Context, o HubServeOptions) error`: holds `serve.lock` (held → exit 20 `pcb is already serving (pid N)`, the PID written in `serve.lock`) and the root `sync.lock` for its life; starts a loop per non-pending account with CalDAV on; starts/stops loops on add, remove, switch; a loop that returns an error not caused by ctx parks the account (`AccountInfo.Stopped`); exit rule of design §4.1.
  - `func (h *Hub) Restart(ctx context.Context, selector string) (AccountInfo, error)`: clears `Stopped`, starts the loop.
  - `func (h *Hub) Health(ctx context.Context, selector string) (Health, error)`.
  - `ServeState` gains `StateStopped ServeState = "stopped"`.

- [ ] **Step 1: Write the failing tests**:
  - `TestServeRunsEveryAccount`: two accounts, each gets a pass (`OnPass` called with both IDs).
  - `TestOneAccountsCaptchaDoesNotStallTheOther`: `SetCaptchaFor` and an expired session for `a` → `a` needs-captcha, `b` keeps passing.
  - `TestDamagedCacheParksOnlyThatAccount`: corrupt `a`'s `pcb.db` → `a` `Stopped` with exit code 20, `b` passing; `Restart(a)` after replacing the file → passing.
  - `TestServeExitsWhenEveryAccountStopped`: without `WaitForLogin`, both parked → `Serve` returns the first stop's error; with `WaitForLogin` → keeps running until ctx ends.
  - `TestServeRefusesWithNoLoggedInAccount`: no `WaitForLogin`, zero ready accounts → exit 10 at once.
  - `TestAddedAccountStartsServing`, `TestRemovedAccountStopsFirst` (its loop ends before the directory goes), `TestCalDAVOffPausesTheLoop` (no pass for that account while off; one at once when back on).
  - `TestSecondServeIsRefused`: a second Hub `Serve` on the same root → exit 20 naming the PID.
  - `TestReloginsOfTwoAccountsAreSpaced`: both sessions revoked, unattended relogin on for both → the fake's `/auth/v4` calls are at least `loginguard.MinInterval` apart (injected clock and `Sleep`).
- [ ] **Step 2: Run** `go test ./internal/service -run 'Serve(Runs|Exits|Refuses)|OneAccounts|DamagedCache|Added|Removed|CalDAVOff|SecondServe|Relogins'` → FAIL.
- [ ] **Step 3: Implement**; the supervisor goroutines end before `Serve` returns.
- [ ] **Step 4: Run** `go test ./internal/service` → PASS (check the package stays under 300 s: `go test -count=1 ./internal/service` time, CI's 600 s timeout).
- [ ] **Step 5: Commit** `✨ Serve every account of the Hub, each in its own loop`

### Task 5: CalDAV per-account URLs and routing

**Files:**
- Modify: `internal/caldav/server.go` (paths, `resolve`, `Handler`), `internal/caldav/auth.go`, `internal/caldav/propfind.go` (principal and home hrefs)
- Modify: `internal/service/hub.go` (implements `Router`)
- Test: `internal/caldav/routing_test.go`, existing contract tests

**Interfaces:**
- Produces in `service`: `type Router interface { Route(ctx context.Context, accountID, password, remote string) (api API, retryAfter time.Duration, err error) }`: `accountID` empty on compatibility URLs; `api == nil, retryAfter == 0` → 401; `retryAfter > 0` → 503 with `Retry-After`; rules of design §4.4 (limiter once, CalDAV-off accounts skipped, broken stores). `*Hub` implements it.
- `caldav.Handler(r service.Router, o Options) http.Handler` (was `service.API`).
- `func AccountPath(localID string) string` → `/dav/<local-id>/` (used by `daemon` and `cli` for the printed URL).
- Hrefs answer in the namespace the request came on: a prefixed request gets prefixed hrefs, a compatibility request compatibility hrefs; `/` answers `current-user-principal` with the prefixed principal.

- [ ] **Step 1: Write the failing tests** (two accounts on the fake, through `httptest`):
  - `TestPrefixedURLServesOnlyThatAccount`: PROPFIND Depth 1 on `/dav/<a>/calendars/me/` with `a`'s password lists `a`'s calendars only.
  - `TestOtherAccountsPasswordIsRefusedOnPrefixedURL`: `b`'s password on `/dav/<a>/...` → 401 with `WWW-Authenticate: Basic realm="pcb <a>", charset="UTF-8"`, body empty.
  - `TestCompatibilityURLRoutesByPassword`: `/dav/calendars/me/` with `a`'s then `b`'s password → each its own calendars; realm `pcb` on the 401 without credentials.
  - `TestRootPointsToPrefixedPrincipal`: PROPFIND `/` with `b`'s password → `current-user-principal` `/dav/<b>/principals/me/`.
  - `TestCalDAVOffAccountIs401`: `a` off → its password 401 on both URL kinds; `b` unaffected.
  - `TestBrokenClientsFile`: `a`'s `clients.json` unreadable → 503 on `/dav/<a>/`; on compatibility URLs `b`'s password 200, an unknown password 503.
  - `TestParkedAccountIs503`: `a` stopped (Task 4) → its password gets 503 `Retry-After: 30` on its URL, `b` 207.
  - `TestLimiterIsSharedAcrossAccounts`: five wrong passwords on `/dav/<a>/`, the sixth on `/dav/<b>/` from the same IP → 503 `Retry-After`.
  - Existing contract tests run unchanged on compatibility URLs with one account and again on the prefixed URL (table over both bases).
- [ ] **Step 2: Run** `go test ./internal/caldav` → FAIL.
- [ ] **Step 3: Implement**: `authenticate` resolves the account prefix first, routes, and serves the request on a shallow copy of `server` holding that request's `API`.
- [ ] **Step 4: Run** `go test ./internal/caldav ./internal/service` and the fuzz seeds → PASS.
- [ ] **Step 5: Commit** `✨ Serve each account under its own CalDAV URL`

### Task 6: CTL v2

**Files:**
- Modify: `internal/ctl/server.go`, `internal/ctl/wire.go`, `internal/ctl/alerts.go`, `internal/ctl/client.go`
- Test: `internal/ctl/server_test.go`, `internal/ctl/alerts_test.go`

**Interfaces:**
- Consumes: Task 3 and 4 `Hub` methods, through an interface in `ctl` (`type Hub interface { Accounts() []service.AccountInfo; Account(string) (service.API, service.AccountInfo, error); Login(...); SignOut(...); Remove(...); SetCalDAV(...); Restart(...); Health(...) }`) so tests stub it.
- Produces:
  - Routes: `GET /v2/accounts`, `GET /v2/status`, `POST /v2/login`, `POST /v2/accounts/{id}/login/totp`, `POST /v2/accounts/{id}/logout`, `POST /v2/accounts/{id}/remove`, `POST /v2/accounts/{id}/caldav` (`{"on": bool}`), `POST /v2/accounts/{id}/restart`. `/v1/*` → 404.
  - `Server.Hub Hub` replaces `Server.API`.
  - `type Status struct { Daemon *Daemon; Listener *Listener; Accounts []AccountStatus; Alerts []Alert }`; `type AccountStatus struct { service.AccountInfo; Login service.Status \`json:"login"\`; Health *service.Health \`json:"health,omitempty"\`; URL string \`json:"url,omitempty"\`; Alerts []Alert \`json:"alerts"\` }`.
  - `Daemon` gains `Kind string \`json:"kind"\`` (`"serve"` or `"ui"`); `NewDaemon(kind, socket, logLevel string, startedAt time.Time)`.
  - `AlertInput` per account; `Alert` gains `Account string \`json:"account,omitempty"\``; new code `account-stopped` from `AccountInfo.Stopped`; process alerts (TLS expiry) stay at the top level.
  - Client methods mirror the routes: `Accounts`, `Status`, `Login`, `SubmitTOTP(ctx, accountID, code)`, `Logout(ctx, accountID, req)`, `Remove`, `SetCalDAV`, `Restart`.

- [ ] **Step 1: Write the failing tests**: `TestV1RoutesAreGone` (404); `TestStatusListsEveryAccount` (stub hub with two accounts → two `AccountStatus`, each with its own alerts and URL `…/dav/<id>/`); `TestAccountStoppedAlert` (stopped account → alert `account-stopped`, severity error, message and fix from `StopInfo`, `account` set); `TestUnknownAccountRoute` (`/v2/accounts/zzzzzzzz/logout` → 422 exit 2 `no account`); `TestTOTPGoesToItsAccount`; `TestDaemonKind`; the TOTP body stays redacted (existing tests ported).
- [ ] **Step 2: Run** `go test ./internal/ctl` → FAIL.
- [ ] **Step 3: Implement** with `http.ServeMux` method patterns; keep the 64 KiB body cap, peer UID check, timeouts, one login at a time.
- [ ] **Step 4: Run** `go test ./internal/ctl` → PASS, `ctl/arch_test.go` green.
- [ ] **Step 5: Commit** `✨ Make the control socket account-aware (v2)`

### Task 7: `internal/daemon`

**Files:**
- Create: `internal/daemon/daemon.go`, `internal/daemon/daemon_test.go`, `internal/daemon/arch_test.go`
- Modify: `internal/cli/serve.go` (moves out: `listenerTLS`, `servesTLS`, `bindIP`, `certNotAfter`, `stopListener`, `startControl`, `thunderbirdURL`, `urlHost`, `listenNetwork`, `serverErrorLog`; keeps flags, intro printing, signals, `waitNotice`)

**Interfaces:**
- Consumes: `Hub.Serve`, `caldav.Handler`, `caldav.AccountPath`, `ctl.Server`.
- Produces:
  - `type Options struct { Dirs paths.Dirs; Config config.Config; Hub *service.Hub; Kind string; LogLevel string; Logger *slog.Logger; Warn io.Writer; Listen func(network, addr string) (net.Listener, error); WaitForLogin bool; Cut <-chan struct{}; OnState func(accountID string, s service.ServeState); OnPass func(accountID string, p service.WatchPass); Ready func(Info) }`
  - `type Info struct { Addr string; Base string; TLS bool; Fingerprint string; CertNotAfter *time.Time }` (`Base` = `<scheme>://<host>:<port>`).
  - `func (i Info) AccountURL(localID string) string` = `Base + caldav.AccountPath(localID)`.
  - `func Run(ctx context.Context, o Options) error`: binds CalDAV and CTL, calls `Ready`, runs `Hub.Serve`, on ctx end drains 5 s (unless `Cut`), returns `Hub.Serve`'s error or nil.
  - `func OfflineInfo(ctx context.Context, dirs paths.Dirs, cc config.CalDAV) Info` (was `offlineListener`).

- [ ] **Step 1: Write the failing tests**: `TestRunServesCalDAVAndControl` (fake Proton, one account: a CalDAV PROPFIND on `Info.AccountURL(id)` answers 207 and `ctl.Client.Status` answers with `daemon.kind` = `o.Kind`); `TestRunDrainsOnCancel` (an in-flight request finishes, `Run` returns within 5.5 s); `TestRunReturnsServeError` (no account, no `WaitForLogin` → exit 10); `TestDaemonArch` (import rule).
- [ ] **Step 2: Run** `go test ./internal/daemon` → FAIL.
- [ ] **Step 3: Implement** by moving the code; `internal/cli/serve.go`'s existing tests keep passing through `daemon.Run`.
- [ ] **Step 4: Run** `go test ./internal/daemon ./internal/cli` → PASS.
- [ ] **Step 5: Commit** `♻️ Extract the daemon wiring into internal/daemon`

### Task 8: CLI on the Hub

**Files:**
- Create: `internal/cli/account.go`, `internal/cli/account_test.go`, `internal/cli/selector.go`
- Modify: `internal/cli/io.go` (`IO.NewService` → `IO.OpenHub func(ctx, env) (*service.Hub, error)`), `login.go`, `logout.go`, `status.go`, `calendar.go`, `event.go`, `sync.go`, `client.go`, `doctor.go`, `install.go:333-357`, `serve.go`, `root.go`, `rig_test.go`; `internal/output/envelope.go` (`SchemaVersion = 2`)

**Interfaces:**
- Consumes: Tasks 3, 4, 6, 7.
- Produces:
  - `IO.OpenHub` returns a `cli` interface `hub` (the `Hub` methods the CLI calls), satisfied by `*service.Hub`; `rig_test.go`'s `stubAPI` tests get a `stubHub` holding one or two `stubAPI`s, so the existing CLI tests keep their stubs.
  - Persistent flag `--account` on the root; `func (a *app) selector(cmd *cobra.Command) string` (flag, else `PCB_ACCOUNT`).
  - `pcb logout` signs out the selected account; `pcb login resume` selects among pending logins (the only one needs no selector); `pcb login` takes no selector (the username decides), `--account` there → exit 2.
  - Commands: `pcb account list`; `pcb account caldav on|off`; `pcb account restart`; `pcb account remove [--yes]` (through CTL when a daemon answers, else offline; `--non-interactive` without `--yes` → exit 10).
  - `pcb status`: every account (or `--account`'s), `--json` `data` = `ctl.Status` shape (offline: `daemon: null`).
  - `pcb client show`: `url` = the per-account URL, `username` = the address.
  - `pcb doctor`: process checks once, account checks per account directory, each result's name prefixed `<address>: `.
  - `pcb uninstall --delete-data`: signs out every account.

- [ ] **Step 1: Write the failing tests** (CLI rig, fake Proton): `TestLoginAddsAccountByUsername` (two logins → `account list --json` two entries); `TestSelectorRequiredWithTwoAccounts` (`calendar list` → exit 2, `data.accounts` has both); `TestPCBAccountEnv` (`PCB_ACCOUNT=b@x calendar list` → `b`'s calendars); `TestStatusShowsEveryAccount`; `TestClientShowPerAccountURL` (`url` ends `/dav/<id>/`, `username` = address); `TestAccountRemoveNeedsYes`; `TestAccountCalDAVOff` (then `account list` shows `caldav: false`); `TestSchemaVersionIs2` (every command's envelope); `TestDoctorNamesTheAccount`; `TestUninstallSignsOutEveryAccount` (both sessions revoked on the fake); `TestServePrintsEveryAccountURL` (`Thunderbird URL (<address>): <base>/dav/<id>/`, one line per account).
- [ ] **Step 2: Run** `go test ./internal/cli` → FAIL.
- [ ] **Step 3: Implement**; help texts say `any username is accepted; pcb shows the account's address`.
- [ ] **Step 4: Run** `make lint test` → PASS; `GOOS=windows go vet ./...` clean.
- [ ] **Step 5: Commit** `✨ Name the account in every CLI command`

### Task 9: Two-account contract and upgrade tests

**Files:**
- Create: `internal/daemon/multiaccount_test.go`

**Interfaces:**
- Consumes: `daemon.Run`, the fake, go-webdav as test client (test-only, as today).

- [ ] **Step 1: Write the tests**: `TestTwoAccountsSideBySide` (two webdav clients on their per-account URLs: each sees only its calendars; a PUT through `a` lands in `a`'s Proton calendar and never in `b`'s; `b` signed out over CTL → `a` keeps answering 207, `b`'s URL 503); `TestMigratedThunderbirdKeepsItsTokens` (an M5 state with a filled cache: a compatibility-URL `sync-collection` token and ETags taken before the upgrade, M6 `Run` on it, the same request with the old token answers the empty delta and the same ETags).
- [ ] **Step 2: Run** `go test ./internal/daemon -run 'TwoAccounts|Migrated'` → they must FAIL when sabotaged: check by routing every password to the first account (first test red) and by skipping the cache move (second red), then restore.
- [ ] **Step 3: Run** `make lint test` → PASS.
- [ ] **Step 4: Commit** `🧪 Prove two accounts side by side and the M5 upgrade`

### Task 10: Documents

**Files:**
- Create: `spec/decisions/0014-several-accounts-per-process.md`
- Modify: `spec/decisions/0013-viewable-caldav-credentials.md` (username = address, amended 2026-11-12 by 0014), `spec/decisions/INDEX.md`, `spec/SPEC.md` §3 (GUIs: planned in M7), §4 (one process, several accounts), §7.1 (layout v2), §9 (URLs, realms), §13 (selector, `pcb account`, one account per daemon removed, schema 2), §15 (CTL v2, `account-stopped`), §18 (M6 row, M7 row "planned"), `README.md` (several accounts, no downgrade after M6, per-account URL), `spec/HUMAN-CHECKS.md` (new step 8: upgrade the test account's M5 state on the step 7 throwaway VM, Thunderbird keeps syncing, then a new Thunderbird calendar on the per-account URL; check Thunderbird keeps the realm-distinct password), `spec/OPEN.md` (Thunderbird auth-cache check; multi-account is fake-only by the author's choice).

- [ ] **Step 1: Write** the documents; every `[verified: …]` names a test from Tasks 0 to 9.
- [ ] **Step 2: Check** every test name cited exists: `grep -o 'Test[A-Za-z0-9_]*' spec/SPEC.md | sort -u | while read t; do grep -rq "func $t" internal || echo "missing $t"; done` → no output.
- [ ] **Step 3: Commit** `📝 Document several accounts per process`
