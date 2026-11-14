# E13 — Unknown and extra iCalendar properties: kept, dropped or refused?

Status: **source-based; the M4 write path routes them; live run pending** (`spec/HUMAN-CHECKS.md` §6, steps W7, 6.3 C1, 6.4.1). Marks as in E10. Evidence: `raw/m4-research.md` §F, §B.1, §B.5. Feeds SPEC §5.6, §8.4, `spec/mapping.md` §3.

## Question

1. Does Proton store and serve back, byte for byte, VEVENT properties outside its routing table (`X-MOZ-*`, other `X-` properties, `CLASS`, `CATEGORIES`, `URL`, `ATTACH`, `GEO`, `PRIORITY`, `RESOURCES`, `CONTACT`, `RELATED-TO`) when they sit in the shared encrypted part?
2. Does the Proton web app keep those lines when the user later edits the event there?
3. Do the calendar parts' properties (`COMMENT` in calendar encrypted, `TRANSP` and `STATUS` in calendar signed) round-trip?

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| The web client puts every VEVENT property outside the routing table into the shared encrypted part ("rest") | proton-cal `docs/crypto.md:188-197` | [claim: proton-cal, read from the web client] |
| carbonate and caloxide do the same and say it round-trips | `carbonate/internal/calendar/write.go:416-421,943-958`; `caloxide/protonmail/calendar.go:736-743` | [claim] |
| The server cannot read an encrypted part, so it keeps it as sent | research F | [inferred, high] |
| cal-gateway strips URL, CATEGORIES, ATTACH, GEO, CLASS, X-APPLE-* on create and keeps only what was already stored on update | `cal-gateway/docs/FEATURE-MATRIX.md:157-167` | [source] |
| Whether the web app keeps "rest" lines after its own edit | none | unknown |
| Thunderbird keeps every property it does not model and writes it back; losing `X-MOZ-LASTACK` or `X-MOZ-SNOOZE-TIME*` makes dismissed alarms fire again | E08 §6.2 | [source: E08] |

## What the code assumes

`internal/ical/write.go` (`Route`, `Patch`); `spec/mapping.md` §3 and §9.
- Every VEVENT property the routing table does not name goes to the shared encrypted part, as sent. [verified: `TestRoutingTable`, `TestPatchCreate`]
- On update, a stored line the body still carries keeps its part and its bytes; one the body dropped is removed; a stored `COLOR` line and stored nested components are kept even when the body lacks them; pcb's own `X-PCB-*` and `X-PM-SESSION-KEY` are always stripped. [verified: `TestPatchKeepsUntouchedBytes`, `TestPatchStripsOursAndKeepsColorAndComponents`, `TestPatchMultiValuedAsMultiset`]
- Not kept: VALARM-level properties (`X-MOZ-LASTACK` inside a VALARM, `DESCRIPTION`, `REPEAT`…): alarms live in the row's `Notifications` and are rendered again (research F, `mapping.md` §5).
- Accepted and dropped: `STATUS:TENTATIVE` (Proton refuses it with 2000), the client's `COLOR`, VCALENDAR-level properties, VTIMEZONE.

The fake Proton stores encrypted parts as sent, which is the assumption under test.

## Live run that settles it

`spec/HUMAN-CHECKS.md` §6:
1. **W7** (agent): creates `pcb-m4-props` with `X-MOZ-GENERATION`, `X-MOZ-LASTACK`, an unknown `X-` property with a parameter, `CLASS:PRIVATE`, `CATEGORIES`, `URL`, `ATTACH`, `GEO`, `PRIORITY`, `RESOURCES`, `CONTACT`, `RELATED-TO`, `COMMENT`, `TRANSP`; reads it back (question 1, 3) and checks that the shared signed part's clear text holds none of them.
2. **6.3 B1** (author): the web app opens it.
3. **6.3 C1** (author): renames it in the web app.
4. **6.4.1** (agent): reads it again: which lines the web app kept (question 2).
5. **6.3 D11** (author, Thunderbird): a dismissed alarm does not fire again after the round trip (the VEVENT-level `X-MOZ-LASTACK` kept, the VALARM-level one lost).

Results: not run yet. Fill:

| Property | After pcb's create | After a pcb edit | After a web-app edit |
|---|---|---|---|

## Decision it feeds

- Lines dropped by Proton on store: pcb would refuse them (a silent loss is worse than a 403, SPEC §1).
- Lines dropped by the web app's own edit: documented in README and `mapping.md` (another client's behaviour; pcb cannot prevent it).
- A dismissed alarm firing again: the VALARM-level `X-MOZ-LASTACK` is needed, and an alarm state store (outside Proton) would be the only fix; until then documented.
