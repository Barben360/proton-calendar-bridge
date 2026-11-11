# E17 — go-webdav audit: fit, bugs, client compatibility

Status: **done for M3** (source audit plus a test client in CI). Outcome: **our own CalDAV server; go-webdav v0.7.0 is a test-only client** (ruling M3-R5). Feeds A1 (SPEC §9). Markers: `[verified: <test or command>]` = reproduced in this repository, `[source: file:line]` = read in the module or a third-party project, `[claim]` = a third party's own statement, `[inferred]` = ours. `GW/` = `go-webdav@v0.7.0` (tag, commit f93b8288, 2025-10-18, MIT). Third-party citations come from the M3 research notes (reference clones of caloxide, carbonate and cal-gateway, all MIT; not committed).

## Question

Can emersion/go-webdav serve the CalDAV surface Thunderbird needs (E08 MUST/SHOULD/AVOID), as is, wrapped, or not at all, and is it useful as an independent client to test our own server?

## Method

1. Read `GW/caldav/server.go`, `GW/internal/server.go`, `GW/internal/elements.go` and `GW/webdav.go` against the E08 list.
2. Read how carbonate, caloxide and cal-gateway use it and what they patch.
3. Run its `caldav.Client` against our server over real HTTP (`TestGoWebdavClientDiscoversAndReads`).

## Findings: the v0.7.0 server against E08

| E08 requirement | v0.7.0 server | Evidence |
|---|---|---|
| MUST 7 `sync-collection` (empty token, 404 tombstones, 4xx on a bad token) | **absent**: REPORT answers 400 for anything but `calendar-query` and `calendar-multiget`; master's sync-collection (PR 198, merged 2025-12-18) is client side only | [source: GW/caldav/server.go:90-102]; [source: gh api, PR 198 touches `caldav/caldav.go` and `caldav/client.go`] |
| MUST 5 `supported-report-set`, `CS:getctag`, `owner` | absent (unknown properties get a 404 propstat) | [source: GW/caldav/server.go:574 TODO; GW/internal/server.go:223-225] |
| MUST 5 / SHOULD accurate privileges | `current-user-privilege-set` hard-coded read+write in one `<privilege>` element (a fix is on unreleased master) | [source: GW/caldav/server.go:518-577; GW/internal/elements.go:457-467]; [source: gh api, commit 1901d193 2026-06-23] |
| MUST 13 iCalendar verbatim | **impossible**: `calendar-data` is always re-encoded from a parsed `ical.Calendar`; an encoder error becomes a 500 propstat, which makes Thunderbird fail the whole 100-href multiget (E08 §3.4) | [source: GW/caldav/server.go:615-623; GW/internal/server.go:212-219] |
| AVOID error text in bodies | multiget copies `err.Error()` into `responsedescription` | [source: GW/internal/elements.go:126-143] |
| Body limit | request bodies read without a bound | [source: GW/internal/server.go:31-45] |
| MUST 2, 6, 8, 14, whitespace-free leaves | fine (308 well-known behind auth, OPTIONS 204 with `DAV: 1, 3, calendar-access`, response-level 404 in multiget, `text/xml` accepted) | [source: GW/caldav/server.go:61-70, 317-340, 247-288; GW/internal/server.go:119-161] |
| PUT | always 201; preconditions left to the backend | [source: GW/caldav/server.go:668-712] (M4 concern) |
| Reuse | the XML element types live in `internal/`, so "our own handlers on its XML types" (earlier SPEC §9 wording) is not possible | [source: GW layout] |

What the three bridges did about it: carbonate rewrites responses with regexes, answers sync-collection by hand and fixes privileges; caloxide forks it (`CTag`, `Color`) and repairs iCalendar the strict encoder rejects; cal-gateway puts five interceptors and an 8 MiB body cap in front [claim, each project's docs; read in research]. In every case the project tests go-webdav's behaviour plus its own patches and still cannot serve verbatim bytes or choose status codes [inferred].

## Decision

Our own server (`internal/caldav`, about 1 200 lines with `encoding/xml` types we own, contract tests mapped one to one to E08: see E08 "What the bridge does (M3)"). go-webdav is kept for one job: an independent client in tests. Cost accepted in M3-R5: the lines are ours to maintain.

## What go-webdav does in this repository

- **Test client only, never in the binary** [verified: `go build -o $TMPDIR/pcb ./cmd/pcb` then `go version -m $TMPDIR/pcb` lists no `go-webdav` and no `go-ical` module among its 36 deps; the only `emersion` module in the binary is `go-vcard`, pulled by go-proton-api]. A source guard, `TestGoWebdavAndGoICalAreTestOnly` (`internal/caldav/arch_test.go`), fails when a non-test package imports either module, with a vacuity check that each one is seen in test imports [claim: Task 6 and review sabotage runs, go-webdav then go-ical imported in production, each turned it red; not re-run here].
- **What its client exercised**: `TestGoWebdavClientDiscoversAndReads` (`internal/caldav/webdav_client_test.go`, one fake-Proton rig, real HTTP, Basic auth): `FindCurrentUserPrincipal`, `FindCalendarHomeSet`, `FindCalendars` (name, description, `supported-calendar-component-set` = VEVENT), `MultiGetCalendar` over hrefs with a space, a slash and a non-ASCII character in the UID, `GetCalendarObject`; every object carries an ETag and the SUMMARY parsed by go-ical matches [verified: TestGoWebdavClientDiscoversAndReads]. An independent XML decoder agreeing with our encoder is the point; it found no defect in M3.
- **Gaps against E08**: it never sends Thunderbird's shapes (`Accept: text/xml`, `Accept-Charset`, `Depth: 1` plus `sync-level`), never runs `sync-collection` (v0.7.0 has no client call; master's is untested here), never sees a 401 challenge flow, a `/.well-known/caldav` redirect, a placeholder object, or a 403 `valid-sync-token`. It does not read `getctag` or `supported-report-set`. Those stay covered by the contract tests alone; the live check is the Thunderbird session (E08, `spec/HUMAN-CHECKS.md` step 6).
- **go-vcard bump**: a go-webdav import in the module graph makes minimal version selection pick `go-vcard v0.0.0-20230815062825-8fda7d206ec9` instead of `…20230331202150-f3d26859ccd3`, and go-vcard is linked into `pcb` through go-proton-api. Comparing the two module trees, only `README.md` and `example_test.go` differ [verified: `diff -rq` of the two module cache directories, 2026-11-11]; the suite passes. Accepted in the Task 6 review.
- **Licence**: MIT [source: GW/LICENSE]; credited in `README.md`, with go-ical (MIT) as its test dependency.

## Open

- Re-run the audit when master's server gains sync-collection and getctag (issues 35, 148, PRs 208, 213 open at the time of the research [source: gh api]); not a reason to change course while verbatim iCalendar is required.
- A go-webdav `SyncCollection` client call could join `TestGoWebdavClientDiscoversAndReads` once released.
