# M5-live — The daemon, the service and the container against the test account

Status: **HUMAN-CHECKS step 7 run on 2026-11-13** (7.1 to 7.7 and 7.9; 7.8 was done on 2026-11-12). Three defects found and fixed (Fixes 7, 8 and 9 below). Not exercised live: Thunderbird (7.1.7, 7.2.5, 7.7.2, 7.4.4), the 2FA-held login path (7.1.5). Feeds SPEC §7.3, §8.1, §13 and E15.

## Setup

- A throwaway Debian 13 VM, with its own run root, the throwaway marker and the guard of `live --vm`. User-scope systemd service installed by `pcb install` (7.1), then the container image on the same VM (7.2). Build under test: 49f2007 for 7.1.1 to 7.1.5, then 69f9539 (7.2), bc0f196 (7.3 and 7.4 and the first cut of 7.5), ede976e (second cut of 7.5, 7.6, 7.7).
- Hybrid mode: the author typed every login (username, password, CAPTCHA, 2FA code) at the VM's terminal; the agent ran everything else. `sudo` lines were typed by the author.
- curl stands in for Thunderbird at 7.1.7, 7.2.5 and 7.7 (a client created with `pcb client add`, a 0600 netrc file). The Thunderbird halves of those steps were not run.
- All times are UTC. Everything written to the account is titled `pcb-m5 ...` and was deleted at 7.9.
- Logins on the VM: 4 (the budget is 3). 7.1 took two, because its first login hit a CAPTCHA and was resumed (7.1.5); 7.2 took one; the re-login of 7.3 was the fourth, approved by the author before it ran (`--extra-login`).

## 7.1 A fresh Debian host, from source: PASS (one defect, Fix 7)

| Step | Result |
|---|---|
| 7.1.1 build and install | ok |
| 7.1.2 `pcb install --plan` then `pcb install` | both exit 0; `ExecStart` is the run root's `bin/pcb`. Nit: the guard banner prints the run-root directories, then unsets them under `--vm` |
| 7.1.3 linger | typed by the author with sudo; `active_state` active, `unit_file_state` enabled |
| 7.1.4 before the login | `pcb status` exit 0, daemon waiting for a login; `pcb doctor` exit 0 with `!` on needs-login and on "no client" |
| 7.1.5 login | see below |
| 7.1.6 after the login | on the restarted daemon: serving; `client show` ok; `pcb sync` exit 0, "already syncs"; the write gate reads "unknown (keys not loaded yet)" until the first CalDAV read, then open |
| 7.1.7 an event edit (curl) | create 201, rename (If-Match) 204, read back; no `refusing a write` and no `not confirmed` in the log. Thunderbird not run |
| 7.1.8 reboot | `sudo systemctl reboot` shut the VM off instead of restarting it (a quirk of how the VM was created); started from the hypervisor at 14:16:27. Serving again at 14:16:33, no login, PROPFIND 207. Thunderbird's refresh without a prompt not run |
| 7.1.9 | `systemd-analyze --user security`: 6.5 MEDIUM. `pcb doctor` exit 0, secrets backend a key file, with the same-volume warning (the key sits in the config directory) |

**7.1.5.** At 14:02:52 `pcb login --non-interactive` stopped on a CAPTCHA (exit 10, ledger row 1). The author tried `resume --totp` (answered that no 2FA code was pending, as designed), then resumed interactively with `pcb login resume --captcha-token` outside `live`: logged in at 14:04:35 (ledger row 2, added by hand). The 2FA-held path (7.1.5's main line) was **not exercised**. HUMAN-CHECKS 7.1.5 had no instructions for resuming interactively after a CAPTCHA.

**Defect, Fix 7 (the `--wait-for-login` handover stall).** On build 49f2007 the first pass after the login failed at 14:06:12 with "cancelled or timed out"; a second failed at 14:09:27, about 167 s later, and no pass succeeded until the daemon was restarted (the restarted daemon then added 403 events at once). A goroutine dump showed a reused HTTP/2 connection to Proton that no longer answered. Fix: drop idle connections after a request got no answer, before the retry or the next pass, in the sync loop and in the login manager; the Proton API client has its own transport with an HTTP/2 health check (15 s ping, 15 s timeout). Commits 42cb6b1, bf00511, d879e6c, 1c4c161, 3b18d40, 69f9539 and bc0f196 (the last adds a black-hole proxy in the fake that stops answering; the health check is tested against it). Verified live at 7.2.3 (build 69f9539, close-idle fix only) and 7.3.3 (build bc0f196, with the health check): see there.

## 7.2 The container: PASS

| Step | Result |
|---|---|
| 7.2.1 | image built. `pcb doctor` with the key file owned by the VM user: exit 10, `key file ... not readable by this user (uid 65532)` with the fix line, as expected. The key was then chowned through a busybox container, no sudo |
| 7.2.2 | healthy in 32 s while logged out; `status` waiting for a login; `key_file` is `/run/secrets/pcb_key` (the compose secret). The VM's user service was stopped meanwhile (port 1080) |
| 7.2.3 | login at 16:57:03 (ledger row 3). The CAPTCHA was solved in the process ("Press Enter once solved"), then the 2FA code: exit 0, no resume, no extra login. The daemon had been waiting; client `default` created at 16:58:39, first pass at 16:59:17 (404 events added, 38 s), then every 30 s with no timeout: the handover works on 69f9539. `status`: serving, `key_file: /run/secrets/pcb_key`, write gate "unknown" until the first CalDAV read. `pcb doctor` exit 0, `! secrets: secrets sealed with a key file (headless fallback)`, **no same-volume warning**, `service: no systemd here` |
| 7.2.4 | the fingerprint of `client show` equals the one `openssl s_client` prints |
| 7.2.5 (curl, client `m5-curl-ctr`, TLS verified against the fingerprinted certificate) | create `pcb-m5 container` 201, rename (If-Match) 204, read back. Thunderbird and its certificate dialog not run |
| 7.2.6 | `restart`: healthy in 31 s; `down` then `up -d`: healthy in 30 s, volume kept; passes resume, CalDAV 207, no login. Bind mount `./data` owned by 65532, mode 0700: starts, waits for a login, healthy. Root-owned `./data`: exit 20, `cannot take the sync lock` with the hint `make sure /data/state is writable by this user` |
| 7.2.7 | `docker exec pcb pcb logout`: `Logged out.`, exit 0 (the revoke reached Proton: see 7.3.1). `down -v`, `rmi`, compose directory removed |

## 7.3 A session ended by Proton: PASS (Fix 7 verified live)

- **7.3.1** (read-only, no login): the account listed 10 sessions. The VM's session from 7.1 was identified by its creation time (14:04:28) and its client. The container session of 16:58 was **absent**: the logout of 7.2.7 did revoke at Proton.
- **7.3.2** The author revoked the VM's session at about 17:12. The session was gone from the list. The daemon logged `sync pass failed ... the Proton session has ended` at 17:12:36 and went to waiting; `status` showed needs-login with the alert `error needs-login: the Proton session has ended (fix: run pcb login)`; the calendar home answered 503 with `Retry-After: 30`, without credentials 401; the daemon was not restarted. The VM's user service had been redeployed at 17:05:49 on bc0f196.
- **7.3.3** Login (ledger row 4, the extra one) at 17:18:36, the CAPTCHA again in the process, exit 0, `last_login` 17:19:53. Same pid, serving at 17:19:55, first pass at 17:19:56 (1 s), then every 30 s, `last_pass.ok` true, no alert, CalDAV 207, the `default` client's password hash unchanged. This is the same handover path as 7.1.5 and it worked on bc0f196: **Fix 7 verified live**.
- **7.3.4** What Proton answered to the refused refresh was **not recorded**: bc0f196 does not log it. That is Fix 8 below.

**Observation O1.** Between "Press Enter once solved" and the 2FA prompt about 10 s passed, at 7.2.3 (69f9539) and again at 7.3.3 (bc0f196). The CLI adds no delay (it logs in with the token at once); API retries (429 with `Retry-After`, back-off of 1, 2, 4 s) are not logged, so the cause is unknown: Proton's latency or silent retries. Not a defect. Candidates: print a progress line after Enter, debug-log the API retries.

**Fix 8 (Proton's answer is logged when it ends the session).** f0ccbef logs `proton_status` and `proton_code` on the `sync pass failed` line when Proton refused the refresh; d1f3b9f words HUMAN-CHECKS 7.3.4 to look at the first such line; 72fd101 pins the 401 after a refresh and makes the secret check bite per case. Tested against the fake only; not run live (no revoke after it).

## 7.4 Idle: PASS (7.4.4 skipped)

- Last curl at 17:21. Idle at 17:56:33: `idle` true, `interval_seconds` 300. Passes at 17:56:33 and 18:01:34 (301 s apart).
- Wake: one authenticated curl at 18:04:21.689; the pass started at 18:04:22.720, `idle` false, `interval_seconds` 30.
- **7.4.4 skipped** (author's decision; hybrid mode, the daemon sat on the VM's loopback, Thunderbird was not run). Whether Thunderbird at default settings keeps the daemon active (E08 section 3.7: its 30-minute refresh is under the 35 minutes) stays the expectation, not a live result.

## 7.5 `status` and `doctor` as the author reads them, one alert: PASS after Fix 9

- Read-me output of `status` and `doctor` was produced for the author on the VM (healthy, during the cut, after) and for the container from 7.2. The author read them all, found them understandable and asked to keep the wording as it is (the proposed `last pass` line in `pcb status` and a wording change of the write gate were declined). The `calendar-keys-failed` alert was not provoked (nothing recorded for it).
- **First cut, build bc0f196**: outgoing 443 rejected 18:04:54 to 18:08:35. Passes failed at 18:05:26 (next in 31 s), 18:06:00 (56 s), 18:06:59 (2m6s) with "cannot reach Proton"; the good pass came at 18:09:07, scheduled, no early retry. **No `sync-stalled` alert at 18:08:34** (3m41s without a sync) and `pcb status` showed no failure line. Curl on the calendar home answered 207 from the cache each minute.
- **Defect, Fix 9.** The threshold was 5 times `health.interval_seconds`, which during failures is the back-off wait (31, 56, 126 s ... up to 10 minutes), so the alert receded as the outage went on (about 50 minutes before it would show). Fix: `health.cadence_seconds` carries the cadence of the pass, and `sync-stalled` is measured against it. Commits c60503d and ede976e (the cadence stays that of the last pass until a woken pass ends; an older idle daemon is read as 300 s; a test for `SetInterval`). `pcb sync --watch` raises no alerts, and the SPEC says so.
- **Second cut, build ede976e**: rejected 18:24:37 to 18:28:38; last good pass 18:24:22. Failures at 18:25:09 (next in 28 s), 18:25:41 (58 s), 18:26:41 (2m0s); `cadence_seconds` stayed 30 throughout. At 18:26:37 no alert (threshold 18:26:52); at 18:27:07 `sync-stalled` was present for each calendar: `no successful sync of <calendar> for 4m16s (fix: see last_pass and the logs (pcb service logs))`. Good pass at 18:28:42; the alerts were gone at 18:29:08. **Fix 9 verified live.**

## 7.6 `uninstall --delete-data`: PASS

On ede976e. Before: the VM's session from the re-login (created 17:19:46) was listed. `--plan`: exit 0, lists disable `--now`, remove the unit, `daemon-reload`, logout (best effort) and both directories; nothing changed. `--yes`: exit 0 at 18:32:04; both directories and the unit gone, both binaries stay, the service inactive. After: no session created on 2026-11-13 listed, so the session was revoked. 7.6.4: `pcb service status` exit 10, `pcb is not installed as a service`, hint `pcb install`; `pcb status` exit 0, logged out, alert needs-login.

Two nits, for the maintainers (code changes, not made here): the apply prints the plan lines only and no revoke confirmation (HUMAN-CHECKS 7.6.2 now says to check the session list); `pcb status` after `--delete-data` still prints the configured `key_file` path, which no longer exists.

## 7.7 `client show --reveal`, `client rotate`: PASS (curl instead of Thunderbird)

- `client show --json --reveal`: a password, `fingerprint` null (plain HTTP on the loopback). Without `--reveal`: `password` null.
- The `default` client used by curl at 18:23:20: `last_used` set.
- `client rotate default`: exit 0, `Client "default" has a new password` and `Hint: update the password in your CalDAV client`. Old password: 401 (root and calendar home); new: 207. Same pid, no restart. The two netrc files were removed.
- 7.7.2 (Thunderbird prompting for the new password) not exercised.

## 7.8

Done on 2026-11-12 (see HUMAN-CHECKS 7.8); not part of this session.

## 7.9 Clean-up

- The author searched the web app for `pcb-m5` and found no event, probably because Proton Calendar's search only covers events once its index is built. The two events were deleted over CalDAV at 19:45 from the workstation, with a throwaway client (`m5-cleanup`) and `If-Match`: `pcb-m5 vm renamed` (UID `pcb-m5-vm`) and `pcb-m5 container renamed` (UID `pcb-m5-container`), 204 and 204. A GET on each then answered 404, and a fresh sync showed neither. The client was removed and the serve stopped.
- The VM was destroyed at about 19:52 with its disk, at the author's request. A re-test needs a new one.
- A holidays calendar that Proton added to the account on its own (after the author deleted a calendar) was exercised by chance: type `holidays`, write gate closed ("not a normal calendar (subscribed or holidays)"). Its event count after the first sync was not recorded.

## What this changes

- SPEC §7.3 (sealed session in a service and a container), §13 (real systemd, linger across a reboot, a real login) and the E15 compose-secret note: verified live.
- SPEC §8.1 (idle and wake): verified by curl; Thunderbird's own behaviour at default settings stays assumed (7.4.4 skipped).
- Fixes 7, 8, 9 as above. Open: O1; the two 7.6 nits.
