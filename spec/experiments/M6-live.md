# M6-live — The upgrade of a real M5 state, and Thunderbird on two URLs

Status: **HUMAN-CHECKS step 8 run on 2026-11-14 by the agent** (8.2 to 8.4), test account only, no new Proton login. The author had reported step 8 run earlier that day without writing the results down; this run replaces that report with recorded results. Every check passed except one checklist line that was out of date (8.4.1, below). Feeds SPEC §4.1, §7.1, §9 and the Thunderbird auth-cache item of `OPEN.md`.

## Setup

- **Workstation variant**, as step 10 was (`M8-live.md`): no throwaway VM for pcb itself. Isolation came from the run root and `scripts/live/run.sh` (every `pcb` and `pcb-m5` command through it, its dir guard on), not from a VM. Nothing ran with the default dirs.
- **The M5 state was real**: the run root of the 2026-11-13 live sessions still held the test account's M5-layout state (root `state.json`, `secrets.sealed` sealed with the run root's key file, `clients.json`, `pcb.db`; phase `ready`, unattended relogin on, last login 2026-11-12 23:46Z), with its M5 build (`pcb-m5`, from before M6 in the old history). A cold archive was taken first (`m5-state.tgz`, never extracted or run).
- Build under test: `pcb` v0.1.0-4-ge8adf96 (main after the first release), in the run root's `bin/`.
- **Logins: none.** The session of 2026-11-12 was still alive (refreshed by pcb, `secrets_generation` 4); no CAPTCHA, no ledger entry.
- Thunderbird 153.7.0esr ran in the step 9 desktop VM (GNOME 48, Wayland), a fresh profile. An SSH reverse tunnel made the VM's `localhost:1080` the workstation's loopback listener, so Thunderbird used the URLs pcb prints. Thunderbird was driven through Marionette (chrome-context scripts): its own calendar code sent every request and raised every password prompt; a script filled the prompts from a 0600 file on the VM, ticked "Use Password Manager to remember this password" and logged each prompt. Marionette's recommended preferences were turned off (`remote.prefs.recommended = false`): they disable password saving, which would have hidden the result.
- All times UTC. The account's address, local ID and calendar IDs are not recorded here.

## 8.2 The upgrade: PASS

- 18:59:14Z, the M6 `pcb status` on the M5 dirs: exit 0. `layout.json` `{"version":2,"sealer":"keyfile"}`, `accounts/<local-id>/` with `state.json`, `secrets.sealed`, `clients.json`, `pcb.db`; root `sync.lock`; no `accounts/*.migrating`. The address was backfilled from the sealed session (the test account's, checked by eye); phase `ready`; `last_login` unchanged (2026-11-12 23:46:14Z): no login. `pcb account list`: one line.
- 8.2.2: `pcb client show --reveal` gives the same password as M5's (`cmp` of two 0600 files); `url` ends in `/dav/<local-id>/`, `username` is the address.
- 8.2.3: `pcb doctor` exit 0; the per-account results (sealed secrets, secrets, cache `schema 2, integrity ok`, caldav clients) are prefixed with the address; the one warning is the expected key-file-plus-unattended-relogin notice.
- 8.2.4: `pcb-m5 status` on the migrated dirs: `phase: logged-out`, `secrets: none`, exit 0, as designed (it finds no root `state.json`). `pcb-m5 login` was not run.

## 8.3 Thunderbird on the old URL and the per-account URL: PASS

- 8.3.1: `pcb serve --wait-for-login` on 127.0.0.1:1080: `Thunderbird URL (<address>): http://localhost:1080/dav/<local-id>/`; `pcb status` one account `serving`.
- 8.3.2 (curl, a client from `pcb client add`, 0600 netrc): PROPFIND Depth 0 on `/dav/calendars/me/` 207 and on `/dav/<local-id>/calendars/me/` 207; without credentials 401 with `Basic realm="pcb"` and `realm="pcb <local-id>"`; a wrong password 401; `/dav/00000000/calendars/me/` 401 with `realm="pcb 00000000"`.
- 8.3.3: there was no M5-era Thunderbird profile to reuse (the M5 serve was not run before the upgrade). In its place: two calendars on the old URL (`/dav/calendars/me/<calendar>/`, username `default`, the M5 password, which is what an M5 profile holds). One password prompt (19:11:14Z) served both; they synced 190 and 209 items, the same counts as the cache.
- 8.3.4: two calendars on the per-account URL (username the address), same profile: **their own prompt** (19:11:35Z), the same items. An event created through the per-account calendar (19:14:36Z, `pcb-m6 live check`) appeared through the old one after a refresh; its title edited through the old one reached the per-account one; deleted through the per-account one, it was gone from the old one. `pcb status`: one account.
- **8.3.5, the realm check (answers the `OPEN.md` item): Thunderbird keeps the two passwords apart.** After 8.3.4 its password manager held two entries for `http://localhost:1080`: realm `pcb` (user `default`) and realm `pcb <local-id>` (the address). At 19:12:06Z `pcb client rotate default`; on the next refresh one prompt (19:12:20Z, the old URL's realm, saved with the new password); Thunderbird itself dropped the per-account realm's stale entry, and the next refresh raised a second prompt for that realm (19:13:16Z). The new password entered at both, every calendar recovered (status 0 at the third refresh) with no further prompt. One realm's password was never sent for the other. Why: Thunderbird looks up and saves CalDAV credentials by origin **and** HTTP realm and username (`calAuthUtils.sys.mjs`, `#getPasswordInfo` and `passwordManagerSave`), so the distinct realms keep the entries apart by design. Two observations for the README: Thunderbird's prompt shows only `http://localhost is requesting your username and password` (no realm), with the username box empty even when the calendar has one; and the remember box is unticked by default.

## 8.4 Account commands: PASS (8.4.1's checklist line was out of date)

- 8.4.1: `pcb account caldav off`: both URLs 401 at once, `pcb status` marks the account `CalDAV off`. **Differs from the checklist**: `health.serve` stays `serving` and the loop keeps syncing. That is the design since M8: CalDAV off refuses CalDAV clients, and the sync pauses only when MCP is off too (`docs/commands.md`, SPEC §4.1, design `2026-11-13-mcp-design.md` §5.5b); MCP was `read-write`. The checklist line predates M8; it is corrected in `HUMAN-CHECKS.md`. `pcb account caldav on`: 207 on both within 5 s.
- 8.4.2: `pcb account restart`: exit 0, `restarted`, nothing visible changed.
- 8.4.3: `pcb sync`: exit 0, `pcb serve is running and already syncs: see pcb status`.
- 8.4.4: see "Cleanup" below.

## Cleanup

- 8.4.4 (20:17:35Z): `pcb account remove --yes --account <local-id>` on the running daemon: `removed`, exit 0; `accounts/` empty; `pcb status` `needs-login`; `pcb doctor` `no account`. The Proton session list check in the web app is the author's (asked for after the run). Then `pcb serve` stopped, the reverse tunnel closed, the Thunderbird profile deleted on the VM, and the run's 0600 password files and state archive deleted. The `pcb-m6` event had already been deleted at 8.3.4.

## Also measured on this state

E09's live half (cold start) and E11's live check ran on the same upgraded state, before 8.3: `E09-decrypt-cost.md` "Live half" and `E08-thunderbird-caldav.md` "Live check of VTIMEZONE (E11)". HUMAN-CHECKS 7.4.4 (Thunderbird idle cadence) ran on the same `pcb serve` and Thunderbird afterwards: `M5-live.md`, "7.4.4".
