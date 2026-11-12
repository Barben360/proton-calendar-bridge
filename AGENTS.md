# Repository rules

- Go module `github.com/Barben360/proton-calendar-bridge`, binary `pcb`. Gate: `make lint test`. Live-session tooling: `make live-preflight-test` (guard), `make shellcheck` (pinned image, as CI), `make rehearse` (`pcb` against the loopback fake `cmd/pcb-fake`), `scripts/live/run.sh` (spec/HUMAN-CHECKS.md); `pcb-fake` and `pcb-lab` are developer tools, never built into the image.
- Every command has `--json` printing one envelope `{schema_version, command, ok, data | error}` and nothing else on stdout. Exit codes: `0` done, `2` bad invocation, `10` you must act, `20` blocked, `30` failed and rolled back, `40` partly done and resumable (`internal/exitcode`).
- Tests must bite: each fails when the behaviour it guards breaks. Assert observable outcomes; no assertion that passes either way. Write the failing test first.
- No secrets anywhere: never in argv, logs, errors, recorded files or commits. Secrets come from a terminal prompt, a file descriptor or the environment.
- Live Proton calls only against the test account (its username and password from your password manager, through local copies of `scripts/*.env.example` in `~/.config/pcb-live/`), never the author's own. Hosts that must never run a live command are listed in the local `~/.config/pcb-live/hosts` (`scripts/live/README.md`), never in the repository. At most one login per lab run, 60 s minimum between lab logins, honour `Retry-After`.
- Logic reused from other projects is audited, rewritten with our own tests and credited in `README.md`. Never copy from ProtonMail/WebClients (GPL-3.0).
- Workflow files are pushed over SSH (`git@github.com:`): the local gh token lacks the `workflow` scope.
- Commit subjects: `<emoji> <imperative>`.
