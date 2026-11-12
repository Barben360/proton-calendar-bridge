# E12 — Recurrence forms Proton refuses or alters

Status: **source-based; the M4 write path gatekeeps on it; live run pending** (`spec/HUMAN-CHECKS.md` §6, steps W8, W9, 6.3 B2–B4 and D5–D10). Marks as in E10. Evidence: `raw/m4-research.md` §E ("research E"), §A.4. Feeds SPEC §8.4, `spec/mapping.md` §4 and §7, rulings M4-R5, M4-R10, M4-R12.

## Question

1. Which RRULE forms does Proton store and show correctly, and which does it refuse or alter? Does the server enforce the web client's limits, or only the client (research E.1 [conflict])?
2. Must DTSTART be the rule's first occurrence?
3. Are the bridge's rewrites shown as the same series in the web app: `FREQ=MONTHLY;BYDAY=2TU` → `BYDAY=TU;BYSETPOS=2` (R5), `FREQ=DAILY;BYDAY=MO,…,FR` → `FREQ=WEEKLY;BYDAY=…` (R12)?
4. Exceptions: an occurrence edited (a new row with RECURRENCE-ID), an occurrence deleted (EXDATE, plus the delete of its exception row), and several row operations in one sync call (mixed batches).
5. What Thunderbird sends for "every weekday", "the second Tuesday of the month" and "this and all future occurrences".

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| Limits: FREQ DAILY, WEEKLY, MONTHLY, YEARLY; COUNT ≤ 49; UNTIL ≤ 2037-12-31; not both COUNT and UNTIL; MONTHLY one BYDAY with BYSETPOS ∈ {-1,1,2,3,4} or one BYMONTHDAY; YEARLY BYMONTHDAY needs BYMONTH; DTSTART is the first occurrence | proton-cal `docs/api.md:386-392` ("server-enforced", mirroring the web client) | [claim: proton-cal] |
| The server validates nothing: an accepted rule can break the web app; INTERVAL ≤ 999 daily, 4999 weekly, 99 yearly | cal-gateway `docs/FEATURE-MATRIX.md:9-14,34-44,214-217` | [claim: cal-gateway] |
| RDATE, EXRULE, RANGE=THISANDFUTURE have no Proton equivalent | cal-gateway `FEATURE-MATRIX.md:68` | [claim: cal-gateway] |
| An exception is its own row (same UID, RECURRENCE-ID, no RRULE or EXDATE, SEQUENCE ≥ master's, else 2001); RECURRENCE-ID with RRULE is 2011 | research E.2, A.4 | [claim: proton-cal live; carbonate live] |
| Deleting a master orphans its exceptions: a series delete batches every row of the UID in one call | research E.2 | [claim: proton-cal live; cal-gateway live] |
| Whether create, update and delete entries may be mixed in one call, and whether a failed entry rolls back the others | research A.5 | unknown |
| Thunderbird writes MONTHLY `BYDAY=<n><DAY>`; "this and future" splits the series under a new UID rather than sending RANGE | research E.3 | [inferred] |

## What the code assumes

`internal/ical/validate.go` (`checkRRule`, `monthlyOrdinal`, `dailyByday`) and `internal/service/writeplan.go` (`Plan`); the table is `spec/mapping.md` §4.
- The bridge refuses, before any Proton call, every rule outside the allowed set per FREQ (ruling M4-R10), RDATE, EXRULE and RANGE (403 `pcb:unsupported-recurrence`, `pcb:rdate`, `pcb:exrule`, `pcb:recurrence-range`), so the server's own enforcement never matters for what pcb sends. [verified: `TestRRuleLimits`, `TestRefusals`]
- Rewrites with the same meaning: R5 and R12. [verified: `TestMonthlyOrdinalBydayIsRewritten`, `TestDailyBydayIsRewritten`]
- DTSTART being the first occurrence is **not** checked.
- One sync call per PUT, master first, then the exceptions (updates and creates), then deletes; an exception row absent from the body is deleted; an exception's SEQUENCE is at least the master's; deleting one occurrence is the master's EXDATE (in both signed parts) plus the delete of its exception row. [verified: `TestDeleteOneOccurrenceOfASeries`, `TestMasterEditMirrorsExceptions`, `TestExceptionCreateHasSequenceAtLeastMaster`, `TestPatchExdateInBothSignedParts`]
- After a refused or partly applied call, the rows are read back: what Proton holds is cached and the answer is 412 for a concurrent change, else 403 `pcb:proton-refused` (M4-R34). [verified: `TestOccurrenceDeleteRaceIsTruthful`, `TestEntryRefusalIsReadBack`]

The fake Proton accepts mixed batches; with its `AllOrNothing` switch it refuses a whole call when one entry fails (both modes are tested).

## Live run that settles it

`spec/HUMAN-CHECKS.md` §6:
1. **W8** (agent, curl): creates `pcb-m4-weekdays` (R12), `pcb-m4-2tu` (R5), `pcb-m4-lastfr` (BYSETPOS -1), `pcb-m4-notfirst` (DTSTART on a Wednesday, `FREQ=WEEKLY;BYDAY=MO`), `pcb-m4-count49`, `pcb-m4-until` (`UNTIL=20371231`), and checks the bridge's refusals (COUNT 50, HOURLY, BYSETPOS 5, RDATE, floating, unknown TZID). Server-side enforcement of the limits is not reachable through pcb: record "not tested (the bridge gatekeeps)".
2. **W9** (agent, curl): a daily series; an exception (one create), an EXDATE (two updates: the exception's SEQUENCE is raised to the master's), an occurrence deleted with its exception row (update + delete), then update + create + delete in one call.
3. **6.3 B2–B4** (author): the web app and Thunderbird show each series as written: occurrences, days, the moved and missing occurrences; for `pcb-m4-notfirst`, which days each app shows.
4. **6.3 D5–D10** (author in Thunderbird, agent reads what was stored): every weekday, the second Tuesday, one occurrence moved, one deleted, "this and all future".

Results: not run yet. Fill:

| Form | pcb answer | Stored RRULE | Web app shows | Thunderbird shows |
|---|---|---|---|---|

and one line each for the batches of W9 and the "this and future" shape.

## Decision it feeds

- A rewrite the web app shows differently: drop it and refuse that form instead (R5 or R12 reversed).
- Proton refusing DTSTART off the rule: add a refusal (or move DTSTART to the first occurrence); accepted but shown differently by the two apps: refuse it (cal-gateway's "honest 403" rule).
- Mixed batches refused or all-or-nothing: the planner must split calls or order them (today: one call per PUT, by design, so a failure leaves no half-written series).
- "This and future" sent as RANGE: it stays refused (403), and README says so.
