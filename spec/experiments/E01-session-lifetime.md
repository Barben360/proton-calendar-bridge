# E01 — Session lifetime and refresh

Feeds C1 auth, the refresh policy (M1) and §5.2 (SPEC §17).

## Question

1. Access-token TTL: what `ExpiresIn` does a refresh return, and does the access token really stop working then?
2. Does a refresh rotate the refresh token (and the access token)?
3. Does Proton still honour the previous refresh token after a rotation?
4. Session lifetime under 6-hourly refresh: how long does a session survive when refreshed every 6 h, and what ends it (code, age)?

## Method

Two separate sessions, never mixed:

- **Disposable session = login #1** of the pending sequence. `pcb-lab probe refresh [--record-dir spec/experiments/raw/E01]` runs **once** on it: one `POST /auth/v4/refresh` with the saved pair (refresh token, and its access token in the body: see the live finding below), saves the new pair, then one retry with the previous pair. It reports HTTP status, Proton code, `ExpiresIn` (as returned, 0 when absent), whether the refresh and access tokens changed, and whether the old refresh token was accepted (a success is saved too, so the file holds the newest pair). It is a raw call rather than go-proton-api's, because go-proton-api's `Auth` drops `ExpiresIn` and the old-token answer's Proton code. A refused first refresh exits 20 (Blocked) with the Proton code and no token. Replaying an old refresh token may end the session on Proton's side, so it is never run on a session anything else uses: the E03 candidates and the seeder log in again afterwards.
- **Dedicated session = the timer's.** Its own state dir, `$XDG_STATE_HOME/pcb-lab-e01` (default `~/.local/state/pcb-lab-e01`), created by its own `pcb-lab login --state-dir ...` step. **Nothing else ever uses that state dir** (no probe, no seeder, no E03 run): a session shared with other probes would not measure the lifetime of a session refreshed only every 6 h. `pcb-lab probe alive --state-dir <that dir>` makes one authenticated `GET /core/v4/users` with the saved access token; if it is rejected (401) it refreshes once, saves the new tokens and repeats the GET. It appends `{at, ok, code, refreshed, rotated, expires_in}` to `e01.jsonl` in that dir. A refused refresh is itself the sample (`ok: false`, the refresh's Proton code, no retry of the GET): that is how the session ended, and its age is `at` minus the first line's `at`. After 14 days from the first sample it samples nothing and says so (the timer then only reports the window is over).
- `scripts/e01-timer/pcb-lab-e01.{service,timer}`: a user timer every 6 h (`OnCalendar=*-*-* 00/6:00:00`), `Persistent=true` so runs missed while the desktop slept catch up (ruling R4), running only `probe alive --state-dir %S/pcb-lab-e01`.

Mapping: question 1 from `ExpiresIn` of `probe refresh` and of the first `refreshed` samples of the long probe; 2 and 3 from `probe refresh`; 4 from the long probe.

Tested offline against the shared fake (`internal/lab/probe/session_test.go`, `cmd/pcb-lab/main_test.go`): rotation and old-token refusal, `ExpiresIn` parsing, no retry after a failed refresh, no token in recordings, `alive` refreshing only on a 401 then retrying the GET, a refused refresh recorded as the sample, the 14-day stop, the non-zero exit of a refused `probe refresh`, and the timer units' content.

## Results

The planned measurements below are still pending. One finding came first.

### Live finding (2026-11-12): a refresh needs the access token

Test account, times UTC, tokens redacted (T1, T2, T3: the pairs successive refreshes returned) [verified: live 2026-11-12]:

| Time | `POST /auth/v4/refresh` | Answer |
|---|---|---|
| 22:34:51 | the login's pair, body without `AccessToken` (go-proton-api `Manager.NewClientWithRefresh`) | 200, new pair T1 |
| 22:34:5x, 22:34:59 | T1's refresh token, body without `AccessToken` | 400, code 10013 "Invalid refresh token" |
| 22:36:00 | (`GET /core/v4/users` with T1's access token) | 200: the session was alive |
| 22:37:30 | T1's refresh token, body with T1's `AccessToken` | 200, new pair T2 |
| 22:40:46 | T2's refresh token, body without `AccessToken` | 400, 10013 |
| 22:41:02 | T2's refresh token, with `AccessToken` (16 s after the refusal) | 400, 10013 |
| 22:44:43 | T2's refresh token, with `AccessToken` | 200, new pair T3 |

`ExpiresIn` was 86400 on these answers.

Rule: **a refresh token obtained from a refresh is refused unless the refresh body carries that pair's current access token**; only the first refresh after a login worked without it. A refused attempt does not spend the refresh token, but is followed by a short cooldown (the 22:41:02 refusal); a client that always sends the access token never meets it.

go-proton-api's exported `Manager.NewClientWithRefresh` sends no access token (`manager_auth.go`); its client's own refresh after a 401 (`Client.authRefresh`) does. Consequences, fixed on 2026-11-12:

- pcb (`auth.Refresh`) used `NewClientWithRefresh`: every session's second refresh, about 48 h after login, was refused and read as "session ended" (needs-login every two days). It now sends the access token (SPEC §5.2).
- pcb-lab refreshed the saved session on every command (`session.Resume`) and in `probe refresh` / `probe alive` without the access token. `Resume` now uses the saved tokens, checked by one `GET /core/v4/users`, without refreshing; every refresh it makes carries the access token, and `probe refresh`'s retry with the previous refresh token carries the previous pair's access token.
- The shared fake (`protontest`) enforces the rule (400, 10013), so a regression fails the tests and `make rehearse`.

## Pending live run (HUMAN-CHECKS steps 2 and 3)

Two steps inside the sequence of `E03-client-identity.md` (build once, from a checkout of this repository: `go build -o ~/.local/bin/pcb-lab ./cmd/pcb-lab`).

**Immediate results, once, right after login #1 (disposable session) and its E03 probe.** Stop on a non-zero exit (refresh refused: the session is gone): stop the sequence and resume from the next step only after a fresh guarded login (E02 protocol):

**Not to be pasted as is: run through `scripts/live/run.sh` (`live`), as HUMAN-CHECKS step 2 (2a, the single `probe refresh`) and step 3 (the dedicated login and timer) spell it, with the run-root dirs and binary. The block below only shows the shape of the commands.**

```
pcb-lab probe refresh --record-dir spec/experiments/raw/E01 --json
```

It runs once on this session and never again; the next login in the sequence replaces it.

**Long probe, as its own login step** (the login of HUMAN-CHECKS step 3; the seeder's proton-cal login (step 4) follows it; at least 60 s after the previous login: the guard is per state dir, so the agent keeps the time of the last login by hand):

**Not to be pasted as is: run through `scripts/live/run.sh` (`live`), as HUMAN-CHECKS step 2 (2a, the single `probe refresh`) and step 3 (the dedicated login and timer) spell it, with the run-root dirs and binary. The block below only shows the shape of the commands.**

```
ENV='pass-cli run --env-file ~/.config/pcb-live/lab.env --'
$ENV pcb-lab login --state-dir ~/.local/state/pcb-lab-e01
pcb-lab probe alive --state-dir ~/.local/state/pcb-lab-e01      # first sample, checks the wiring
install -Dm644 scripts/e01-timer/pcb-lab-e01.service ~/.config/systemd/user/pcb-lab-e01.service
install -Dm644 scripts/e01-timer/pcb-lab-e01.timer   ~/.config/systemd/user/pcb-lab-e01.timer
systemctl --user daemon-reload
systemctl --user enable --now pcb-lab-e01.timer
systemctl --user list-timers pcb-lab-e01.timer
```

Uninstall (at the end of the 14 days; `alive` also prints it):

```
systemctl --user disable --now pcb-lab-e01.timer
rm ~/.config/systemd/user/pcb-lab-e01.{service,timer}
systemctl --user daemon-reload
```

Fill in here: `ExpiresIn`; refresh token rotated (yes/no); access token rotated; old refresh token accepted (yes/no, Proton code); then "long probe running since <date>", and after the window the first `ok:false` line (age of the session when it ended, code) or "survived 14 days of 6-hourly refresh". The `e01.jsonl` goes in `raw/E01/` as `alive.jsonl` (no secrets in it).

## Feeds

C1 refresh policy (M1): how often to refresh, whether to keep the old token, what an unattended re-login must expect (§5.2).
