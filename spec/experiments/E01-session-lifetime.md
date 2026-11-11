# E01 — Session lifetime and refresh

Feeds C1 auth, the refresh policy (M1) and §5.2 (SPEC §17).

## Question

1. How long does an access token live (`ExpiresIn`, and in practice)?
2. Does a refresh rotate the refresh token (and the access token)?
3. Does Proton still honour the previous refresh token after a rotation?
4. What ends a session when nothing refreshes it, and after how long?

## Method

All probes use the saved test-account session (`session.json` in `~/.local/state/pcb-lab`), never a new login.

- `pcb-lab probe refresh [--record-dir spec/experiments/raw/E01]`: one `POST /auth/v4/refresh` with the saved refresh token, saves the new pair, then one retry with the previous refresh token. It reports HTTP status, Proton code, `ExpiresIn` (as returned, 0 when absent), whether the refresh and access tokens changed, and whether the old refresh token was accepted (a success is saved too, so the file holds the newest pair). The refresh is a raw call rather than go-proton-api's, because go-proton-api's `Auth` drops `ExpiresIn` and the old-token answer's Proton code. Run it after the other probes: replaying an old refresh token may end the session on Proton's side.
- `pcb-lab probe alive`: one authenticated `GET /core/v4/users` with the saved access token and no refresh; appends `{at, ok, code}` to `~/.local/state/pcb-lab/e01.jsonl`. After 14 days from the first sample it samples nothing and says so. This measures how long an unrefreshed access token keeps working, and the code once it stops.
- `scripts/e01-timer/pcb-lab-e01.{service,timer}`: a user timer every 6 h (`OnCalendar=*-*-* 00/6:00:00`), `Persistent=true` so runs missed while the desktop slept catch up (ruling R4), running only `probe alive`.

Tested offline against the shared fake (`internal/lab/probe/session_test.go`, `cmd/pcb-lab/main_test.go`): rotation and old-token refusal, `ExpiresIn` parsing, no retry after a failed refresh, no token in recordings, one line per `alive` run, no refresh or login from `alive`, the 14-day stop, and the timer units' content.

## Results

No live measurement yet (the test account's CAPTCHA is pending, HUMAN-CHECKS step 1).

## Pending live run (HUMAN-CHECKS step 1)

Right after the author solves the CAPTCHA and the first `Other` login succeeds (sequence in `E03-client-identity.md`), and again once the final `Other` login of that sequence is done:

```
cd /home/jduff/Workspace/worktrees/proton-calendar-bridge-m0
go build -o ~/.local/bin/pcb-lab ./cmd/pcb-lab

# Immediate results (right after login #1 and the E03 probe on Other):
pcb-lab probe refresh --record-dir spec/experiments/raw/E01 --json

# Long probe, installed only after the last login of the live run (a later
# login replaces the session the timer samples):
pcb-lab probe alive                       # first sample now, checks the wiring
install -Dm644 scripts/e01-timer/pcb-lab-e01.service ~/.config/systemd/user/pcb-lab-e01.service
install -Dm644 scripts/e01-timer/pcb-lab-e01.timer   ~/.config/systemd/user/pcb-lab-e01.timer
systemctl --user daemon-reload
systemctl --user enable --now pcb-lab-e01.timer
systemctl --user list-timers pcb-lab-e01.timer
```

`probe refresh` is not a login, so the 60 s guard does not apply, but it is run once per session. Its replay of the old refresh token comes last for that reason; if the session dies, `pcb-lab login` is needed and the guard and E02 protocol apply.

Uninstall (and at the end of the 14 days; `alive` also prints this):

```
systemctl --user disable --now pcb-lab-e01.timer
rm ~/.config/systemd/user/pcb-lab-e01.{service,timer}
systemctl --user daemon-reload
```

Fill in here: `ExpiresIn`; refresh token rotated (yes/no); access token rotated; old refresh token accepted (yes/no, Proton code); then "long probe running since <date>", and after the window the first `ok:false` line (age of the session when it ended, code). The state of `e01.jsonl` goes in `raw/E01/` as `alive.jsonl` (no secrets in it: `at`, `ok`, `code`).

## Feeds

C1 refresh policy (M1): how often to refresh, whether to keep the old token, what an unattended re-login must expect (§5.2).
