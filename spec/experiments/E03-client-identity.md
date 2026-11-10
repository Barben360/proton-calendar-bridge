# E03 — Client identity (`x-pm-appversion`)

Go/no-go gate of M0 (SPEC §17). Feeds C1 transport, decision 0011, SPEC §5.1.

## Question

Which `x-pm-appversion` values does Proton accept at login, and which of them yields calendar access (scopes and calendar endpoints)?

## Current conclusion

**Gate passed; scope list and other candidates measured at the live run.** On 2026-11-10 the author's fork of proton-cal, which sends `x-pm-appversion: Other`, logged into the author's real account (after a CAPTCHA), listed calendars and passed its full upstream live integration suite (read and write). `Other` therefore gets calendar access. Default identity: `Other` (decision 0011, provisional until the live run below). That evidence comes from the author's account, not the test account; the lab never touches the author's account (decision 0005).

## Candidates (collected 2026-11-11, read only; at most 4)

| # | Value | Source | Reported outcome |
|---|---|---|---|
| 1 | `Other` | proton-cal `docs/crypto.md:62` (also caloxide `cmd/caloxide/main.go:46`, `FORK.md:94`) | accepted; calendar API works (proton-cal live suite; caloxide, tested 2026-10-29) |
| 2 | `linux-mail@1.0.0` | carbonate `internal/proton/client.go:55` (table of tried values at `:41-:46`; `docs/ARCHITECTURE.md:333`) | accepted at login, version number not checked; `linux-calendar@5.0.0` gave 5002, `linux-bridge@3.0.0` gave 8004. Not truthful (claims to be Proton Mail on Linux). Calendar access not stated |
| 3 | `web-calendar@5.0.131.2` | client id `web-calendar` from ProtonMail/WebClients `packages/shared/lib/constants.ts:176` (version string read only, no code copied); version from the official web client's iCalendar PRODID in carbonate `internal/calendar/calendar_test.go:16` | unmeasured. WebClients injects the real version at build time, so this exact number is a plausible recent one, not the current one. Official identity (decision 0008 fallback) |
| 4 | `external-drive-caloxide@0.2.1-stable` | caloxide `FORK.md:96` (reproduction value at `:106`) | login works, every calendar endpoint answers 9100 (insufficient scope). Negative control |

Dropped: `external-calendar-<name>@x.y.z` (caloxide `FORK.md:95`: 5002 at login), `linux-calendar@5.0.0` and `linux-bridge@3.0.0` (carbonate, refused).

## Method

`pcb-lab probe calendar-access` resumes the saved session with the candidate as `x-pm-appversion` (`PCB_LAB_APPVERSION`) and makes three GETs: `/calendar/v1`, `/calendar/v1/{first}/bootstrap`, `/settings/calendar`. It prints and records, for each, HTTP status and Proton code (a skipped bootstrap is reported when the list yields no calendar). The login itself reports the scope list (`pcb-lab login`, `pcb-lab whoami`). A candidate gets calendar access when all three answer 200 / 1000. Recordings: `spec/experiments/raw/E03/<candidate>-<step>.json`, redacted by `internal/lab/redact`.

Tested offline against the shared fake (`internal/lab/labtest`): `internal/lab/probe/probe_test.go`, `cmd/pcb-lab/main_test.go`.

## Results

Existing evidence only (above). No live measurement on the test account yet.

## Pending live run (HUMAN-CHECKS step 1)

Blocked on the test account's CAPTCHA (first login 2026-11-10 23:47 UTC answered Proton 9001; `hv.json` holds the challenge). After the author solves it, from `/home/jduff/Workspace/worktrees/proton-calendar-bridge-m0` with `pcb-lab` built, in this order. Every login is at least 60 s after the previous one (the guard refuses sooner with exit 20); probes use the saved session and are not logins.

```
ENV='pass-cli run --env-file scripts/lab.env.example --'
REC='--record-dir spec/experiments/raw/E03'

# 1. Other, with the solved token (E02 data point, see E02)
$ENV pcb-lab login --captcha-token <token from hv.json>
pcb-lab whoami --json                                    # scope list for Other
PCB_LAB_APPVERSION=Other pcb-lab probe calendar-access $REC
# (E01 immediate probe runs here, see E01: pcb-lab probe refresh)

# 2-4. other candidates, each >= 60 s after the previous login
PCB_LAB_APPVERSION=linux-mail@1.0.0 $ENV pcb-lab login
PCB_LAB_APPVERSION=linux-mail@1.0.0 pcb-lab probe calendar-access $REC
PCB_LAB_APPVERSION=web-calendar@5.0.131.2 $ENV pcb-lab login
PCB_LAB_APPVERSION=web-calendar@5.0.131.2 pcb-lab probe calendar-access $REC
PCB_LAB_APPVERSION=external-drive-caloxide@0.2.1-stable $ENV pcb-lab login
PCB_LAB_APPVERSION=external-drive-caloxide@0.2.1-stable pcb-lab probe calendar-access $REC

# 5. back on the default for E01 and the seeder, >= 60 s later
$ENV pcb-lab login
```

If a login ends in a CAPTCHA (exit 10), stop there, record it for E02 (`hv-log.jsonl`) and follow the E02 protocol; the remaining candidates wait. Each login passes `PCB_LAB_APPVERSION` in the environment, so the candidate is also the identity of the session it creates.

Write the results table (status / code per endpoint per candidate, scope list) here, then:
- if `Other` fails on the test account while the author's evidence says it works: investigate before anything else (account state, not identity);
- if no candidate gets calendar access: stop M0 and report (gate);
- otherwise set decision 0011 to final (or revise it) and turn SPEC §5.1 to `[verified: E03]`.

## Feeds

C1 transport (default identity, configuration key), decision 0011, SPEC §5.1; E01 and the seeder reuse the `Other` session.
