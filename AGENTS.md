# Repository rules

[CONTRIBUTING.md](CONTRIBUTING.md) holds the rules every change follows: tests that bite, the `--json` envelope and exit codes, no secrets, code from elsewhere, the docs rules, gitmoji commits and squash-merged pull requests. Read it first. This file adds what an agent working in this repository also needs.

## Build and test

- Go module `github.com/Barben360/proton-calendar-bridge`; binaries `pcb` (static, no GUI) and `pcb-ui` (Fyne, cgo). Gate: `make lint test`; it compiles `cmd/pcb-ui`, so it needs a C compiler and the GL/X headers.
- Windows: the Windows code compiles on Linux, so check it with `GOOS=windows go vet ./...` (and `GOOS=windows CGO_ENABLED=0 go build ./cmd/pcb`); `pcb-ui` for Windows builds only on Windows (cgo, MSYS2 UCRT64). Windows tests run through `scripts/win/vmtest.sh` on a Windows machine over SSH, its target in `PCB_WIN_SSH` (never written in the repository; `docs/development.md`), and in CI's `windows` and `windows-test` jobs. Only code that calls Windows APIs is build-tagged `windows`.
- Live-session tooling: `make live-preflight-test` (guard), `make shellcheck` (pinned image, as CI), `make rehearse` (`pcb` against the loopback fake `cmd/pcb-fake`), `scripts/live/run.sh` (`spec/HUMAN-CHECKS.md`). `pcb-fake` and `pcb-lab` are developer tools, never built into the image.

## Live Proton calls

- Only against the test account (username and password from your password manager, through local copies of `scripts/*.env.example` in `~/.config/pcb-live/`), never the author's own.
- Hosts that must never run a live command are listed in the local `~/.config/pcb-live/hosts` (`scripts/live/README.md`), never in the repository. Never write a machine name, local path, account address or Proton ID of the author into the repository.
- At most one login per lab run, 60 s minimum between lab logins, honour `Retry-After`.

## Git

- Commit subjects and pull request titles: `<gitmoji> <Capitalised imperative>` (CONTRIBUTING.md). Merge with `gh pr merge <n> --squash`.
- Workflow files are pushed over SSH (`git@github.com:`): the local gh token lacks the `workflow` scope.
