# 0022 — Crash and error reports, sent only after a yes

- **Date**: 2026-11-19
- **Decided by**: author (reports of crashes and bug-revealing errors of the public releases reach the author's own Bugsink under the neutral name `errors.bbarrois.tech`; every byte chosen by pcb, field by field, with no SDK; asked after a crash, never before), the rest the agent's calls (design `spec/designs/2026-11-19-crash-reports-design.md`)
- **Status**: accepted; built 2026-11-19; amends SPEC §19 and `docs/security.md` "What never leaves pcb" (a second host besides Proton and github.com, after consent), and decision 0015 (pcb-ui's dialog, panel and Settings switch)

When pcb crashes, or meets an error that points to a bug, it keeps a report on disk and offers it. Nothing leaves before the user agrees.

- **What is kept.** A fatal crash of `pcb` or `pcb-ui` (`runtime/debug.SetCrashOutput`, read at the next start of any pcb process), an error that `mapError` cannot classify, and a panic in a CalDAV or MCP handler (middleware around the daemon's handler). Not Proton being down, rate limits, network, credentials, usage errors. Dev and source builds have no address and keep nothing.
- **Allow list.** The report is the exact JSON event that will be sent (`reports/pending-<id>.json` in the state directory), built from named fields only: release, OS name, architecture, Go version, binary, command (never its arguments), install kind, failure type, pcb's function names with file and line, a repeat count. A panic's or error's text is dropped, kept only when it is a Go runtime message. Never `server_name`, user, request, environment, a path of the machine. Standard library only: no `sentry-go`, whose events carry the host name by default and whose fields are removed instead of chosen.
- **Consent.** `reports.json` holds `ask` (default), `always` or `never`; `PCB_CRASH_REPORTS` overrides it for one process. `pcb crash-reports status|show|send|discard|always|ask|never`; `pcb-ui` asks with Show report, Send, Always send, Don't send (a dialog at start, a panel while it runs, Settings "Send crash reports"); `pcb doctor`, the `pcb serve` log and a once-a-day stderr line tell a headless user. Other commands never prompt.
- **Who sends.** Every process collects at start (local files only); only `pcb serve` and `pcb-ui` send in the background, and `pcb crash-reports send` on request, so a short command never waits.
- **Limits.** 10 sends a day, 20 waiting, 3 failed attempts then the report is dropped, `Retry-After` honoured, the same cause once a day, no redirect, 10 s timeout, nothing printed on a failed send.
- **The address.** `buildinfo.ReportDSN`, stamped by `make build`, `ui` and `image` from `REPORT_DSN`; the release and image workflows pass the secret `PCB_REPORT_DSN`. A missing secret builds without reporting. `make test` and `make lint` never stamp it.
- **The server.** Bugsink 2.6.3 accepts the event shape and honours the fingerprint (`make bugsink-check`). It sees the IP of the request and is configured not to record it.

Why:
- The author hears of a crash only if a user opens an issue; a crash file on someone else's machine is lost.
- pcb is a privacy tool: choosing each field makes a leak a visible code change, not a library upgrade.
- Asking after a crash keeps the yes explicit; `always` serves the author's own container and those who choose it.

Consequences and limits:
- A crash before the state directory is known (a bad flag) is not reported.
- A kill, a power cut or an out-of-memory kill leaves an empty crash file and no report.
- The Bugsink host, its retention and the secret live outside this repository (the homelab's own repository, the GitHub secret): a lost secret silently turns reporting off in the next release.
- Cost if wrong: `internal/report`, `internal/cli/crashreports.go`, the daemon and service hooks, the dialog and panel in `internal/ui/desktop`; removing the feature leaves unused `reports/` and `reports.json`.
