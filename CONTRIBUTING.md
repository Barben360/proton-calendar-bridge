# Contributing to proton-calendar-bridge

How to propose a change. Building, testing and the developer tools are in [docs/development.md](docs/development.md); user documentation is in [docs/](docs/).

## Before you start

- For anything bigger than a small fix, open an issue first and say what you want to change and why.
- Never point a development build at a Proton account you care about. Tests run against a fake Proton; live checks use a dedicated test account only ([docs/development.md](docs/development.md#live-sessions)).
- Security problems: do not open a public issue. Report them privately through the repository's Security tab ("Report a vulnerability").

## The gate

```sh
make lint test
```

It needs Go (the version in `go.mod`), a C compiler and the GL/X headers of the desktop app ([docs/install.md](docs/install.md#build-dependencies-of-the-desktop-app)). CI runs the same gate, plus a secret scan (gitleaks), the container and systemd smoke tests and a check of the pull request's title.

## Rules for the code

- **Tests that bite.** Each test fails when the behaviour it guards breaks: assert what a user or a caller observes, never an assertion that passes either way. Write the failing test first.
- **Machine-readable output.** Every command has `--json`, printing one envelope `{schema_version, command, ok, data | error}` and nothing else on stdout (`--help` is the one exception: it prints usage text, exit 0), with the exit codes of `internal/exitcode`: `0` done, `2` bad invocation, `10` you must act, `20` blocked, `30` failed and rolled back, `40` partly done and resumable.
- **No secrets anywhere.** Never in arguments, logs, errors, recorded files or commits. Secrets come from a terminal prompt, a file descriptor or the environment. A test value that looks like a secret carries an inline `gitleaks:allow` comment with the reason; a false positive already in the history goes in `.gitleaksignore` (`file:rule:line`, with the reason).
- **Code from elsewhere.** Logic reused from another project is audited, rewritten with our own tests and credited in the README's Credits section. Never copy from ProtonMail/WebClients (GPL-3.0).

## Rules for the docs

- The README is the landing page: what pcb does, how to run it, links. Details go in the page of `docs/` for that task; design, decisions and experiments go in `spec/`.
- User pages carry no internal labels (milestone codes, decision numbers, ruling IDs, `[verified: …]` tags): say what changed in plain words.
- A change of behaviour updates the page that describes it, in the same pull request. A change to the `pcb-ui` window comes with fresh screenshots: `make screenshots`, then commit `docs/assets/ui/`.

## Commits and pull requests

- One topic per pull request. Pull requests are **squash-merged**, so the pull request's title becomes the commit on `main`.
- Titles and commit subjects follow [gitmoji](https://gitmoji.dev): the emoji itself (not `:sparkles:`), a space, then a capitalised imperative subject, e.g. `✨ Add free/busy export` or `🐛 Keep alarms on a moved occurrence`. CI refuses a pull request whose title does not.
- Say in the description what changes, why, and how you checked it.

## License

Contributions are released under the project's [MIT license](LICENSE).
