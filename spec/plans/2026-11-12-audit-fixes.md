# Initial-version audit fixes — plan

> **For agentic workers:** use superpowers:subagent-driven-development. Each task reads its audit reports in the SDD workspace (`audit/<area>.md`); finding IDs below are `<area>-<ID>` from those reports.

**Goal:** fix what the deep audit of the initial version (M0–M5, main at 557fe84, 2026-11-12) found. That is every Important finding and the Minors worth their cost; the rest is parked in `spec/OPEN.md` with a reason.

**Spec:** `spec/SPEC.md` (authority), `spec/mapping.md`, decisions 0001–0013. The audit reports are the evidence. A report's proposed fix is a suggestion, and this plan's ruling wins.

## Global Constraints
- **Test discipline.** Every fix has a test that goes red without it (record the sabotage). Use no live Proton, only the protontest fake.
- **Secrets** never appear in argv, logs, errors, or JSON without `--reveal`.
- **CalDAV.** A not-ready or transient state answers 503 with Retry-After and never a 4xx. A 4xx is reserved for refusals that the docs list.
- **Exit codes:**

  | Code | Meaning |
  |---|---|
  | 0 | ok |
  | 2 | usage |
  | 10 | MustAct: the user must act; systemd does not restart |
  | 20 | transient, or no daemon |

  The same failure gets the same code in every command.
- **Messages.** One sentence stating the cause, then a `Hint:` with one runnable next step. Messages never show Go error chains. Every new or changed message is quoted in a test.
- **No over-development.** Fix the defect and nothing around it.

## Triage

**Ruling key:** F = fix (task named), P = park in OPEN.md with the reason, A = accept as is, D = docs only.

| Finding | Ruling | Task |
|---|---|---|
| proton-I1 no timeout on login, 2FA, AuthInfo and unlock calls | F: a per-call deadline inside package auth | 1 |
| proton-I2 any 422 means "wrong password" | F: match Proton code 8002 only, as `Scope.wrongPassword` does | 1 |
| proton-M1 refresh holds c.mu for up to 30 s, and Tokens() ignores ctx | P | — |
| proton-M2, M7 Retry-After lost on login 429, and the HTTP-date form ignored | F | 1 |
| proton-M3 keyPass not zeroed | F | 1 |
| proton-M4 = state-I3 future-dated loginguard stamp | F: treat a stamp in the future (beyond 5 min of skew) as stale, log it once, never block | 1 |
| proton-M5 passphrase verify time vs cards time 0 | P (crypto semantics; needs a live check) | — |
| proton-M6 9001 on an authenticated read untyped | F: typed as human verification (alert `human-verification`, its hint) | 1 |
| state-I4 a CAPTCHA during login while ready overwrites a working state | F: an interactive login that hits a CAPTCHA leaves the stored state as it was | 1 |
| state-I1 a sealed store this build cannot read is a dead end | F: name the cause (newer pcb or another sealer: "upgrade pcb"); give `pcb logout` a confirmed local wipe when the store cannot be read (it says it cannot revoke) | 2 |
| state-I2 state.json refuses unknown fields with the hint "logout" | F: stay strict; the message says "written by a newer pcb: upgrade pcb"; never suggest logout for it | 2 |
| state-I5 synchronous(NORMAL) lets seq roll back within an epoch | F: `synchronous=FULL` | 2 |
| state-I6 cache migration in two transactions | F: one transaction, or an idempotent backfill on open when version is 2, rows exist and pins is empty | 2 |
| state-M root run on a system install leaves files the pcb user cannot read | F: refuse to run as root when the state dir belongs to another user (hint `sudo -u pcb pcb …`) | 2 |
| state-M logout does not take sync.lock | F | 2 |
| state-M stale `.tmp-*` and `.key-*` after a crash | F: remove those older than 1 h at start | 2 |
| state-M secrets.sealed removal without a dir fsync | F | 2 |
| state-M pcb.db mode not repaired | F | 2 |
| state-M tombstones never pruned | P | — |
| state-M login forces a logout step after a rollback | P | — |
| surface-I1 prop names echoed with unbounded length | F: refuse a name or namespace over 256 bytes (400, before any work) | 3 |
| surface-I2 corrupt clients.json gives a silent 401 | F: log the load error (Error, once per change) and answer 503; it is not a limiter failure | 3 |
| surface-M1 0.0.0.0 also binds `[::]` | F: "tcp4" for an IPv4 literal; `[::]` stays dual stack | 3 |
| surface-M2 SANs list every interface IP | P (LAN peers know the IP; `hostnames` exists) | — |
| surface-M3 certificate not regenerated when an interface IP changes | P | — |
| surface-M4 fingerprint change logged at Info | F: Warn, naming old and new fingerprint | 3 |
| surface-M5 no renewal while running | P (the alert says restart) | — |
| surface-M6 a local user can lock out loopback Thunderbird | P (with the Docker NAT item) | — |
| surface-M7 a WARN per failed TLS handshake | F: Debug | 3 |
| surface-M8 `/.well-known/caldav` answers 308 to every method | A | — |
| surface-M9 8 MiB PROPFIND and REPORT bodies, no connection cap | F: 1 MiB cap for PROPFIND and REPORT; connection cap P | 3 |
| read-I1 render counters never pruned | F: prune to live row IDs where `keepDuplicates` runs | 4 |
| read-M1 duplicate RECURRENCE-ID both emitted | F: keep the most recently edited, count the rest as duplicates | 4 |
| read-M2 no control-character guard on read | F: drop C0 controls except HTAB in emitted values (CR and LF in TEXT are escaped already) | 4 |
| read-M3 recurring timed placeholder in UTC | F: use the row's start timezone when it is a known tzdata zone | 4 |
| read-M4 one malformed reminder turns the event into a placeholder | F: drop that alarm only, and count it | 4 |
| read-M5 O(N) per single GET | P | — |
| read-M6 LRU "2 000 by default" docs | D | 7 |
| read-M7, M8 | P | — |
| write-I1 refusals not logged | F: one Warn per refusal (code, calendar, href; no event content) | 5 |
| write-I2 UID column vs parts divergence gives a 409 self-conflict | F: compare unescaped; a conflict that names the object itself is never a 409 (503 per SPEC/OPEN) | 5 |
| write-I3 personal part ignored by the read-back and not re-sent | F: refuse updates of rows that carry a personal part (403 `pcb:personal-part`, listed in mapping.md and README) until live check L.4 #10 | 5 |
| write-M4 If-Match lists and weak tags | F: parse the list; a weak tag never matches (RFC 9110 strong comparison) | 5 |
| write-M5 alias kept when a create never reached Proton | P | — |
| write-M6 `AttendeeEdits` used only for a debug line | F: remove | 5 |
| write-M1, M2, M3 docs drift | D | 7 |
| ux-I1 "a sync is already running" while serve holds the lock; the lock is taken before the login check | F: check the login first; a holder that is the daemon (CTL answers) gets "pcb serve is running and already syncs" | 6 |
| ux-I2 doctor exits 20 on a fresh install waiting for login; a bad config gives 20 | F: needs-login while the daemon waits is a warning (exit 0); a config error is 10 | 6 |
| ux-I3 key and state errors are 10 in serve, 20 elsewhere | F: 10 everywhere | 6 |
| ux-M wrong key size gives no cause in `client add` | F | 6 |
| ux-M doctor cannot see sealed copies the key no longer opens (parked M5) | F: doctor warns per client "password cannot be shown: pcb client rotate NAME" | 6 |
| ux-M Go error chains leak (strconv, duplicated "listen tcp") | F | 6 |
| ux-M empty lists print a header only | F: "No CalDAV client yet." etc. | 6 |
| ux-M Ctrl-D at a prompt leaves "Error:" on the prompt line | F | 6 |
| ux-M `login --help` defaults; `--json` advertised where rejected | F | 6 |
| ux-M dead condition in printPass | F | 6 |
| ux-M hint format, "did you mean", `--version`, rotate "Hint:" on success | A | — |
| ux-M unit paths with a space or non-ASCII fail under `--plan` | F: `--plan` shows the refusal with exit 10 and the reason, not 20 | 6 |
| ux-M Windows and macOS behaviour; supported platforms never stated | D: README states Linux (amd64, arm64) supported; macOS and Windows build only and the CLI runs offline | 7 |
| ux-M JSON envelope with data and error (CAPTCHA) | D: SPEC §13 describes it | 7 |
| ux-M README and SPEC drift | D | 7 |

## Tasks

The order is 1 → 7. One implementer at a time works in the worktree.

- **Task 1: Proton and login robustness.** Findings: proton-I1, I2, M2, M3, M4, M6, M7; state-I3, I4.
  - Files: `internal/proton/**`, `internal/loginguard`, `internal/service/login.go`, `session.go`.
  - Commit: `🐛 Bound and classify the login calls`.
- **Task 2: State and store durability.** Findings: state-I1, I2, I5, I6, and the state Minors ruled F.
  - Files: `internal/store/**`, `internal/service/{local,errors,secrets}.go`, `internal/atomicfile`, `internal/cli/logout.go`.
  - Commit: `🐛 Keep the state readable and durable across versions and crashes`.
- **Task 3: Network surface.** Findings: surface-I1, I2, M1, M4, M7, M9.
  - Files: `internal/caldav`, `internal/access`, `internal/tlsutil`, the serve listener.
  - Commit: `🔒 Bound property names and fail closed on a broken clients file`.
- **Task 4: Read path.** Findings: read-I1, M1, M2, M3, M4.
  - Files: `internal/ical`, `internal/service/render.go`, `merge.go`.
  - Commit: `🐛 Keep render counts and recurrence output exact`.
- **Task 5: Write path.** Findings: write-I1, I2, I3, M4, M6.
  - Files: `internal/caldav/write.go`, `internal/service/write*.go`, `spec/mapping.md`, the README refusal list.
  - Commit: `🐛 Log write refusals and stop self-conflicts`.
- **Task 6: CLI and operations.** Findings: ux-I1, I2, I3 and the ux Minors ruled F.
  - Files: `internal/cli/**`, `internal/doctor`, `internal/service/{sync,errors}.go`.
  - Commit: `🩹 Consistent exit codes and next steps`.
- **Task 7: Docs.** Every D row, plus every P row moved to `spec/OPEN.md` "Parked by the initial-version audit" with its reason, plus an audit summary in `spec/audits/2026-11-12-initial-version.md` (scope, counts, this table, what held up).
  - Commit: `📝 Record the initial-version audit`.
