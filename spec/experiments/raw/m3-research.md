# M3 research — CalDAV read for Thunderbird (E17 + card codec + mapping + auth/TLS)

Date: 2026-11-11. Read-only study; no code copied. Paths abbreviated: citations are file:line relative to each project's own tree: caloxide = WentTheFox/caloxide (https://github.com/WentTheFox/caloxide), carbonate = develonrails/carbonate (https://github.com/develonrails/carbonate), cal-gateway = jmdlab/cal-gateway (https://github.com/jmdlab/cal-gateway), proton-cal = cheeseandcereal/proton-cal (https://github.com/cheeseandcereal/proton-cal), go-webdav = emersion/go-webdav v0.7.0 (https://github.com/emersion/go-webdav, tag v0.7.0, commit f93b8288, 2025-10-18), go-proton-api = ProtonMail/go-proton-api `v0.0.0-20261022084531-3f8fbebac58d` (https://github.com/ProtonMail/go-proton-api), gopenpgp = ProtonMail/gopenpgp `v2.11.1-proton` (https://github.com/ProtonMail/gopenpgp). SevenOfNine-labs = SevenOfNine-labs/proton-calendar-bridge (https://github.com/SevenOfNine-labs/proton-calendar-bridge).
- `E08 §x` = `spec/experiments/E08-thunderbird-caldav.md` (authoritative for Thunderbird, "TB")
- Confidence: [source] = read in the cited code; [claim: X live] = the project X says it observed it against a real account/client; [claim: X] = X states it, no live evidence; [inferred] = my reading, not tested.

Licences: go-webdav MIT (`go-webdav/LICENSE`); caloxide, carbonate, cal-gateway MIT; go-proton-api MIT. **Correction:** proton-cal is **Unlicense (public domain)**, not MIT (`proton-cal/README.md:332-334`, file `UNLICENSE`). SevenOfNine-labs has no CalDAV (REST + bearer token, `internal/security/auth.go:14-27`); ignored below.

---

## A. E17 — go-webdav audit for a CalDAV server

### A.1 Maintenance
- Last release **v0.7.0, 2025-10-18** (gh releases); before: v0.6.0 2024-12-06, v0.5.0 2024-01-18. ~1 release/year. [source: gh api]
- `master` moves slowly: last push 2026-06-28; post-v0.7.0 commits are mostly client side, plus `1901d193` (2026-06-23) "derive current-user-privilege-set from a read-only flag" — **unreleased**. [source: gh api]
- 35 open issues + ~20 open PRs. Server-relevant open items:
  - https://github.com/emersion/go-webdav/issues/35 "Collection Synchronization" (open since 2020)
  - https://github.com/emersion/go-webdav/issues/148 "Missing CalDAV server features" (RFC 6578, RFC 6638 unchecked)
  - https://github.com/emersion/go-webdav/issues/41 trailing-slash issues
  - https://github.com/emersion/go-webdav/issues/13 streaming multistatus
  - https://github.com/emersion/go-webdav/issues/118 weak ETags; https://github.com/emersion/go-webdav/issues/117 multiple ETags in If-Match
  - https://github.com/emersion/go-webdav/issues/183 PROPPATCH; https://github.com/emersion/go-webdav/issues/178 allprop returns too much; https://github.com/emersion/go-webdav/issues/195 partial multistatus
  - PRs still open: getctag https://github.com/emersion/go-webdav/pull/208 (2026-06-24) and https://github.com/emersion/go-webdav/pull/213; If-Match on client PUT https://github.com/emersion/go-webdav/pull/209.
  - https://github.com/emersion/go-webdav/pull/198 "sync-collection functionality" (merged 2025-12-18) touches only `caldav/caldav.go` and `caldav/client.go`: **client** side. Master's `caldav/server.go` still has no sync-collection. [source: gh api]

### A.2 What the v0.7.0 CalDAV server does

| Feature | v0.7.0 server | Evidence |
|---|---|---|
| Routing | resource kind from **path depth** only: root / principal (1) / home set (2) / calendar (3) / object (4) | `go-webdav/caldav/server.go:295-315` |
| `/.well-known/caldav` | exact-path **308** to `CurrentUserPrincipal(ctx)` (so it runs behind your auth: unauthenticated → your 401, OK for TB E08 §1.5) | `server.go:61-70` |
| OPTIONS | **204**, `DAV: 1, 3, calendar-access`, `Allow` without PUT on collections | `go-webdav/internal/server.go:119-130`; `caldav/server.go:317-340` |
| PROPFIND body | needs `text/xml` or `application/xml` (TB sends `text/xml`: OK); empty body = allprop; Depth default infinity; **body size unbounded** | `internal/server.go:31-45,132-161` |
| PROPFIND `/` | only `current-user-principal` + `resourcetype`, and the response href is the **principal path**, not `/` | `caldav/server.go:461-474` |
| Principal | `current-user-principal`, `calendar-home-set`, `resourcetype` (collection+principal); exact `r.URL.Path == principalPath` compare, a slash mismatch gives an **empty 207** | `server.go:382-407,476-496` |
| Home set | `current-user-principal`, `resourcetype` | `server.go:498-516` |
| Calendar | `resourcetype` (collection+calendar), `displayname`, `calendar-description`, `supported-calendar-data`, `supported-calendar-component-set` (default VEVENT), `max-resource-size`, `current-user-principal`, **`current-user-privilege-set` hard-coded read+write** in ONE `<privilege>` element | `server.go:518-577`; `internal/elements.go:457-467` |
| Missing on calendar | `getctag`, `supported-report-set`, `sync-token`, `owner`, `A:calendar-color`, `calendar-timezone` (unknown props → 404 propstat) | `server.go:574` TODO; `internal/server.go:223-225` |
| Object | `getetag` (Go `%q` quoting), `getcontenttype` `text/calendar`, `getcontentlength`, `getlastmodified`, `calendar-data` | `server.go:603-644`; `internal/elements.go:379-396` |
| REPORT | only `calendar-query` and `calendar-multiget`; anything else **400** ("expected calendar-query or calendar-multiget") → **no sync-collection** | `server.go:90-102` |
| calendar-query | comp/prop/param filters decoded; matching left to the backend (exported `caldav.Filter`/`Match`, with RRULE expansion through go-ical) | `server.go:210-245`; `caldav/match.go:12,35,131` |
| multiget | one `GetCalendarObject` per href; a missing href becomes a response-level status (TB-compatible) **with `responsedescription` = `err.Error()`** (leaks backend error text) | `server.go:247-288`; `internal/elements.go:126-143` |
| calendar-data | always **re-encoded** from a parsed `ical.Calendar` (no verbatim bytes); an encoder error gives a 500 propstat for that object | `server.go:615-623`; `internal/server.go:212-219` |
| GET/HEAD | `ETag`, `Last-Modified`, re-encoded body | `server.go:342-367` |
| PUT | `If-Match`/`If-None-Match` handed raw to the backend (`ConditionalMatch`; `strconv.Unquote` fails on `W/"…"`); content type must be `text/calendar`; **always 201** | `server.go:668-712`; `go-webdav/webdav.go:41-68` |
| DELETE | 204 on success | `internal/server.go:92-97` |
| PROPPATCH / COPY / MOVE | 501 | `server.go:664-666,743-749` |
| MKCOL | MKCOL only (no MKCALENDAR) | `server.go:718-741` |
| Scheduling (RFC 6638), RFC 5397 beyond `current-user-principal` | none | grep |
| Reusable pieces | the XML element types are in `internal/` → **not importable** by our module. Exported: `caldav.Backend/Handler`, `Calendar`, `CalendarObject`, `Filter`/`Match`, `NewPreconditionError`, the client | `go-webdav/caldav/*.go`, `go-webdav/elements.go` |

### A.3 Thunderbird needs (E08 MUST list) vs v0.7.0

| E08 requirement | v0.7.0 | Gap |
|---|---|---|
| MUST 1 401 + `WWW-Authenticate: Basic` | not in lib (wrapper) | ours anyway |
| MUST 2 well-known redirect same origin, no 403 | 308 OK | none (TB follows 308, E08 §1.5) |
| MUST 3 `resourcetype` always present | yes (`internal/server.go:174-176`) | trailing-slash empty 207 risk [inferred from `server.go:387,413`] |
| MUST 5 `supported-report-set` listing sync-collection; `CS:getctag`; `owner` | **absent** | wrapper or fork |
| MUST 5 accurate privileges (read-only calendars) | **always read+write** (fixed on unreleased master) | Proton subscribed/holiday/view-only calendars look writable |
| MUST 6 OPTIONS 2xx on home and calendar | 204 | none |
| MUST 7 sync-collection (empty token, 404 tombstones, 4xx on bad token, 507 paging) | **absent (400)** | full re-implementation |
| MUST 8 multiget, response-level 404 | yes | one undecodable event → 500 propstat → TB fails the **whole 100-href multiget** (E08 §3.4) |
| MUST 10 PUT 201/204, 412, 409 no-uid-conflict | 201 always; preconditions are the backend's job | M4 wrapper |
| MUST 13 serve iCalendar verbatim | **impossible** (re-encoded via go-ical) | M4 alarm state (X-MOZ-*) at go-ical's mercy |
| MUST 14 `text/xml` bodies | yes | none |
| AVOID whitespace in leaves | encoding/xml, no indent | none |

### A.4 How the three bridges use it and what they patched

**caloxide** (fork, `replace` → `./third_party/go-webdav`, `caloxide/go.mod:15,29`)
- Patch (`caloxide/third_party/go-webdav/CALOXIDE-PATCH.md:9-28`): adds `Calendar.CTag` and `Calendar.Color`, served as `CS:getctag` and `A:calendar-color` (diff on `caldav/caldav.go:69`, `caldav/elements.go:23`, `caldav/server.go:570`).
- No sync-collection: TB falls back to ctag + PROPFIND Depth 1 (E08 §3.1).
- CTag = sum of FNV-64 of (path, ETag) over the served view + count (`caloxide/caldav/cache.go:452-471`).
- go-ical's strict encoder forced a repair pass: `makeEncodable` (`caloxide/caldav/encodable.go:18-24,50`) and `sanitizeICS` for unescaped newlines in imported events (`caloxide/caldav/caldav.go:86-117,130-142`). [claim: caloxide, from real data]

**carbonate** (upstream v0.7.0, mis-marked `// indirect` in `carbonate/go.mod:39` though imported directly)
- `internal/davcompat` buffers every response and **rewrites the XML with regexes** (`carbonate/internal/davcompat/davcompat.go:26-27,86,152-170,326-380`):
  - splits the merged `<privilege><read/><write/></privilege>` into one element per privilege (`:417`);
  - adds PUT/GET/HEAD to `Allow` (`:384-388`);
  - strips `getctag` from the request and injects it into the **first successful propstat**, because Evolution stops at the first propstat (`carbonate/docs/ARCHITECTURE.md:137-150`);
  - supplies `supported-report-set`;
  - turns PUT 201 into 204 via a context mark (`davcompat.go:40-47`; `ARCHITECTURE.md:388-391`).
- sync-collection is caught by regex before go-webdav sees it (`carbonate/internal/caldav/compat.go:23-35,41-75`), answered from hand-built XML (`carbonate/internal/caldav/sync.go:165-205`).
- MKCALENDAR and PROPPATCH are intercepted (`carbonate/internal/caldav/backend.go:114-141`).
- Root OPTIONS answers with a DAV header, because GNOME Online Accounts needs it (`carbonate/internal/server/server.go:199-230`).
- Depth-routing constraint documented (`ARCHITECTURE.md:57-79`).

**cal-gateway** (upstream v0.7.0, `cal-gateway/go.mod:11`), five interceptors in front of go-webdav (`cal-gateway/internal/server/server.go:161-186`):
- root PROPFIND, because go-webdav gives 404 for `principal-URL`/`calendar-home-set` there and macOS gives up (`rootpropfind.go:1-8`);
- principal PROPFIND for `calendar-user-address-set` (`principalpropfind.go:1-13`);
- PROPPATCH 501 → 207 accept-all (`proppatch.go:1-7`);
- a PUT guard (VTODO etc. → 403) (`putguard.go:1-23`);
- RFC 6638 inbox/outbox plus a `calendar-auto-schedule` DAV token (`scheduling.go:44-49,55-61`);
- `http.MaxBytesHandler` 8 MiB, because go-webdav buffers bodies with no limit (`server.go:179-192`).
- No getctag, no sync-collection: TB does a full Depth 1 PROPFIND every refresh [inferred: grep finds `getctag` only on the inbox, `scheduling.go:302-304`].

**Known bugs / pitfalls collected:**
- privileges merged and hard-coded;
- PUT always 201;
- no ctag, no sync, no supported-report-set;
- root PROPFIND too thin;
- PROPPATCH 501 breaks macOS;
- unbounded bodies;
- `responsedescription` leaks `err.Error()`;
- strict encoder: 500 propstat, which TB turns into a failed multiget;
- depth-based routing fixes the URL layout;
- weak/multi ETags unsupported.

### A.5 Verdict
**Write our own minimal server; do not depend on go-webdav at runtime.** Use it only in tests: its `caldav.Client` (plus master's client SyncCollection) is an independent client next to TB's MOCK shapes.
- Using it "as is" fails TB-relevant MUSTs: sync-collection, getctag, supported-report-set, privileges.
- "With wrappers" means: regex XML rewriting (carbonate), a fork (caloxide), or five interceptors that re-implement half the PROPFINDs (cal-gateway). In each case we'd test go-webdav's behaviour plus our patches, and we still could not serve verbatim bytes or control status codes. Its XML types are `internal/`, so SPEC §9's "our own handlers on its XML types" is not possible. That contradicts "every assumption tested and we own the behaviour".
- Surface estimate for our own server [inferred]:
  - **Methods**: OPTIONS (any path, 200/204 + `DAV: 1, 3, calendar-access`, never `calendar-schedule`/`auto-schedule`), PROPFIND Depth 0/1 (refuse infinity with 403 `propfind-finite-depth`), REPORT `sync-collection` + `calendar-multiget` (+ `calendar-query` only when E16 recurrence expansion exists; until then do not advertise it), GET/HEAD on objects, 308/301 on well-known, 401 challenge. PUT/DELETE answer 403 in M3 (M4 adds them).
  - **Resources**: `/`, `/.well-known/caldav`, principal, home set, one collection per calendar, `<name>.ics` objects directly under it.
  - **Properties (~18)**: `resourcetype`, `displayname`, `current-user-principal`, `principal-URL`, `owner`, `calendar-home-set`, `current-user-privilege-set`, `supported-report-set`, `supported-calendar-component-set` (VEVENT only), `supported-calendar-data`, `CS:getctag`, `DAV:sync-token`, `A:calendar-color`, `calendar-description` (optional), `getetag`, `getcontenttype`, `getcontentlength`, `calendar-data` (REPORT only).
  - **Size**: ~1 000–1 300 lines of Go with encoding/xml (go-webdav's broader server is ~1 700 lines of `caldav/server.go` + `internal/`), plus ~2× that in contract tests that map one-to-one to E08 MUST 1–14 and AVOID.

---

## B. Proton event cards → iCalendar

### B.1 Parts and keys
- Card `Type`: 0 clear, 1 encrypted, 2 signed, 3 encrypted+signed (bit flags) [source] (`proton-cal/docs/crypto.md:181-182`; `go-proton-api/calendar_event_types.go:87-93`; `caloxide/protonmail/calendar.go:120-145`).
- Row fields: `SharedKeyPacket`, `CalendarKeyPacket`, `SharedEvents`, `CalendarEvents`, `AttendeesEvents`, `PersonalEvents`; per part `MemberID` (personal only), `Author` (`go-proton-api/calendar_event_types.go:9-44`). go-proton-api omits `AddressKeyPacket`, `AddressID`, `RecurrenceID`, `Notifications`, `Color`, `IsOrganizer` → read them from the raw JSON (we keep the raw row: fine) (`cal-gateway/internal/proton/update.go:170-190`; `carbonate/internal/calendar/calendar.go:60-79`).

| Part | Crypto | Key | Props (proton-cal table) | Confidence |
|---|---|---|---|---|
| Shared signed | plaintext + detached sig | none | UID, DTSTAMP, DTSTART, DTEND, [RECURRENCE-ID], [RRULE], EXDATE*, SEQUENCE, ORGANIZER, [X-PM-CONFERENCE-ID] | [claim: proton-cal live] `proton-cal/docs/crypto.md:173-176`; real-account sample in `carbonate/docs/ARCHITECTURE.md:260-271` [claim: carbonate live] |
| Shared encrypted | SEIPD, sig over **plaintext** | session key from `SharedKeyPacket` (PKESK to calendar key) — or `AddressKeyPacket` (PKESK to **our address key**) for Proton-to-Proton invites | UID, DTSTAMP, CREATED, SUMMARY, DESCRIPTION, LOCATION, X-PM-CONFERENCE-URL **+ every other property ("rest")** | crypto.md:176,188-197 [claim: proton-cal, read from web client]; AddressKeyPacket `cal-gateway/internal/proton/crypto.go:166-175`, `update.go:174-182` [claim: cal-gateway, has live invite tests] |
| Calendar signed | plaintext + sig | none | UID, DTSTAMP, EXDATE*, STATUS, TRANSP | crypto.md:177 |
| Calendar encrypted | SEIPD + sig | `CalendarKeyPacket` | UID, DTSTAMP, COMMENT; may be **absent** (web-app events have no calendar card / key packet) | crypto.md:178; `proton-cal/docs/api.md` "Updates reuse session keys" [claim: proton-cal live] |
| Attendees | encrypted (sig?) | **shared** session key | UID, ATTENDEE* (with `X-PM-TOKEN`) | crypto.md:179,227-232; `proton-cal/pkg/event/decrypt.go:56-57`; `cal-gateway/.../crypto.go:175` |
| Personal | armored PGP message (no key packet), carries `MemberID` | **unclear**: go-proton-api and carbonate decrypt it with the **calendar** keyring (`go-proton-api/calendar_event_types.go:46-71`; `carbonate/internal/calendar/calendar.go:393-404`) | VALARM per member | [claim: proton-cal live] events observed carry **no** PersonalEvents; `Notifications` is the source of truth (`proton-cal/docs/api.md` "Note: reminders…", crypto.md:194-197). Key [inferred] |

- Key chain: address key → member passphrase (try **every** address key) → calendar keys (keep all generations) → PKESK → SEIPD (`proton-cal/docs/crypto.md:114-135`; `cal-gateway/internal/proton/crypto.go:18-24,113-144`).
- Decrypt with `GetBinary`, never `GetString`: `gopenpgp/crypto/message.go:204-206` folds CRLF→LF and sanitises UTF-8 (`proton-cal/pkg/pgp/pgp.go:119-121`; `cal-gateway/.../crypto.go:334-336`). go-proton-api's `Decode` uses `GetString` (`go-proton-api/calendar_event_types.go:70`). Its signature check re-canonicalises through `NewPlainMessageFromString` (`gopenpgp/crypto/message.go:93-100`), which only works if the fragment was pure CRLF and valid UTF-8 [inferred].
- Fallback: cal-gateway tries the calendar keyring, then each address keyring, on every encrypted card (`cal-gateway/.../crypto.go:325-352`).

### B.2 Merge into one VEVENT

| Bridge | Rule | Cite |
|---|---|---|
| proton-cal | line-level merge. Structural single-valued props (UID, DTSTAMP, DTSTART, DTEND, RRULE, RECURRENCE-ID, SEQUENCE): **shared-signed wins**. Other single-valued props: first seen. Multi-valued (EXDATE, ATTENDEE, RDATE, CATEGORIES, COMMENT): union, exact-duplicate lines dropped. Nested components (VALARM) kept verbatim. **`X-PM-SESSION-KEY` stripped**. Other `X-PM-*` kept. Unknown props verbatim | `proton-cal/pkg/ical/merge.go:17-43,62-115,117-164`; `crypto.md:288-306` |
| carbonate | single-valued: first seen; multi-valued list + all `X-` kept; part `COLOR` dropped in favour of the row `Color`; VALARMs from `Notifications`. Line-based: BEGIN/END lines are skipped, so a nested VALARM inside a part would be flattened [inferred] | `carbonate/internal/calendar/calendar.go:81-95,107-190` |
| caloxide | go-ical props map, **later card replaces earlier wholesale** (calendar card overrides shared on UID/DTSTAMP/EXDATE); Personal/Attendees ignored (TODO) | `caloxide/caldav/encodable.go:187-191`; `caloxide/caldav/caldav.go:170-201` |
| cal-gateway | parses selected fields into a struct and **rebuilds** the VEVENT (DTSTART from row instants and zones): unknown props are lost | `cal-gateway/internal/caldav/ics.go:31-108` |

Row-level, non-card data to re-inject (all [claim: proton-cal live], `proton-cal/docs/crypto.md:199-234`):
- `Color` → `COLOR` (RFC 7986).
- `Notifications` → one VALARM each: Type 0 → EMAIL, else DISPLAY. DISPLAY needs a DESCRIPTION (`proton-cal/pkg/event/ics.go:132-176`).
- **Tri-state**: `null` = calendar default (`CalendarSettings.DefaultPartDayNotifications` / `DefaultFullDayNotifications`), `[]` = none, array = custom. caloxide applies the defaults (`caloxide/caldav/caldav.go:255-261`).
- `Attendees[].Status` joins attendee cards by `Token` (0 needs-action, 1 tentative, 2 declined, 3 accepted) (`proton-cal/pkg/event/decrypt.go:69-83`; `cal-gateway/.../crypto.go:176-194`).

### B.3 Master + exceptions + EXDATE → one resource per UID
- Exceptions are separate rows with the same UID. `RecurrenceID` = Unix start of the original occurrence (plaintext). The RECURRENCE-ID line is in the exception's shared signed card. `RRule` and `Exdates[]` are plaintext columns denormalised from the master's signed card. No server-side expansion. [claim: proton-cal live] (`proton-cal/docs/api.md` "Raw event row", "Recurring events"; `proton-cal/pkg/event/decrypt.go:26-44`)
- **Deleting a master orphans its exceptions** (no cascade) [claim: proton-cal live], so orphan groups exist in real data. Serve them as is: TB fabricates a master (E08 §6.4). cal-gateway does the same (`cal-gateway/internal/caldav/ics.go:41-42`).
- Order: master first, then exceptions by RecurrenceID, then ID (`proton-cal/pkg/event/ics.go:43-94`; `caloxide/caldav/series.go:75-103`; carbonate keeps arrival order, `carbonate/internal/caldav/backend.go:437-452`).
- An exception VEVENT must carry RECURRENCE-ID and **no RRULE/EXDATE** (Radicale #1635 pitfall) (`cal-gateway/internal/caldav/ics.go:102-105`).
- Duplicate masters (several rows with RecurrenceID 0 under one UID) happen. cal-gateway sets extras aside (`cal-gateway/internal/caldav/backend.go:1380-1392`, "recurring-master corruption case" `ics.go:39-40`) [claim: cal-gateway live]. Policy needed [call].
- EXDATE may appear in both signed cards: take the union as a set (proton-cal); never last-wins (caloxide).

### B.4 Signatures
- What they cover: plaintext (cleartext cards) or the **plaintext of encrypted cards**, binary literal (sig type 0x00). Signer: the author's **address** key (`proton-cal/docs/crypto.md:316-335`; `carbonate/docs/ARCHITECTURE.md:412-414`).
- Who verifies:

| Bridge | Behaviour | Cite |
|---|---|---|
| proton-cal | **never verifies** | `proton-cal/pkg/pgp/pgp.go:94-121`; `proton-cal/pkg/event/decrypt.go:19-21` |
| cal-gateway | never verifies | `cal-gateway/internal/proton/crypto.go:32-34` |
| carbonate | verifies against **our** address keyring only; failure → `Unverified=true`, event still served (other members' events are never verifiable that way) | `carbonate/internal/calendar/calendar.go:434-471` |
| caloxide | verifies against the **row `Author`'s public keys** (fetched via GetPublicKeys and cached; empty Author → own keys); failure → counted and logged, event **served anyway** | `caloxide/caldav/caldav.go:152-157,227-253,720-747` |

- Recommendation: verify like caloxide (Author → public keys, cached), never drop on failure, count it and expose it in `doctor`/logs. Mark the event in the model so M4 refuses to re-sign over unverified content [call].

### B.5 Final VCALENDAR emitted

| Bridge | PRODID | VTIMEZONE | Notes |
|---|---|---|---|
| proton-cal | `-//proton-cal//proton-cal//EN` | none (bare IANA TZID, as Proton stores it) | **no trailing CRLF** (`proton-cal/pkg/ical/merge.go:50-58`, RFC 5545 violation); `proton-cal/pkg/event/ics.go:14-15`; crypto.md:307-314 |
| carbonate | `-//carbonate//EN` | none | `carbonate/internal/calendar/calendar.go:134-143,197-215` |
| caloxide | card props + `-//caloxide//Proton Calendar//EN` if missing | none | `caloxide/caldav/encodable.go:26-27,127-135` |
| cal-gateway | `-//cal-gateway//cal-gateway//EN` | generated: static table + **approximate** Jan/July probe (US-style transition dates, northern hemisphere) | `cal-gateway/internal/caldav/ics.go:17-18,71-84`; `vtimezone.go:10-23` (wrong rules for southern-hemisphere zones) |

- Proton accepts and emits TZID without VTIMEZONE [claim: proton-cal live] (`crypto.md:281-283`). TB does not need VTIMEZONE (E08 §6.3).
- For M3, omit VTIMEZONE. If we add it later, Go's `time.Time.ZoneBounds` (Go ≥ 1.19) walks real tzdata transitions, unlike cal-gateway's probe [inferred].

---

## C. Cache → CalDAV mapping for Thunderbird

The M2 schema matters here (`internal/store/cache/schema.go:23-45`, `cache.go:242-256,408-420,506-520`):
- `events` has `uid`, `etag`, `seq`, `recurrence_id`.
- `tombstones` is `(calendar_id, id, seq)` with **no UID**.
- `seq` is **one global counter** (`meta.seq`), named by `epoch`.

| Item | Recommendation | Why / evidence |
|---|---|---|
| Resource | **one per UID**: master + exception rows (+ orphan exceptions) in one `.ics` | E08 MUST 9, §6.4; RFC 4791 §4.1. All three bridges converged on this after per-row resources broke clients: `caloxide/caldav/series.go:13-24`, `carbonate/internal/caldav/backend.go:411-415`, `cal-gateway/internal/caldav/ics.go:35-42` ("Error 2") |
| href | `<homeset>/<calseg>/<name>.ics`. calseg: opaque, stable, URL-safe from the Proton calendar ID (carbonate: sha256 half, `carbonate/internal/caldav/backend.go:204-208`). name: the UID itself if it is in `[A-Za-z0-9._-]` and short; else a reversible prefixed encoding (e.g. `~`+base64url) | the UID branch equals TB's own naming for the events it creates (E08 §4.4), so M4 hrefs round-trip. caloxide does the same with `enc.`+hex (`caloxide/caldav/caldav.go:286-337`). Avoid per-row-ID hrefs (cal-gateway `backend.go:1415`) because a re-created master would change the href |
| Tombstone → href | **add `uid` to `tombstones`** (schema v2) or keep an id→uid map in the same DB. carbonate kept it in memory and, after a restart, cannot name deletions (`carbonate/internal/caldav/sync.go:29-57,111-118`) | needed to emit a per-UID 404 |
| ETag (resource) | `"v1-" + hex32(sha256(render version ‖ sorted (row ID, row ETag) ‖ every non-row render input))`. The non-row inputs: the calendar's default notifications if `null` Notifications are expanded; the calendar colour if injected | the row ETag already covers the whole raw row (SPEC §8.2). Folding on LastEditTime alone (carbonate `backend.go:454-473`, cal-gateway `backend.go:1430-1444`) misses edits that do not move it. A default-reminder change does not touch rows but changes the output |
| CTag | `epoch:max(seq of the calendar's rows and tombstones):settingsHash` (index on `(calendar_id, seq)` exists) | cheap, no Proton call (carbonate calls Proton `modelevents/latest` on every PROPFIND, `carbonate/internal/caldav/backend.go:143-172`); TB uses it only without sync (E08 §3.1) |
| sync-token | `urn:pcb:sync:1:<epoch>:<seq>` (URI per RFC 6578). Valid iff same epoch and `seq ≤ current` and `seq ≥ oldest retained tombstone`. Otherwise **403 + `<D:error><D:valid-sync-token/></D:error>`** | TB resets the token and does a full sync on 4xx with a stored token (E08 §3.3). Never send the exact body `<D:error xmlns:D="DAV:"/>` with 403 (read as rate limit) |
| Empty token | 207: every resource (href, getetag, getcontenttype `text/calendar; charset=utf-8; component=vevent`) + `<sync-token>` | TB deletes every local item not listed (E08 §3.2 "full-sync semantic") |
| Delta | in **one SQLite read transaction**, set U = UIDs of rows with seq > t ∪ UIDs of tombstones with seq > t (same calendar). For each UID: any live row → 200 response with the new fold ETag; none → `<response><href>…</href><status>HTTP/1.1 404 Not Found</status></response>` (no propstat). New token = the seq read in that same transaction | E08 §3.2, MUST 7 (response-level 404; MOCK shape). Deleting an *exception* row is a **change** to the UID resource, not a 404 |
| Paging | optional 507 element + intermediate token (TB re-issues) | E08 §3.2, MOCK pages at 3 |
| multiget | per href: 200 with getetag + calendar-data, or response-level 404. **One undecryptable/unparsable resource must not fail the batch**: serve what decrypted (proton-cal leniency), never a 500 propstat | E08 §3.4; go-webdav's encoder failure mode (A.3) |
| PROPFIND Depth 1 / ctag | also serve them correctly (fallback path, other clients, tests) | E08 SHOULD |

Reference bridges:
- carbonate's sync **answers a resync as a 207 full listing in place of a delta** (`carbonate/internal/caldav/sync.go:71-75,117,126-153`). Its XML ignores the `Resync` flag (`:165-205`). A client holding a token treats that listing as a delta, so items deleted meanwhile **stay forever** in TB, which only drops unlisted items when its token is null (E08 §3.2) [inferred from code + E08]. The right answer is the 403 `valid-sync-token`.
- carbonate omits `getcontenttype` in sync responses; TB accepts that only because hrefs end in `.ics` (E08 §3.2 iCloud workaround).
- carbonate uses Proton's `modelevents/latest` as the token [claim: carbonate live, `carbonate/docs/ARCHITECTURE.md:166-185`]. caloxide and cal-gateway have no sync-collection.

---

## D. Auth and transport

| Bridge | CalDAV auth | Credential storage | TLS | Bind |
|---|---|---|---|---|
| caloxide | Basic; password = base64 of a 32-byte random key. That key also decrypts the sealed session (`auth.json`). Checked by bcrypt (DefaultCost) against an **in-memory** hash | key never stored; bcrypt hash in RAM (`caloxide/auth/auth.go:330-392`) | cert/key file + optional client CA = mTLS (`caloxide/config/tls.go:10-42`), **but the CalDAV listener calls `ListenAndServe()`, not `…TLS`**, so CalDAV is always plain HTTP even when TLS is configured (`caloxide/cmd/caloxide/main.go:272-274,348-349` vs CardDAV `:394`) [source] | default `127.0.0.1:8081` (`main.go:455-456`) |
| carbonate | Basic, one username + one "bridge password" (24 random bytes, base64url, `carbonate/internal/session/session.go:89-96`), also the scrypt key of the session file; `subtle.ConstantTimeCompare` on raw strings (leaks length, `carbonate/internal/server/server.go:178-197`) | in RAM, from env `CARBONATE_BRIDGE_PASSWORD` or a prompt; **printed to the terminal at login** (`carbonate/cmd/carbonate/main.go:91,175-190`) | **none** | `127.0.0.1:8080`; `-addr` is labelled "localhost only" but **not enforced** (`main.go:250,257`; `server.go:91`) |
| cal-gateway | Basic, one static user + password from config; constant time over sha256 digests (no length leak); logs failures with the real IP for fail2ban (`cal-gateway/internal/server/server.go:238-280`) | plaintext in the config file | none; nginx expected in front (`cal-gateway/DEPLOYMENT.md:178`); **warns, does not refuse**, on a non-loopback bind (`server.go:80-106`) | config |

Other security-relevant mistakes:
- caloxide's `handlers` map is read and written from concurrent requests without a lock (`caloxide/cmd/caloxide/main.go:270,329-342`): a data race [inferred].
- caloxide returns `err.Error()` to the client on auth errors (`main.go:325`).
- go-webdav copies backend errors into `responsedescription` (`go-webdav/internal/elements.go:136-142`).
- carbonate reads REPORT bodies with an unbounded `io.ReadAll` (`carbonate/internal/caldav/compat.go:42`) and so does go-webdav (cal-gateway fixed it with `MaxBytesHandler`).
- carbonate's `CARBONATE_DEBUG=1` dumps access tokens (`carbonate/docs/ARCHITECTURE.md:361-363`).
- cal-gateway advertises `calendar-auto-schedule` (`scheduling.go:49`). TB then **stops emailing invitations itself** and registers free-busy (E08 §5.1). Its outbox always answers "everyone free" (`cal-gateway/docs/FEATURE-MATRIX.md:93-101`): wrong availability shown.
- Privileges hard-coded writable (A.2): in TB's cached mode, a write to a read-only Proton calendar fails silently (E08 §3.8).
- No bridge verifies signatures on read, except caloxide (best effort) (B.4).
- None hashes per-client credentials at rest. None generates a self-signed cert.

Recommendations (for SPEC §9/§19):
- **App passwords**:
  - `pcb client add <name>` generates ≥ 128 bits from crypto/rand (e.g. 24 bytes base64url), shown once.
  - Store `name, id, sha256(salt‖pw)` or HMAC-SHA256 under a key from the sealed store, never the password.
  - Compare digests in constant time, revoke per client, record last use.
  - A slow KDF (bcrypt/argon2) buys nothing for a random 128-bit secret and costs ~50–100 ms on every request [inferred], since TB authenticates each request.
  - Username: accept any value, or the account email (TB's Account Hub sends the email, E08 §1.1).
  - 401 with `WWW-Authenticate: Basic realm="pcb", charset="UTF-8"`, never 403 (E08 MUST 1).
  - Rate-limit failures per IP.
- **Transport**:
  - Loopback: plain HTTP allowed (TB accepts `http://localhost:PORT/` typed with the scheme, E08 §2.3).
  - **Non-loopback bind refuses to start without TLS** (stricter than cal-gateway's warning). Also check at bind time, after resolving the address, not only from the flag text (carbonate's gap).
- **Self-signed cert**:
  - Generate once: ECDSA P-256, SANs = configured hostnames + bind IPs, key 0600 in the state dir. Persist and reuse it: regenerating triggers TB's exception dialog again (E08 §2.3, per-host exception).
  - `pcb doctor` / `pcb client add` print the SHA-256 fingerprint so the user can compare it in TB's exception dialog.
  - TLS ≥ 1.2.
  - Also accept a user cert/key (e.g. from a reverse proxy or ACME).
- **Hardening**: `MaxBytesHandler` (≤ 8 MiB); `ReadHeaderTimeout`; no error text in bodies or `responsedescription`; never advertise scheduling.

---

## What still needs a live Thunderbird check
1. Detection with our 401 → Basic flow on `/.well-known/caldav` (308 vs 301), on `/` and on a principal URL; whether necko then sends credentials preemptively (latency, auth cost) — E08 "not determined".
2. sync-collection end to end: empty token full sync, delta with exception-row change, UID 404 deletion, 403 `valid-sync-token` reset after a cache epoch change, 507 paging with a large calendar.
3. multiget with encoded UIDs (`~`/base64url names, `%40` in UIDs): decoded-path equality (E08 §3.4).
4. **`readOnly` persistence**: if M3 serves read-only privileges, does TB clear the flag when M4 starts sending `write`? E08 §5 infers it is never cleared, which would force users to re-subscribe or untick "read-only" at M4.
5. Self-signed cert over the network: does the creation dialog offer the exception (CertError path, E08 §2.3) or must the user add it first under Settings → Certificates? Behaviour on an IP literal SAN; certificate validity length accepted.
6. Real Proton data: non-IANA TZIDs from imported events (TB alert, E08 §6.3); orphan exception groups (`X-MOZ-FAKED-MASTER`); duplicate masters; malformed fragments (caloxide `sanitizeICS` cases).
7. VALARMs from `Notifications` (defaults, EMAIL vs DISPLAY): shown and fired once; no re-fire after a refresh.
8. First-sync time and memory for thousands of events (E09 decrypt cost, LRU sizing); TB's startup refresh of all calendars.
9. `getcontenttype` with `;component=vevent`; calendar name/colour only read at subscribe (rename does not propagate, E08 §1.3).
10. Shared calendars of another member: signature verification against Author keys, and AddressKeyPacket-keyed invites decrypt (cal-gateway claim).

## Recommended M3 architecture (≤ 15 lines)
1. `internal/caldav`: our own `net/http` handler with encoding/xml types we own; routes `/`, `/.well-known/caldav` (308), `/dav/principals/<u>/`, `/dav/calendars/<u>/`, `/dav/calendars/<u>/<calseg>/`, `…/<name>.ics`. No go-webdav at runtime.
2. Methods: OPTIONS, PROPFIND (Depth 0/1), REPORT sync-collection + calendar-multiget, GET/HEAD. PUT/DELETE/PROPPATCH/MKCALENDAR → 403/405 until M4. `DAV: 1, 3, calendar-access` only.
3. Read model: cache rows → per-UID groups (master, then exceptions by RecurrenceID; orphans kept) → decrypt on demand (calendar key; AddressKeyPacket first when present; GetBinary) → line-level merge (proton-cal rules + X-PM-SESSION-KEY strip) + row `Color`/`Notifications` (tri-state, calendar defaults) → VCALENDAR without VTIMEZONE, CRLF-terminated, PRODID `-//pcb//EN`.
4. In-memory LRU of rendered resources keyed by the fold ETag; nothing decrypted written to disk (SPEC §7.2).
5. Signature verification against the Author's keys, never blocking a read; counters in `doctor`.
6. Cache schema v2: `tombstones.uid`; per-calendar `max(seq)` query; resource ETag = sha256 fold of (render version, row IDs+ETags, calendar settings used).
7. CTag `epoch:seq:settingsHash`; sync-token `urn:pcb:sync:1:<epoch>:<seq>`; deltas and the token read in one SQLite transaction; bad token → 403 `valid-sync-token`.
8. Privileges: read-only `<privilege><read/></privilege>` for subscribed/holiday/view-only calendars. For owned calendars, decide now between read-only in M3 and write in M4 (live check 4) [call].
9. Auth middleware: Basic against hashed per-client app passwords from the sealed store; constant time; 401 challenge; rate limit.
10. Listener: loopback HTTP by default; any other bind requires TLS (generated persistent self-signed or user cert); `MaxBytesHandler`, timeouts, no error text in responses.
11. Tests: E08 MUST/AVOID as table-driven contract tests with golden XML shapes (TB MOCK as reference), go-webdav `caldav.Client` as a second independent client, fuzzing on the XML and iCalendar parsers.
