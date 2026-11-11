# 0012 — iCalendar-native model for v1

- **Date**: 2026-11-11
- **Decided by**: agent (controller ruling M3-R2), author to confirm
- **Status**: provisional until the author confirms; supersedes 0006 for v1

The bridge keeps events in iCalendar from end to end in v1. A decrypted Proton event is a set of iCalendar fragments (the card parts, `internal/proton/cards`); the iCalendar codec (`internal/ical`) merges them into one VEVENT line by line, keeping every property it does not interpret (`X-MOZ-*`, `X-PM-*`, unknown ones) byte for byte, and the CalDAV adapter serves the result. There is no JSCalendar (RFC 8984) layer in v1.

Why:
- CalDAV is the only adapter (0007), and Proton itself stores iCalendar fragments: a JSCalendar model would sit between two iCalendar ends.
- A JSCalendar round trip is lossy where Thunderbird needs fidelity: E08 asks the server to store iCalendar verbatim and return it unchanged, `X-MOZ-LASTACK` and `X-MOZ-SNOOZE-TIME*` included (dismissed alarms re-fire otherwise). Mapping every unknown property, parameter and nested component through JSCalendar and back is the codec work 0006 wanted to avoid, done twice.
- Byte-exact handling is testable: a part goes in, the same lines come out.

JSCalendar (0006) returns with the first adapter that is not iCalendar-native (REST, MCP, JMAP), as a codec over this model. Cost if wrong: that codec layer is added later rather than now.
