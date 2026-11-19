# Crash and error reports — design

- **Date**: 2026-11-19
- **Status**: approved by the author on 2026-11-19; built (decision 0022)
- **Decided by**: author (choices marked [author]); the rest is the agent's call, marked [call]
- **Amends**: SPEC §19 and `docs/security.md` "What never leaves pcb" (a new outbound host, after consent); decision 0015 (pcb-ui: a new prompt, panel and Settings switch); to be recorded as decision 0022

## 1. Intent

The author wants the crashes and the bug-revealing errors of every user of the public releases, so they can fix them [author]. Today a crash leaves at best a few lines in a log file on someone else's machine, and the author hears of it only if that user opens an issue.

Reports land in the author's self-hosted Bugsink (Sentry protocol) on the homelab, under a neutral hostname, `errors.bbarrois.tech` [author: neutral; call: the name], beside the InfirLib project it already serves.

pcb is a privacy tool: it holds Proton secrets and calendar contents. A report is sent only after the user said yes, after a crash [author: "ask after a crash"], and carries no event data, no secret, no name, no address, no path of the user's machine.

## 2. Goals, non-goals, anti-goals

Goals:
- A fatal crash of `pcb` or `pcb-ui` (an unrecovered panic in any goroutine, or a Go runtime fatal error) is saved on disk and offered at the next start of any pcb process.
- An error that points to a bug (§5.3) is saved and offered the same way.
- `pcb-ui` asks with Send / Always send / Don't send, and can show the exact report first. Headless users (`pcb serve`, systemd, the container) are told by `pcb doctor` and the serve log, and answer with a command or an environment variable.
- Each report reaches the author's Bugsink grouped by cause, with the release, the OS and the binary.

Non-goals:
- Logs, breadcrumbs, performance traces, session tracking, user counts.
- Reports from `pcb-fake`, `pcb-lab`, dev builds or builds from source (no address is built in, §5.6).
- A crash report for the author's own Bugsink UI or for InfirLib (unchanged).

Anti-goals (a failure even if reports arrive):
- Any byte of event content, calendar or account name, account address, Proton ID, token, password, hostname, username, local path or IP-derived data leaving the machine.
- A report sent without a yes, or a "Don't send" that does not delete it.
- Reporting that slows pcb, changes an exit code, a `--json` envelope or a CalDAV answer, or prints anything on a failed send.
- An install flooding Bugsink (a crash loop under `restart: unless-stopped`).

## 3. Constraints

- Every byte sent is chosen by pcb, field by field (allow list), never a library's default event with fields removed [author: approach A].
- Standard library only, like `internal/update` and `internal/logfile` [call, follows approach A].
- Bugsink 2.6.3 accepts Sentry envelopes; it was proven with sentry-dart only, so a Go-shaped event must be checked against a real Bugsink first (§7, step 1).
- `CONTRIBUTING.md`: `--json` envelope and exit codes on every new command; tests that bite; no secret anywhere (the DSN's public key is not a secret by Sentry's design, but it stays out of the repository, §5.6).
- AGENTS.md: no machine name, local path or account address of the author in the repository. The hostname `errors.bbarrois.tech` is public by nature (it is in every release binary and named in `docs/security.md`), so it may appear in docs; the DSN does not.
- Windows: `pcb-ui.exe` is built `-H=windowsgui` (no console); the crash file must work there.

## 4. Approaches

| | A. Own small client (chosen) [author] | B. `sentry-go` SDK |
|---|---|---|
| How | ~500 lines, standard library: parse the crash file, build a Sentry event, spool it, send after consent | The SDK builds and sends; a custom transport spools until consent; `BeforeSend` scrubs |
| Privacy | Allow list: only fields pcb writes exist | Deny list on SDK events; new SDK versions can add fields (e.g. `server_name` is the hostname by default) |
| Fit | Same style as the update check | Global hub, integrations, a dependency tree |
| Cost / risk | More code; Bugsink acceptance to prove first | Less format code; Sentry-native |

Both use `runtime/debug.SetCrashOutput` for fatal crashes (the only way to see a panic in a goroutine pcb does not wrap). Rejected: a watchdog parent process re-executing pcb (doubles every process, breaks the container's PID 1 and `pcb-ui`'s single-instance logic) [call].

## 5. Design

### 5.1 Package `internal/report` (new) [call]

Standard library plus `internal/atomicfile`, `internal/filelock`, `internal/acl` and `internal/buildinfo` only (an `arch_test.go` pins it, like `internal/logfile`). It owns: the crash file, the parser, the event builder, the spool, the consent state and the sender. Callers: `internal/cli`, `cmd/pcb-ui`, `internal/ui`, `internal/daemon`, `internal/service`, `internal/doctor`.

### 5.2 Fatal crashes [call]

- At start, every `pcb` and `pcb-ui` process (before any other goroutine starts: first lines of `cli.Execute` and `cmd/pcb-ui` `run`) creates `<state>/reports/crash-<pid>-<unix-nanos>.txt` (directory private like `logs/`, file 0600) and passes it to `debug.SetCrashOutput`. A clean exit removes it. A failure to create it is ignored (no reporting for that process, no message).
- A process ending in a crash leaves a non-empty file. The next start of any pcb process turns every non-empty `crash-*.txt` older than its own into a pending report (§5.4) and deletes it; empty ones older than 24 h (a kill -9, a power cut) are deleted.
- The state directory is not known before flags and config are read for `pcb` commands that take `--state`; the builder places the call as early as the state directory is known and documents the window before it (a crash there is not reported).
- `GOTRACEBACK` is left at Go's default: a panic prints the panicking goroutine only. A runtime fatal error (`concurrent map writes`) prints all goroutines; only the first goroutine block is kept.

### 5.3 Errors that point to a bug [call]

- `internal/service/errors.go` `mapError`'s fall-through (`"unexpected error"`, today `blocked`) records a report: the error's type chain (`%T` of each `errors.Unwrap` / `Unwrap() []error` level, up to 8) and the stack where `mapError` was called (`runtime.Callers`). The error's text is never kept.
- CalDAV and MCP HTTP handlers: `net/http` recovers a handler's panic and only logs it. A middleware around the daemon's handler (`internal/daemon/daemon.go`, `srv.Handler`) recovers, records a report from the panic value's type and `debug.Stack()`, then re-panics `http.ErrAbortHandler` so the client sees what it sees today.
- Not reported: anything `mapError` classifies (Proton down, rate limits, network, credentials, sealer), usage errors, rollbacks.
- Same fingerprint (§5.4) more than once a day: kept once (a count is added to the pending file); this applies to crashes and errors alike [call].

### 5.4 The report and what it holds (allow list) [call]

A pending report is the exact JSON event that will be sent, `<state>/reports/pending-<event_id>.json`, so "show" displays what leaves the machine. At most 20 pending; the oldest is dropped past that.

| Field | Value |
|---|---|
| `event_id`, `timestamp` | random, now |
| `platform` | `go` |
| `level` | `fatal` (crash), `error` (§5.3) |
| `release` | `pcb@<buildinfo.Version>` |
| `environment` | `production` |
| `tags` | `binary` (`pcb` / `pcb-ui`), `command` (the cobra command path, e.g. `pcb serve`; never its arguments), `install` (derived the way `internal/update/hint.go` reads an install: `container` from `PCB_INSTALL`, else `script` / `msi` / `other` from where the binary sits; no new marker is stamped by the installers [call]), `source` (`crash` / `error` / `http-panic`) |
| `fingerprint` | `[mechanism type, exception type, in-app function names of the top 5 in-app frames]`: the local dedup key too. Function names are pcb's own code and already in the frames, so it adds no information; it keeps Bugsink from folding every `"(text removed)"` panic into one issue [call]. The spike (§7) checks Bugsink honours it |
| `contexts.os` | `name` = GOOS only; `contexts.runtime` = Go version; `contexts.device.arch` = GOARCH |
| `exception.values[0].type` | crash: the runtime error kind (`runtime error`, `fatal error`, `signal SIGSEGV`) or `panic`; error: the outermost Go type |
| `exception.values[0].value` | kept only when the line is a Go runtime message (`panic: runtime error: …`, `fatal error: …`, `[signal …]`); otherwise `"(text removed)"`. Error type chain for §5.3 |
| `exception.values[0].stacktrace.frames` | function, module-relative file, line, `in_app` for this module. No argument words (the hex values after a function name are dropped). A path that is not module-relative or GOROOT-relative (a build without `-trimpath`) is cut to its last two elements |
| `exception.values[0].mechanism` | `{type: "go.crash" / "go.error" / "go.http", handled: false/true}` |
| `extra.count` | repeats folded into this report (§5.3) |

Built: Bugsink 2.6.3 accepts this shape through `/api/<project>/envelope/` and keeps every field above (release, tags, fingerprint, contexts, `in_app`, `mechanism.handled: false`, `extra.count`); it honours `fingerprint` (same stack, other fingerprint = another issue; same fingerprint = one issue with two events). `make bugsink-check` re-runs the check.

Never set: `server_name`, `user`, `request`, `breadcrumbs`, `modules`, `contexts.os.version`, any environment variable. A test builds a report from a panic whose text, arguments and wrapped errors carry marker strings, and asserts none appear in the JSON.

### 5.5 Consent [author: ask after a crash; details call]

- `<state>/reports.json`: `{"mode": "ask" | "always" | "never"}`, default `ask`; read and written like `update.json` (`Load`/`Edit` under a lock).
- `PCB_CRASH_REPORTS=always|never` overrides it for one process (the container on the homelab sets `always`).
- `always`: pending reports are sent by the next long-lived process (below, §5.6). `never`: new reports are not even saved; switching to `never` deletes the pending ones. `ask`: they wait.
- `pcb-ui`, at start with pending reports and mode `ask`: a dialog, "pcb stopped unexpectedly" (a crash) or "pcb ran into an unexpected error" (errors only), with "Show report" (the JSON, read-only, scrollable), "Send", "Always send", "Don't send" (deletes them). Closing the dialog = ask again next start.
- `pcb-ui`, a report saved while it runs with mode `ask`: a panel at the top of the window, the same buttons, styled like the update panel (`docs/desktop.md`). No OS notification [call].
- Settings: a "Send crash reports" switch (on = `always`, off = `ask`; `never` is a CLI choice), next to "Check for updates".
- CLI: `pcb crash-reports status | show | send | discard | always | ask | never`, each with `--json`. `send` and `show` act on all pending; `status` gives mode, override, pending count.
- `pcb doctor`: a `crash reports` line, "N waiting: `pcb crash-reports show`, then `send` or `discard`", only when pending and mode `ask`. `pcb serve` logs the same once at start.
- Other CLI commands never prompt. With pending reports and mode `ask`, a command run with stderr on a terminal and without `--json` prints one line on stderr, at most once a day: "pcb kept a crash report: pcb crash-reports show" [call]. CLI-only installs (macOS, servers without `pcb-ui`) learn of reports this way too.

Who collects and who sends [call]:
- Every `pcb` and `pcb-ui` process collects at start (turns crash files into pending reports): local file work only, no network.
- Only long-lived processes send automatically in `always` mode: `pcb serve` (systemd, container) and `pcb-ui`, in the background, at start and when a new report is saved. `pcb crash-reports send` sends on request and waits. Short commands (`pcb status`, health checks, `pcb sync` from cron) never send, so they never wait and never use up an attempt.
- An attempt (§5.6) is counted only when an HTTP request was actually made and failed.

### 5.6 Sending [call]

- Endpoint: `buildinfo.ReportDSN`, set with `-ldflags -X` by `make build` / `make ui` / `make image` from the variable `REPORT_DSN`; `release.yml` and `image-build.yml` pass the GitHub secret `PCB_REPORT_DSN`. Empty (dev, source builds) = no report is ever saved or sent, and the CLI says "this build has no crash report address".
- One POST per report to `https://errors.bbarrois.tech/api/<project>/envelope/`, header `X-Sentry-Auth: Sentry sentry_version=7, sentry_key=<public key>, sentry_client=pcb`, `User-Agent: pcb`, no cookie, no redirect followed, 10 s timeout, the dedicated client of `internal/update.NewClient` style.
- 2xx: delete the pending file. 429: keep, honour `Retry-After` (stored in `reports.json`). Other failures: keep, retry at the next start, at most 3 attempts then delete [call].
- At most 10 sends per install per day (counted in `reports.json`) [call]. A failed send says nothing outside `pcb crash-reports send` (which reports per report in its envelope).
- Bugsink is already configured to ignore client-IP headers; it records Traefik's internal address. `docs/security.md` says GitHub-style "the server sees your IP, as any visit; it is not stored".

### 5.7 Homelab side (repository my-homelab, its own PR) [call]

- `services/bugsink/compose.yaml`: routers for `Host(errors.bbarrois.tech)`: ingestion path public (same `PathRegexp` as today), everything else `lan-only@file`, HTTP redirect. `BASE_URL` stays `https://erreurs.infirlib.fr` so InfirLib's DSN and mails are unchanged.
- `service.yaml` `hosts:` gains `errors.bbarrois.tech`; DNS as the repo does for other `*.bbarrois.tech` hosts; `internal/config/bugsink_repo_test.go` updated.
- In the Bugsink UI: a project `pcb`; its DSN, with the host changed to `errors.bbarrois.tech`, goes into the GitHub secret `PCB_REPORT_DSN` (author, by hand: `gh secret set`).
- `services/proton-calendar-bridge/compose.yaml`: `PCB_CRASH_REPORTS: always`.
- Bugsink retention for the project: Bugsink's defaults; the author may lower it in the UI.

### 5.8 Docs [call]

- `docs/security.md`: "Crash reports" section beside "The update check" (what is sent, the allow list in plain words, where, when, how to see, turn off, delete); "What never leaves pcb" amended.
- `docs/desktop.md` (dialog, panel, switch, screenshots via `make screenshots`), `docs/commands.md` (`pcb crash-reports`), `docs/container.md` (`PCB_CRASH_REPORTS`), `docs/troubleshooting.md` (a crash: what pcb keeps and offers).
- SPEC §19 and a decision 0022.

## 6. Tests that bite

- A helper subprocess (the test binary re-executed) that panics in a goroutine, with marker strings in the panic text and an argument, under the real crash-file setup: the next start's pending report exists, has the expected frames, and contains no marker.
- Same with `panic(runtime error)`: the runtime message is kept. With a `concurrent map writes` fatal error: one goroutine kept.
- `mapError` fall-through with a wrapped error whose text carries a marker: a report with the type chain, no marker. A classified error (Proton 503, network): no report.
- HTTP handler panic: the client sees the connection aborted as today, and one report exists.
- Consent: `ask` keeps, `always` sends, `never` saves nothing and deletes the pending ones, `PCB_CRASH_REPORTS` overrides the file, "Don't send" deletes. Empty `ReportDSN`: nothing saved.
- Sender against an `httptest` server: auth header, path, body equal to the pending file, no cookie, no redirect followed; 429 with `Retry-After` keeps and delays; 3 failures delete; the daily cap holds.
- Dedup and the 20-report cap.
- pcb-ui: the dialog and panel buttons do what they say (model tests in `internal/ui`, like `update_test.go`), screenshots refreshed.
- `pcb crash-reports` commands: `--json` envelope and exit codes (`clitest` harness).
- Windows: crash file and parsing in the `windows-test` CI job.

## 7. Order and the Bugsink check

1. Spike first (throwaway): a hand-written Go-shaped event posted to `bugsink/bugsink:2.6.3` in a local container. It must show grouping by stack, `in_app` frames, release, tags, `handled: false`. If Bugsink drops or mangles a field, the event shape in §5.4 changes before any code is written. A `make bugsink-check` target (not in the gate) keeps it re-runnable.
2. `internal/report` (crash file, parser, builder, spool, consent, sender) with its tests.
3. Wiring: CLI, daemon middleware, `mapError`, doctor, serve log.
4. pcb-ui dialog, panel, switch.
5. Release and image workflows (`REPORT_DSN`), docs, decision.
6. Homelab PR (§5.7), the Bugsink project and the secret; then a release candidate and a forced crash on the test machine end to end.

## 8. Left to the builder

- Exact Go API of `internal/report`, file names inside `reports/`, wording of dialog and panel text (plain, like the update panel).
- The cobra hook used to know the state directory early (§5.2).
- Whether pcb-ui's dialog is a Fyne dialog or a window section.
