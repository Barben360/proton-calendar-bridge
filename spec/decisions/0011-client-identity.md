# 0011 — Client identity: Other by default

- **Date**: 2026-11-11
- **Decided by**: agent (controller ruling R13), author to confirm
- **Status**: provisional until the E03 live run

The bridge sends `x-pm-appversion: Other` by default. It is truthful (it claims to be no official Proton client), and on 2026-11-10 the author's fork of proton-cal, which sends exactly that value, logged into the author's real account (after a CAPTCHA), listed calendars and passed its full upstream live integration suite, read and write. This passes the M0 go/no-go gate (E03).

The value stays configuration, not code. If the E03 live run (`spec/experiments/E03-client-identity.md`) shows `Other` losing a scope the bridge needs, this decision is revised: another truthful value if one works, else the official identity per 0008. The scope list and the other candidates are measured at that run.
