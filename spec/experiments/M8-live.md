# M8-live — Agents over MCP (Claude Code) against the test account

Status: **HUMAN-CHECKS step 10 run on 2026-11-13/14 UTC** (10.1 to 10.7; the optional TLS check 10.5.5 was skipped), in the **workstation variant** (below). Every check passed; three defects found, all fixed, a fourth found while fixing the first, and one observation settled (table at the end, each fix named by its commit subject: the merge into main keeps the subjects, not the branch's hashes). Feeds SPEC §1, §12, §13, §15, §16, README "Agents (MCP)" and `spec/OPEN.md` "Parked by M8".

## Setup

- Build under test: main **34792ab** (the M8 merge). Client: **Claude Code 2.1.325**, the author's own installation and Claude login, run interactively in a terminal multiplexer and driven by the coordinating agent.
- **Deviations from HUMAN-CHECKS step 10** (all at the author's request):
  - **On the author's workstation, not the step 9 VM.** The run isolated itself instead: its own run root, with `PCB_CONFIG_DIR` and `PCB_STATE_DIR` set in the MCP server's `env` and in every terminal command; `[secrets] backend = "keyfile"` in the run root's `config.toml`, so the workstation's desktop keyring was never touched; the CalDAV listener on 127.0.0.1:1090, since another test daemon held 1080. HUMAN-CHECKS now describes this as the accepted "workstation variant", with its limits: no keyring sealing exercised by the run itself, the author's own Claude Code configuration (its plugins came along, see 10.3), no TLS check.
  - **The loopback login page was filled in with `curl`**, the password and the 2FA code piped from the password manager (never in argv, a file or the agent's prompt). The author solved the CAPTCHA.
  - **Each Proton write was checked through a second, independent session of the test account over CalDAV** (an existing checker daemon and a CalDAV client made for it, both removed in 10.7), not in the Proton web app.
  - The author checked what only a person can see: `pcb-ui`'s wizard (10.4.2) and the 10.6 dialog.
  - 10.1.2's question (does Claude Code keep the session bus) was settled on a throwaway state with `backend = auto`, deleted right after: **A**.
- All times are UTC. Everything written to the account was titled `pcb-m8 ...` and deleted in 10.3.
- **Logins: 2.** The first (10.2.2) met a CAPTCHA; its re-solve was the second, an extra login the author approved. The workstation's global ledger went from 14 to 15.

## What Claude Code declared

| | Result |
|---|---|
| URL elicitation | **yes**: the login page was offered as "wants to open a URL", with `Open in browser`, `I'm done, continue` and `Decline` |
| Form elicitation | **yes**: the delete confirmation, the exact question `Delete the event on 2026-11-15 10:00 Europe/Paris: «pcb-m8 renamed»?`, with Accept and Decline |
| The session bus for its MCP servers | **kept** (10.1.2 case A: a throwaway state with `backend = auto` got the layout `secret-service`) |

## 10.1 Register `pcb mcp`: PASS (a documentation nit)

- 10.1.1: offline, no account, no daemon.
- 10.1.2: registered with `claude mcp add --scope project` with the run root's `pcb` by absolute path and the two `PCB_*_DIR` in `-e`. `-e KEY=value` is variadic: the server's name must come **before** `-e`, or Claude Code reads the name as one more variable (HUMAN-CHECKS 10.1.2 now says so). Case **A** (above).

## 10.2 Login through the loopback page: PASS (M8-F1, M8-F2)

- 10.2.1: `pcb status` showed the daemon of kind `mcp`, no listener, `stdio 1`. The agent's `status` answered `now` with `"zone":"Local"`, not the IANA name of the workstation's zone (Europe/Paris) (**M8-F1**).
- 10.2.2: ledger slot 23:40:45Z. Claude Code offered the page by **URL elicitation** (above); the URL had the expected form. The page: the address read-only, a password field, the unattended-relogin box unticked, with the keyring sentence under it on a key-file state (F5 of the M7 run, fixed on the M7 branch after build 34792ab). The password was posted at 23:41:30Z: the CAPTCHA step. Re-solve slot 23:44:21Z (the extra login); the CAPTCHA continue at 23:44:23Z (200): the 2FA step; the code posted: "The account is added" at 23:44:32Z.
- Then `I'm done, continue` in Claude Code: the tool result was the offer's static answer (`where: page`, `expires_in_s: 300`, `cli_fallback`) although the login had ended on the page, which reads as a login still to do (**M8-F2**).
- 10.2.3: the agent's `status`: one account, `ready`; `pcb account list`: `ready`, MCP on; the layout `keyfile` (as configured), the key file in the run root's config directory.
- 10.2.4: the same URL again: connection closed (the login had ended). `login` again: `refused`, already logged in.
- 10.2.5: the nonce is nowhere under the state or config directory.

## 10.3 Create, update, delete: PASS (M8-F1)

Checked at Proton through the independent CalDAV session (23:46 to 23:52Z):
- 10.3.1: `calendars_list` then `event_create` (the agent passed `tz`): `DTSTART`/`DTEND` in Europe/Paris, 2026-11-15 10:00 to 11:00, `SUMMARY:pcb-m8 created`, a `VALARM` at `-PT10M`.
- 10.3.2: `event_update`: `SUMMARY:pcb-m8 renamed`, the times and the alarm unchanged. Its result said `"zone":"Local"`, and the agent remarked that the result says Local while the event was created in Europe/Paris (**M8-F1**: the result named only the zone wall clocks were read in, not the event's).
- 10.3.3: `events_list` for 2026-11-16 to 22: no event, which is right (the four recurring series of the default calendar have no occurrence that week).
- 10.3.4: `event_delete` asked by **form elicitation** (the question above). Decline: `refused`, "the deletion was not confirmed: nothing was deleted", the event still answered 200. Accept: `deleted: true`, scope `series`, then 404.
- 10.3.5: `/exit`: within a few seconds no `pcb mcp` process was left and `pcb status` was offline (`daemon: not running (offline view)`).
- Side effect: the client loaded the author's own plugins and MCP servers next to `pcb-live`. No effect on the checks; HUMAN-CHECKS' rules now say so.

## 10.4 Relayed to `pcb-ui`: PASS (F11)

- The first start of `pcb-ui` failed: 1080 was held by the checker daemon, so `pcb-ui` exited 20 with its startup dialog, and `pcb-ui.log` held only `control socket started` and `control socket stopped`, nothing about the listener (**F11**). With `listen = "127.0.0.1:1090"` it came up (23:54:02Z) as kind `ui`, serving, no new login.
- 10.4.1: `status` through the relay; `pcb status`: `relay 1`.
- 10.4.2: `login` with `pcb-m8-nobody@example.invalid`: the result `where: pcb-ui` with the CLI fallback and no URL. The author: the window came to the front on the wizard with that address filled in; Cancel; `pcb account list` still one account.
- 10.4.3: `events_list` for 2026-11-15: a monthly series of the dataset at 10:00 to 10:30, as expected. `pcb-ui.log`'s `mcp call` lines: `status`, `login`, `events_list`, each `entry=relay grant=stdio:full`, the account as its local ID, `outcome=ok`; no title, URL or address.
- 10.4.4: `/exit`: `relay 0`, `pcb-ui` kept serving; stopped with SIGTERM.

## 10.5 Remote `/mcp` with a token: PASS (O3; 10.5.5 skipped)

23:58 to 00:01Z.
- 10.5.1: `[mcp] remote = true`, `pcb serve --wait-for-login` (run as a user unit) on 127.0.0.1:1090, its log `mcp: remote on`.
- 10.5.2: the token `m8-live --allow read` created in the client's own shell (`pcbm_` prefix checked, never printed); `.mcp.json` with `pcb-remote`, `type: http` and `headers` from `${PCB_MCP_TOKEN}`: the expansion works.
- 10.5.3: Claude Code asked to approve the project server; `/mcp`: connected, authenticated, six tools, all read-only (List calendars, Get an event, List events, Search events, Free/busy, Status). `status` and the events (the same series on 2026-11-15) answered; `pcb status`: `remote 1`. `pcb mcp token list` showed LAST USED 23:58:34, the session's first call, while calls ran until 23:59:14 (**O3**: a use is recorded at most once a minute, and the 40 s sat within it; settled below).
- 10.5.4: wrong token 401, a foreign `Origin` 403, no header 401.
- 10.5.5 (TLS, optional): **skipped**: it would bind a LAN address on the workstation. `NODE_EXTRA_CA_CERTS` stays `[assumed]`.
- 10.5.6: the token removed at 00:00:39Z; the next call: Claude Code showed `MCP server "pcb-remote" rejected the Authorization header in its config (update it, then run /mcp to reconnect)` and marked it disconnected. Then the token unset, `.mcp.json` back to the stdio server, `[mcp]` removed, `pcb serve` stopped.

## 10.6 An agent session blocks the others: PASS

From 00:01Z: a stdio session (kind `mcp`); `pcb serve` exit 20, `pcb is already serving (pid N, kind mcp)` with the hint `end the agent session (pcb mcp), then try again`; `pcb-ui`: the dialog's text as written (checked by the author), exit 20. `/exit`: offline.

## 10.7 Cleanup: PASS

`pcb logout`: `Logged out.`, revoked. The account's session list (`pcb-lab probe`) holds no session created on 2026-11-13 any more (this run's and step 9's are gone). The `pcb-m8` event was already deleted (10.3.4). The checker daemon stopped, its CalDAV client and the temporary files removed, the terminal multiplexer ended, the run root removed, no `pcb-ui` or `pcb` process left.

## Findings and their fixes

| # | Finding | Fix | Commit (subject) |
|---|---|---|---|
| M8-F1 | `status` and the write results said `"zone":"Local"` (Go's name for the process's zone when `TZ` is unset); the update result named only the zone wall clocks were read in, which the agent took for the event's | the process's zone goes by its IANA name (`TZ`, the `/etc/localtime` link, `/etc/timezone`; a system that names none is labelled `UTC+hh:mm`); `event_create` and `event_update` also give `event_zone`, the zone the event keeps | "Name the process's zone by its IANA name in MCP results"; "Name the zone the event keeps in write results" |
| M8-F1b | found by the F1 work, not met live (the agent passed `tz`): on a host with `TZ` unset, wall-clock times given without `tz` were written in UTC, since a zone named `Local` has no TZID to write | new times carry the local zone's IANA name as their TZID (UTC only when the system names no zone) | "Name the process's zone by its IANA name in MCP results" |
| M8-F2 | after `I'm done, continue` the login tool returned the offer's static answer although the login had ended on the page | the accepted answer reads the page: `state: ready`, `state: waiting` (step, the last failure, the seconds left), or `refused` when the page closed without a login | "Answer an accepted login page with the login's state" |
| F11 | `pcb-ui` exited 20 on a port in use with nothing in its log about the listener | `daemon.Run` (`pcb serve`, `pcb-ui`, a `pcb mcp` session) logs at error a listener or control socket that cannot start, a TLS setup that fails and a listener that fails, with the address and the system's reason | "Log a listener that cannot start before the daemon stops" |
| O3 | LAST USED stayed at the session's first call while calls went on for 40 s | not a defect: SPEC records a use per request, at most once a minute; a test holds the rule at `/mcp`, and SPEC and `pcb mcp token list --help` say it lags the last call by up to a minute | "Say LAST USED follows a token's requests, once a minute" |
| — | HUMAN-CHECKS step 10's text (`-e` after the server's name; the client's own plugins and MCP servers; the workstation variant) | corrected | "Correct HUMAN-CHECKS step 10 after its live run" |

## What the next live run checks

10.5.5 (TLS with `NODE_EXTRA_CA_CERTS`); in the VM variant, a session daemon that seals with the keyring; the fixed results: `status` naming the IANA zone, an update result's `event_zone`, the `login` answer after `I'm done, continue` (`state: ready`), and the listener's error line in `pcb-ui.log` on a port in use.
