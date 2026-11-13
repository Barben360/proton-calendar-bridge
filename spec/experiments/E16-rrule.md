# E16 — RRULE library audit vs our own expander

Status: **done (M8 Task 2)**. Outcome: **our own expander over Proton's recurrence set; `github.com/teambition/rrule-go` v1.8.2 is not adopted**. Feeds `ical.Expand` (M8 Task 3), SPEC §6 ("Recurrence") and §17. Markers as in E17: `[verified: <test>]` = reproduced here, in a scratch module that is not committed (its cases are listed below so Task 3 can port them); `[source: file:line]` = read in the module; `[claim]` = a third party's statement; `[inferred]` = ours. `RG/` = `rrule-go@v1.8.2` (tag 2023-01-13, commit 429348ca, MIT).

## Question

Does rrule-go, already in the module graph as go-ical's dependency, expand every RRULE Proton accepts and every RFC 5545 example correctly, with occurrences across a DST change at the wall-clock time RFC 5545 §3.3.5 gives? If not, `ical.Expand` gets its own expander, limited to Proton's set.

Decision rule (plan, Task 2): if every Proton form and every RFC example matches **and** DST occurrences keep their wall-clock time, adopt the library. Otherwise write our own over Proton's set only.

## Method

1. Read `RG/` (licence, `rrule.go` iteration, `rruleset.go`, `str.go` parsing) and its GitHub issues and pull requests (`gh api repos/teambition/rrule-go/issues?state=all`, 77 entries, 2026-11-13).
2. Scratch module on `rrule-go v1.8.2`, Go 1.27.3, zone data from the system:
   - `TestRFC5545Examples`: every example of RFC 5545 §3.8.5.3, results as printed, compared as wall clocks in `America/New_York` and as instants;
   - `TestProtonForms`: each rule shape Proton accepts (E12 table, `checkRRule` in `internal/ical/validate.go`, ruling M4-R10), in `Europe/Paris`;
   - `TestDSTLibraryZoned`, `TestProtonDST`, `TestGoTimeDate`: gaps and overlaps in Paris, New York and Sydney;
   - `TestParsingEdges`, `TestHugeMonthlyInterval`, `TestCost`.

## Findings: the library

| Point | Finding | Evidence |
|---|---|---|
| Licence | MIT, © 2017-2023 Teambition | [source: RG/LICENSE] |
| Maintenance | last release v1.8.2 (2023-01-13), last push 2024-08-15; a fork's author says the original author confirmed it is unmaintained (issue 72) | [source: gh api]; [claim: issue 72] |
| Size | about 1 800 lines without tests, all frequencies and BY parts (a dateutil port) | [source: `wc -l RG/*.go`] |
| Binary | today it never reaches `pcb`: only `internal/caldav`'s tests pull it, through go-ical | [verified: `go list -deps ./cmd/pcb` has no rrule-go or go-ical; `go mod why -m github.com/teambition/rrule-go`] |
| DST (issues 59, 63; PR 66 open) | duplicates or wrong times on HOURLY across a change | [claim: issues 59, 63]; HOURLY is outside Proton's set |
| UNTIL in local time (issue 67; PR 68 open) | `StrToRRule` reads a local UNTIL as UTC; `StrSliceToRRuleSetInLoc` with a `DTSTART;TZID` line reads it in that zone | [verified: `TestParsingEdges`: until = `2026-01-07 10:00 +0000 UTC`]; [source: RG/str.go:307-360] |
| Panics (issue 72; PRs 74-77 open) | BYDAY ordinals outside the period, BYEASTER offsets, INTERVAL=0 accepted | [claim: issue 72]; [verified: `INTERVAL=0` expands as 1]; none of these shapes is in Proton's set |
| BYSETPOS | correct on every case below | [verified: RFC 31, 32; P13-P18, P30] |
| COUNT with EXDATE | COUNT is counted on the rule, EXDATE removes afterwards, as RFC 5545 §3.8.5.3 says | [verified: P25: `COUNT=5` minus one EXDATE gives 4]; [source: RG/rruleset.go:134-177] |
| EXDATE matching | by instant (`time.Equal`), so a UTC EXDATE removes a TZID occurrence | [verified: P26] |
| How it builds times | every occurrence is `time.Date(y, m, d, h, mi, s, 0, DTSTART's location)` | [source: RG/rrule.go:637-639, 672-674] |

### RFC 5545 §3.8.5.3 examples

All 39 examples (42 rules, counting the alternative forms of "every day in January", "Tuesday and Thursday for five weeks" and "every 20 minutes") match as printed, wall clocks and instants [verified: `TestRFC5545Examples`, 42/42], with one note:

- "Every 3 hours from 9:00 AM to 5:00 PM" prints `UNTIL=19970902T170000Z` but lists 15:00; 17:00Z is 13:00 EDT. Verified erratum **3883** (2014-02-14) corrects it to `UNTIL=19970902T210000Z`. The library gives 09:00, 12:00, 15:00 with the corrected UNTIL and 09:00, 12:00 with the printed one, both right [verified: `TestRFC5545Examples/33`, `TestRFC5545Example33Corrected`].

### Proton's accepted forms

All 30 cases give the expected set [verified: `TestProtonForms`, 30/30]: plain DAILY, `COUNT=49`, `INTERVAL=999`, UNTIL in UTC inclusive; WEEKLY `BYDAY=MO,TU,WE,TH,FR` (R12's target), `INTERVAL=2` with WKST SU and MO, `INTERVAL=4999`; MONTHLY plain, `BYMONTHDAY=31` (months without a 31st skipped, RFC example 39's rule), `BYMONTHDAY=-1`, `BYDAY=<day>;BYSETPOS=` 1, 2 (R5's target), 3, 4, -1, with `INTERVAL=3`, and the data set's `BYSETPOS=-1;BYDAY=FR` order (`testdata/seed/dataset.yaml`); YEARLY plain, `BYMONTH=2;BYMONTHDAY=29` and plain from 29 February (leap years only), `BYMONTHDAY=-1`, `INTERVAL=99`; UNTIL at 2037-12-31 23:59:59 local; COUNT with EXDATE; daily and weekly 10:00 across both 2026 Paris changes.

One case is recorded, not counted as a match: **DTSTART off the rule** (`pcb-m4-notfirst`: a Wednesday DTSTART, `FREQ=WEEKLY;BYDAY=MO`). The library leaves DTSTART out (Mondays only) [verified: P29]. RFC 5545 §3.8.5.3 says "The DTSTART property defines the first instance in the recurrence set" and §3.3.10 "The DTSTART property value always counts as the first occurrence", but also that a set from an unsynchronised DTSTART "is undefined". What Proton's web app shows is E12's live step 6.3 B2.

`MONTHLY;INTERVAL` has no bound in Proton's set: 999, 99 999 and 999 999 999 neither panic nor hang (a few µs) [verified: `TestHugeMonthlyInterval`]. Walking a daily rule from 1990 to a 2037 range costs about 3 ms [verified: `TestCost`].

### DST: the failing cases

RFC 5545 §3.3.5, applied to recurrence instances by §3.3.10: a local time that occurs twice is **the first** of the two; a local time that does not exist is read **with the UTC offset before the gap** (02:30 in a 02:00→03:00 gap is 03:30 summer time). Weekly rules at the transition time, `COUNT=3`, second occurrence on the change day [verified: `TestDSTLibraryZoned`]:

| Zone, change | RFC 5545 §3.3.5 | rrule-go v1.8.2 | Result |
|---|---|---|---|
| Europe/Paris, gap 2026-03-29, 02:30 | 01:30Z (03:30 CEST) | 01:30Z | match |
| Europe/Paris, overlap 2026-10-25, 02:30 | 00:30Z (02:30 CEST, the first) | 01:30Z (02:30 CET, the second) | **fails** |
| America/New_York, gap 2026-03-08, 02:30 | 07:30Z (03:30 EDT) | 06:30Z (**01:30 EST**: the wall clock moves back an hour) | **fails** |
| America/New_York, overlap 2026-11-01, 01:30 | 05:30Z (01:30 EDT) | 05:30Z | match |
| Australia/Sydney, gap 2026-10-04, 02:30 | 16:30Z on 10-03 (03:30 AEDT) | 16:30Z | match |
| Australia/Sydney, overlap 2026-04-05, 02:30 | 15:30Z on 04-04 (02:30 AEDT) | 16:30Z (02:30 AEST, the second) | **fails** |

Cause: the library builds each occurrence with `time.Date` in DTSTART's zone [source: RG/rrule.go:637-639], and Go's `time.Date` "does not guarantee which" of the two zones it picks in a transition (Go documentation); in practice it picks differently per zone [verified: `TestGoTimeDate`, same table]. The plan's own Task 3 case (Paris spring gap) passes by chance. Away from the transition hour every occurrence keeps its wall-clock time [verified: P27, P28, RFC examples 2, 3, 6, 7, 12].

The same rules expand correctly when the library is fed wall clocks as UTC (no DST) and each result is resolved to an instant by our own §3.3.5 function [verified: `TestDSTLibraryFloatingPlusResolver`, 6/6]. That workaround is not the rule's "adopt it": it moves UNTIL, EXDATE, RECURRENCE-ID and the instant resolution, the parts that carry the risk, into our code, and keeps an unmaintained dependency for the date walk only.

### Parsing edges (whichever expander)

- `DTSTART;VALUE=DATE` is not parsed by `StrSliceToRRuleSetInLoc` ("bad TZID parameter format") [verified: `TestParsingEdges`].
- An UNTIL given as a DATE on a timed DTSTART bounds at midnight, so the UNTIL day's own occurrence is lost (`DTSTART` 10:00, `UNTIL=20260107` gives 01-05 and 01-06) [verified: `TestParsingEdges`]. RFC 5545 §3.3.10 requires the same value type as DTSTART; `checkRRule` accepts a DATE UNTIL on any DTSTART (`untilDay`, `internal/ical/validate.go`).
- Lower-case part names and unknown parts are errors [verified: `TestParsingEdges`]; `checkRRule` reads part names case-insensitively.

## Decision

By the plan's rule: every RFC 5545 example (with erratum 3883) and every Proton form match, but **DST occurrences do not keep the wall-clock time RFC 5545 §3.3.5 gives** (three of six transitions above: Paris and Sydney overlaps take the second instant, the New York gap moves 02:30 back to 01:30 EST). The library is **not adopted**; `ical.Expand` (Task 3) gets **our own expander over Proton's set**. Further reasons, not needed for the outcome: the library is unmaintained [claim: issue 72], it would enter the `pcb` binary for the first time, and it omits a DTSTART off the rule.

## What Proton's set bounds

Writes: `ical.ParseWrite` refuses, before any Proton call, every RRULE outside Proton's set (`UnsupportedRecurrence`), RDATE (`RDate`), EXRULE and RANGE (E12 "What the code assumes"). **RDATE is never stored** (Proton refuses it; `ical.RDate` refusal), so Expand handles RRULE, EXDATE and overrides only (global constraint, correcting design §5.6). The expander must support exactly these shapes (`checkRRule`, ruling M4-R10), and nothing else:

| Part | Values |
|---|---|
| FREQ | DAILY, WEEKLY, MONTHLY, YEARLY |
| INTERVAL | ≥ 1 (≤ 999 daily, ≤ 4999 weekly, ≤ 99 yearly, unbounded monthly) |
| COUNT | 1-49, DTSTART counting as the first |
| UNTIL | a DATE or a UTC DATE-TIME, inclusive, ≤ 2037-12-31; never with COUNT |
| WKST | a weekday (changes WEEKLY with INTERVAL > 1 and several BYDAY: RFC examples 37 and 38) |
| WEEKLY BYDAY | a list of plain weekdays |
| MONTHLY | nothing (DTSTART's day; months without it skipped), or one BYMONTHDAY in 1..31 or -31..-1 (months without it skipped), or one plain BYDAY weekday with one BYSETPOS in {-1, 1, 2, 3, 4} |
| YEARLY | nothing (DTSTART's month and day; 29 February only in leap years), or one BYMONTH with one BYMONTHDAY |

Reads may meet rules another client stored. Two have a Proton-set equivalent that `ParseWrite` already rewrites: MONTHLY `BYDAY=<n><DAY>` (R5, `monthlyOrdinal`) and DAILY with plain BYDAY (R12, `dailyByday`). [inferred] Expand can apply those two rewrites first, then check the shape with `checkRRule`, and report any other shape as an `UnsupportedRecurrence` refusal for the caller to show. Numeric limits (COUNT ≤ 49, UNTIL ≤ 2037, INTERVAL caps) do not affect how a rule expands. The per-call caps (1000 occurrences per series, range ≤ 366 days) bound the cost.

## To apply in Task 3

The library is not adopted, so:

- `go.mod`: **no change**. `github.com/teambition/rrule-go v1.8.2 // indirect` stays, pulled only by go-ical in `internal/caldav`'s tests. `internal/ical` must not import it.
- `README.md` §Credits: **no line**. The expander is written from RFC 5545 (§3.3.5, §3.3.10, §3.8.5.3), and no code is taken from rrule-go or dateutil. If Task 3 copies anything after all, the credit is: "**teambition/rrule-go** v1.8.2 (MIT): audited in experiment E16 (`spec/experiments/E16-rrule.md`) and not adopted; …".
- SPEC §17, row E16, "Method" column (as E17's row): `done (M8): rrule-go v1.8.2 not adopted (DST gap and overlap instants differ from RFC 5545 §3.3.5); own expander over Proton's set, \`E16-rrule.md\``. SPEC §6, bullet "Recurrence" (the line "library choice after the audit E16 when time-range queries or free-busy are needed [call]") becomes "expansion in `ical.Expand`, our own expander over Proton's set (E16)".
- If a later ruling overturns this and adopts the library: `require github.com/teambition/rrule-go v1.8.2` moves to the direct block. README gets the credit line above, ending "used as a dependency for the date walk; wall clocks resolved by our own RFC 5545 §3.3.5 code". SPEC §17 reads `done (M8): rrule-go v1.8.2 adopted on wall clocks, own instant resolution`.

Points for `ical.Expand` (Task 3), from the findings:

- **Resolve wall clocks to instants with our own §3.3.5 function, never bare `time.Date`**: an overlap takes the first instant, a gap the offset before the gap. Use it for DTSTART, each occurrence, EXDATE, RECURRENCE-ID and all-day midnights. Floating and all-day times use it in `loc`.
- **Add tests that bite on the resolver**: the plan's `TestExpandWeeklyKeepsWallClockAcrossDST` (Paris spring gap) passes with bare `time.Date`. Add the New York gap (`DTSTART;TZID=America/New_York:20260301T023000`, weekly, second occurrence 2026-03-08 07:30Z) and the Paris overlap (`DTSTART;TZID=Europe/Paris:20261018T023000`, weekly, second occurrence 2026-10-25 00:30Z).
- **COUNT before EXDATE**: COUNT counts generated occurrences; EXDATE removes afterwards (P25).
- **UNTIL**: inclusive. A UTC UNTIL compares as an instant. A DATE UNTIL on a timed DTSTART keeps the whole local day in DTSTART's zone [call: `checkRRule` accepts the shape; the RFC calls it invalid].
- **DTSTART off the rule**: RFC 5545 says DTSTART is the first instance. Include it and count it towards COUNT [assumed → E12 live step 6.3 B2, which records what the web app shows].
- **Long series**: without COUNT, DAILY and WEEKLY can jump to the period before `from` instead of walking from DTSTART. MONTHLY with a huge INTERVAL must stop past year 9999, not overflow.
- Port the cases in this file (the RFC examples whose rules are in Proton's set: 1-4, 6-11, 12 and 13 after R5's rewrite of `BYDAY=1FR`, 16, 37, 38; P01-P30; the six DST rows) as `expand_test.go` table cases.

## Appendix: the Proton-form cases

DTSTART `TZID=Europe/Paris` at 10:00 local unless stated. Expected starts are local; "first n" means an unbounded rule checked on its first n occurrences.

| # | DTSTART | RRULE (and EXDATE) | Expected |
|---|---|---|---|
| P01 | 2026-01-05 | `FREQ=DAILY` | first 5: 01-05..01-09 |
| P02 | 2026-01-05 | `FREQ=DAILY;COUNT=49` | 01-05..02-22 |
| P03 | 2026-01-05 | `FREQ=DAILY;INTERVAL=999;COUNT=3` | 2026-01-05, 2028-09-30, 2031-06-26 |
| P04 | 2026-01-05 | `FREQ=DAILY;UNTIL=20260108T090000Z` | 01-05..01-08 (UNTIL is the last instance) |
| P05 | 2026-01-05 | `FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR` | first 10: 01-05..01-09, 01-12..01-16 |
| P06 | 2026-01-05 | `FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;WKST=SU;COUNT=4` | 01-05, 01-07, 01-19, 01-21 |
| P07 | 2026-01-06 | `FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO` | 01-06, 01-11, 01-20, 01-25 |
| P08 | 2026-01-05 | `FREQ=WEEKLY;INTERVAL=4999;COUNT=2` | 2026-01-05, 2121-10-27 |
| P09 | 2026-01-15 | `FREQ=MONTHLY;COUNT=3` | 01-15, 02-15, 03-15 |
| P10 | 2026-01-31 | `FREQ=MONTHLY;BYMONTHDAY=31;COUNT=4` | 01-31, 03-31, 05-31, 07-31 |
| P11 | 2026-01-31 | `FREQ=MONTHLY;COUNT=3` | 01-31, 03-31, 05-31 |
| P12 | 2026-01-31 | `FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=4` | 01-31, 02-28, 03-31, 04-30 |
| P13 | 2026-01-13 | `FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2;COUNT=4` | 01-13, 02-10, 03-10, 04-14 |
| P14 | 2026-01-30 | `FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1;COUNT=4` | 01-30, 02-27, 03-27, 04-24 |
| P15 | 2026-01-04 | `FREQ=MONTHLY;BYDAY=SU;BYSETPOS=1;COUNT=3` | 01-04, 02-01, 03-01 |
| P16 | 2026-01-21 | `FREQ=MONTHLY;BYDAY=WE;BYSETPOS=3;COUNT=3` | 01-21, 02-18, 03-18 |
| P17 | 2026-01-22 | `FREQ=MONTHLY;BYDAY=TH;BYSETPOS=4;COUNT=3` | 01-22, 02-26, 03-26 |
| P18 | 2026-01-13 | `FREQ=MONTHLY;INTERVAL=3;BYDAY=TU;BYSETPOS=2;COUNT=3` | 01-13, 04-14, 07-14 |
| P19 | 2026-03-13 | `FREQ=YEARLY;COUNT=3` | 2026, 2027, 2028-03-13 |
| P20 | 2028-02-29 | `FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=3` | 2028, 2032, 2036-02-29 |
| P21 | 2028-02-29 | `FREQ=YEARLY;COUNT=2` | 2028-02-29, 2032-02-29 |
| P22 | 2026-02-28 | `FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-1;COUNT=3` | 2026-02-28, 2027-02-28, 2028-02-29 |
| P23 | 2026-03-13 | `FREQ=YEARLY;INTERVAL=99;COUNT=2` | 2026-03-13, 2125-03-13 |
| P24 | 2037-12-29 | `FREQ=DAILY;UNTIL=20371231T225959Z` | 12-29, 12-30, 12-31 |
| P25 | 2026-01-05 | `FREQ=DAILY;COUNT=5`, `EXDATE;TZID=Europe/Paris:20260106T100000` | 01-05, 01-07, 01-08, 01-09 |
| P26 | 2026-01-05 | `FREQ=DAILY;COUNT=3`, `EXDATE:20260106T090000Z` | 01-05, 01-07 |
| P27 | 2026-03-27 | `FREQ=DAILY;COUNT=4` | 03-27..03-30, all 10:00 |
| P28 | 2026-10-18 | `FREQ=WEEKLY;COUNT=3` | 10-18, 10-25, 11-01, all 10:00 |
| P29 | 2026-01-07 (Wednesday) | `FREQ=WEEKLY;BYDAY=MO;COUNT=2` | library: 01-12, 01-19; RFC reading: 01-07, 01-12 |
| P30 | 2025-01-31 17:00 | `FREQ=MONTHLY;BYSETPOS=-1;BYDAY=FR` | first 4: 01-31, 02-28, 03-28, 04-25 |

## Open

- E12 live step 6.3 B2: whether Proton's web app shows a DTSTART that is off the rule (the "DTSTART off the rule" point in Task 3's list).
- Revisit only if expansion must cover rules beyond Proton's set (CalDAV `calendar-query` time ranges over foreign data); a maintained library should then be re-audited with the same DST table.
