# E08 — What Thunderbird's CalDAV provider sends and expects

Status: **source-verified; implemented in M3 and covered by contract tests ("What the bridge does (M3)"); live Thunderbird session pending (HUMAN-CHECKS step 6, after M4, ruling M3-R1)**. Feeds the M3/M4 CalDAV adapter (A1), SPEC §9, E11 (SPEC §17).

## Question

What does Thunderbird's CalDAV provider send (discovery, authentication, sync, writes, free-busy) and what does it require or choke on in the server's answers, so that the bridge's CalDAV adapter can be built to it?

## Method

Read Thunderbird's source: mozilla/releases-comm-central (GitHub mirror), default branch, commit `921a58758f766e0a76d50e94d6fc1d15eae3596f` (2026-11-10), licence MPL-2.0. Read only: nothing was executed and no code was copied (ruling R8; decision 0002). This replaces the planned proxy capture of a real Thunderbird against Radicale (SPEC §17): the live Thunderbird session is a deferred human check (`spec/HUMAN-CHECKS.md`, last item), and scripted CalDAV clients in CI cover the rest.

Paths are relative to the repo root of that commit. Abbreviations:
- `PROV` = `calendar/providers/caldav/CalDavProvider.sys.mjs`
- `CAL` = `calendar/providers/caldav/CalDavCalendar.sys.mjs`
- `REQ` = `calendar/providers/caldav/modules/CalDavRequest.sys.mjs`
- `HND` = `calendar/providers/caldav/modules/CalDavRequestHandlers.sys.mjs`
- `SES` = `calendar/providers/caldav/modules/CalDavSession.sys.mjs`
- `UTL` = `calendar/providers/caldav/modules/CalDavUtils.sys.mjs`
- `PU` = `calendar/base/modules/utils/calProviderUtils.sys.mjs`
- `AU` = `calendar/base/modules/utils/calAuthUtils.sys.mjs`
- `DET` = `calendar/base/modules/utils/calProviderDetectionUtils.sys.mjs`
- `MOCK` = `calendar/test/CalDAVServer.sys.mjs` (Thunderbird's own mock CalDAV server used in its tests; a ready-made reference for payload shapes)

"inferred" = not read in comm-central. Usually the behaviour lives in Gecko/necko, which is not in this repo. Line numbers are those of the commit above and drift with it.

## Results

Headline facts that shape the server design:
- Thunderbird NEVER sends `calendar-query` (no time-range filter), `free-busy-query`, MKCALENDAR, PROPPATCH or MKCOL. Verified by grep: the only hits for these strings are in unrelated UI code.
- The calendar is read as a whole: either `sync-collection` (if advertised) or `PROPFIND Depth:1`, then `calendar-multiget` for changed items.
- It never reads `calendar-description`, `calendar-order`, `calendar-timezone`, `max-resource-size` or `getlastmodified`. Verified by grep.

### 1. Discovery

#### 1.1 Entry points
- Calendar creation dialog (`calendar/base/content/dialogs/calendar-creation.js`) and the Account Hub (`mail/components/accountcreation/views/email.mjs`) both call `cal.provider.detection.detect(...)`.
- That calls `CalDavProvider.detectCalendars(username, password, location, savePassword)` (`PROV` lines 57-125).
- Account Hub, `#getCalendars` (email.mjs ~2228): location = `https://<hostname>`. Hostnames are the email domain, plus the incoming mail host if it is in the same eTLD+1. Username = full email. Password = the mail account password.
- `locationToUri` (`DET` 103-118):
  - Empty location: `https://` + domain part of the username.
  - Location containing `://`: used as given. A plain `http://localhost:8080/...` is accepted.
  - Bare hostname: `https://<host>`.

#### 1.2 Detection method order (`PROV` 69-75)
`attemptGoogleOauth`, `attemptLocation`, `dnsSRV`, `wellKnown`, `attemptRoot`. The first non-null result wins.
- `attemptGoogleOauth` (250-270) only fires for gmail.com, googlemail.com or an MX record at `*google.com`. It does an MX lookup first, which is harmless for us.
- `attemptLocation` (151-158):
  - Skipped when the path is `/`.
  - Otherwise `detectCollection(location)`.
- `dnsSRV` (166-219):
  - Only when the path is `/`.
  - Looks up SRV `_caldavs._tcp.<host>` (https) or `_caldav._tcp.<host>` (http).
  - Sorted by priority, then weight. Only the first record is used.
  - TXT on the same name, key `path=...`, gives the context path.
  - Tries `detectCollection` at `scheme://srvhost:port<path>`. If that fails, tries `scheme://srvhost:port/.well-known/caldav`.
  - The SRV target host is added to the set of sites that may receive the password.
- `wellKnown` (227-231): `detectCollection(/.well-known/caldav)` resolved against the location (always host root).
- `attemptRoot` (239-242): `detectCollection(/)`.

#### 1.3 `detectCollection` (`PROV` 279-329)
Request: `PROPFIND <url>`, `Depth: 0`, `Content-Type: text/xml; charset=utf-8`. Body (REQ `CalDavPropfindRequest`, 739-771):
```
<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D='DAV:' xmlns:A='http://apple.com/ns/ical/' xmlns:C='urn:ietf:params:xml:ns:caldav'><D:prop>
 <D:resourcetype/><D:owner/><D:displayname/><D:current-user-principal/><D:current-user-privilege-set/>
 <A:calendar-color/><C:calendar-home-set/></D:prop></D:propfind>
```
Common headers on every request (`PU.prepHttpChannel`, 108-110): `Accept: text/xml`, `Accept-Charset: utf-8,*;q=0.1`, cache bypass.

Decision tree on the first response (`firstProps` = first `<response>` in the multistatus, whatever its href):
1. HTTP 401 or 403: `AuthFailedError`. This is thrown out of `detectCalendars`. Detection stops immediately and no later method is tried (`PROV` 96-99, 101-105 and 297-298).
2. Any other non-207, or an unparseable body: returns null and the next method is tried (`PropfindResponse.ok` = status 207 && xml, REQ 922-924).
3. `D:resourcetype` contains `C:calendar`: that URL is the calendar. The response MUST contain a `resourcetype` element. If the prop is absent, `.has` on undefined throws a TypeError, the method counts as failed and detection moves on.
4. Else `C:calendar-home-set` present (`href` children): `handleHomeSet(href)`.
5. Else `D:current-user-principal`: `handlePrincipal` (PROPFIND Depth 0 for `C:calendar-home-set`), then `handleHomeSet`.
6. Else `D:owner`: same as 5.
7. Else null.

`handleHomeSet` (377-406): `PROPFIND Depth:1` on the home set, props `resourcetype, displayname, current-user-privilege-set, A:calendar-color`.
- Every `<response>` whose `resourcetype` contains `C:calendar` becomes a calendar. Non-calendar collections (the home set itself, inbox/outbox) are skipped.
- Every response MUST carry a `resourcetype` element (it may be empty). A response without one throws.
- Detection does NOT check `supported-calendar-component-set`. Any collection typed `C:calendar` is offered.
- Hrefs (relative or absolute) are resolved against the final response URI, so redirects are handled.

`handleCalendar` (416-443):
- Name = `displayname`. Fallback = last non-empty path segment of the URL.
- Color = `A:calendar-color`. An 8-digit `#RRGGBBAA` has the alpha stripped. Fallback = hash colour of the URL.
- `readOnly` is set from `current-user-privilege-set`: calendar is read-only unless the set contains one of `D:write`, `D:write-content`, `D:bind`, `D:all`. If the prop is missing or in a 404 propstat (`privSet` returns null, REQ 836-838), nothing is set (writable).
- Name and colour are read ONLY at detection/subscribe time. The later sync-time PROPFIND does not request them, so server-side renames or recolours never propagate (inferred from the absence of any later request for them).

#### 1.4 Resource-is-a-calendar rule
- Detection: `resourcetype` contains `{urn:ietf:params:xml:ns:caldav}calendar` (matched through `CalDavNsUnresolver`, `UTL` lines 46-54 of the second file block, i.e. the `C:` prefix).
- Per-sync check, `checkDavResourceType` (CAL 1574-1740):
  - `calendar` in resourcetype: OK.
  - `collection` only: error `DAV_DAV_NOT_CALDAV`.
  - Otherwise: error `DAV_NOT_DAV`.

#### 1.5 401 / redirects during detection
- Redirects are handled by `CalDavRequestBase.asyncOnChannelRedirect` (REQ 175-246):
  - Followed by necko (301, 302, 303, 307, 308).
  - Forces `requestMethod` back to the original method (PROPFIND) and re-sends the upload stream. This works even for 302/303, which necko would normally turn into GET.
  - Copies headers `Depth, Originator, Recipient, If-None-Match, If-Match, Accept`. Copies `Authorization` only if the new prePath (scheme+host+port) equals the old one.
  - If an `Authorization` header was already set and the redirect goes to a different origin, the request is ABORTED (`NS_ERROR_ABORT`).
- A well-known to same-origin 301/302/307/308 redirect works. MOCK does exactly this: `/.well-known/caldav` 301 to `/principals/`, and `/principals/` answers `current-user-principal`.
- The final URI is `response.uri`. All relative hrefs are resolved against it.
- `/.well-known/caldav` may answer 401 (needs a challenge) or redirect. It must NOT answer 403 for an unknown/unauthenticated case: 403 is treated as auth failure and aborts detection (`CalDavResponseBase.authError`, REQ 312-315).
- 404 or 405 on well-known: falls through to `attemptRoot` (`PROPFIND /`), which must then return `current-user-principal` or `calendar-home-set`.

#### 1.6 Later (per-calendar) server-info chain (`CAL` 1566-1570 header comment)
Run at first refresh, and again whenever the calendar was disabled by an error:
1. `checkDavResourceType`: `PROPFIND` Depth 0 on the calendar URL (always with trailing slash; `makeUri()` adds it). Props: `resourcetype, owner, current-user-principal, current-user-privilege-set, supported-report-set, C:supported-calendar-component-set, CS:getctag`.
2. `checkServerCaps`: `OPTIONS` on the PARENT of the calendar URL (`setCalHomeSet(true)` strips the last path segment). On 404 it retries `OPTIONS` on the calendar URL itself (CAL 1763-1771). Requires a 2xx response and reads the `DAV:` header. Any other status (401, 403, 405...) gives `completeCheckServerInfo(NS_ERROR_FAILURE)`, which reports nothing and leaves the calendar flagged disabled-by-error: it silently never syncs.
3. Only if `DAV:` advertises `calendar-auto-schedule` or `calendar-schedule`: `findPrincipalNS` and `checkPrincipalsNameSpace` (section 5).
4. `completeCheckServerInfo` then `refresh()`.


### 2. Authentication and TLS

#### 2.1 Basic / Digest / Negotiate (necko) vs OAuth
- All HTTP auth is delegated to necko through `nsIAuthPrompt2`. The code does not set `Authorization` itself, except for OAuth bearer tokens. So no preemptive Basic is sent from Thunderbird code. Necko answers the 401 challenge (`WWW-Authenticate`). Whether necko later sends credentials preemptively on the same path is inferred.
- The server must therefore send `401` plus `WWW-Authenticate: Basic realm="..."` (or Digest) on unauthenticated requests. MOCK does `Basic realm="test"` (`MOCK` 128-159).
- During detection (`CalDavDetectionSession.promptAuth`, SES 163-199):
  - Credentials are supplied only for hosts in `passwordSites` (eTLD+1 of the location, or the SRV target). An `http` entry also permits `https` on the same site.
  - On `PREVIOUS_FAILED` it returns false, which gives a 401 and `AuthFailedError`.
  - Proxy challenges are ignored.
- Normal operation (`AU` `Prompt`, calAuthUtils lines ~120-330):
  - Looks up Login Manager by `origin=prePath` and `httpRealm=realm` plus the calendar's `username` property.
  - If found, it is used silently.
  - If not found, it opens the standard auth dialog with a "remember password" checkbox.
  - A saved login that the server rejects (`PREVIOUS_FAILED`) is deleted and the user is prompted again.
  - One password per server+realm (`calendar.network.multirealm` pref = false, `calendar/base/calendar.js` line 144).
- OAuth (`SES` `CalDavSession.prepareRequest`, 56-90):
  - Only if no saved Login Manager entry exists for the origin and username, AND `OAuth2Module.initFromHostname(host, username, "caldav")` recognises the host.
  - That is only known OAuth providers (Google etc.).
  - The comment mentions Fastmail app passwords: a saved login wins over OAuth.
  - A generic self-hosted server never triggers OAuth.
  - `Authorization: Bearer <token>` is set explicitly, and only for the origin the token was issued for.
- Digest special case (`CAL.safeRefresh`, 1346-1362): before every refresh, if the stored `mAuthScheme == "Digest"`, it does a SYNCHRONOUS `OPTIONS` on the calendar URL and requires status exactly 200, else the refresh is reported as failed. Scheme is derived from the `Authorization` request header of the first PROPFIND (CAL 1620-1634); the realm is parsed with `wwwauth.split('realm="')[1].split('", ')[0]`. Basic is simpler and avoids this. If using Digest, `OPTIONS` must return 200 and the realm must be quoted followed by `", "`.

#### 2.2 401 mid-sync
- On the calendar's first PROPFIND (`checkDavResourceType`): any 4xx (`response.clientError`, incl. 401 after the user cancels or the password fails) sets `disabled=true` and `auto-enabled=true` on the calendar and aborts (CAL 1600-1606). The user must re-enable it.
- On sync-collection with a stored token: any 4xx other than the two exceptions below resets the token and triggers a full resync (HND 545-557). With no stored token it is a hard DAV_REPORT_ERROR and the calendar is flagged disabled-by-error (HND 558-564).
- Multiget non-207 results in `notifyGetFailed`. Etags handler non-207 results in `DAV_REPORT_ERROR`.
- Write failures (PUT/DELETE) are covered in section 4.

#### 2.3 TLS
- On a TLS error (`CalDavSimpleResponse.onStreamComplete`, REQ 477-498) `cal.provider.checkBadCertStatus` runs (PU 291-312).
- For a bad-cert error class it opens Thunderbird's certificate exception dialog `chrome://pippki/content/exceptionDialog.xhtml` with `prefetchCert: true`. A 5 s per-host dedupe applies (PU 198-281).
- If the user adds the exception, the calendar is refreshed.
- During detection a bad cert surfaces as `CertError` in the UI (PROV 107-111).
- Self-signed certificates therefore work through a user-confirmed exception. Nothing is automatic.
- Plain `http`: no scheme check exists anywhere in the CalDAV code. `http://localhost:PORT/...` is accepted when entered with the explicit scheme. The Thunderbird test suite runs its mock server on `http://localhost:<port>` (`MOCK` 119-121). Whether necko applies a secure-context or upgrade policy is inferred as "no".
- A bare hostname without a scheme always becomes `https://` (`DET` 110-116).
- Password-save in the detection path is restricted to the entered site (see 2.1).


### 3. Sync

#### 3.1 Which mechanism
Decided in `checkDavResourceType` (CAL 1650-1668):
- If the calendar's depth-0 PROPFIND returns `D:supported-report-set` containing `<D:sync-collection/>` (`{DAV:}supported-report/report/sync-collection`), `mHasWebdavSyncSupport = true`. This is the ONLY condition. The `DAV:sync-token` property is not requested or checked at all.
- Else if `CS:getctag` is returned, ctag polling is used.
- Else it relies on a full listing every refresh.

`safeRefresh` (CAL 1321-1429) then:
- With sync support: every refresh goes directly to `getUpdatedItems`, which runs `sync-collection` (no ctag check; see the comment at 1364-1366).
- Without sync support:
  - `PROPFIND` Depth 0 `CS:getctag` on the calendar.
  - Same ctag: nothing is fetched.
  - Different or missing ctag: full etag listing.
  - 404: calendar flagged unavailable. Non-207: no change.

#### 3.2 sync-collection (`HND.CalDavWebDavSyncHandler.doWebDAVSync`, 434-490)
Request: `REPORT <calendar url>`, headers `Depth: 1` (both Depth and `sync-level` are sent on purpose, "some servers still depend on the depth header"), `Content-Type: text/xml; charset=utf-8`:
```
<?xml version="1.0" encoding="UTF-8"?>
<sync-collection xmlns="DAV:"><sync-token/>  | <sync-token>TOKEN</sync-token>
<sync-level>1</sync-level><prop><getcontenttype/><getetag/></prop></sync-collection>
```
- First sync sends an empty `<sync-token/>`.
- Token is stored verbatim (XML-escaped when sent back). It is persisted only after the follow-up multiget finishes (HND 618-646, 972-978).

Response parsing (HND 648-806), per `<response>`:
- Collected fields: `href`, `getcontenttype`, `getetag` (anywhere in the response, including nested propstat), `status`, `propstat/status`, and `sync-token` (anywhere).
- Content type: missing or `text/plain` AND href ends `.ics`: treated as `text/calendar` (iCloud / iCal Server workaround).
- Deleted: response-level `<status>` containing ` 404` (index > 0). Local item removed. MOCK's deleted shape: `<response><status>HTTP/1.1 404 Not Found</status><href>..</href><propstat><prop/><status>HTTP/1.1 418 ...</status></propstat></response>`. A response-level 404 is the right form.
- Created/updated: `getcontenttype` starting `text/calendar`, non-empty `getetag`, non-empty href. A status check exists but is effectively always true because of an `indexOf`-truthiness bug: any status on such a response is accepted (inferred from code reading).
- Needs fetching iff href unknown locally or etag differs from the cached one.
- Status 507 on a response: sets `additionalSyncNeeded` and processing continues. After the multiget it re-issues sync-collection with the new token. Use this for paging; MOCK pages at 3 items and puts the intermediate token at the end (`MOCK` 411-478).
- Directory entries ending in `/` with status 200 are ignored (iCloud).
- An unrecognised `<response>` that either has `text/calendar` content type or a status not containing ` 404` counts as an "unhandled error" and aborts the sync (HND 760-773, 590-598). Do not return odd status elements for non-calendar entries.
- Whitespace trap: leaf text is NOT trimmed (`_walk` -> `characters(textContent)`), so `<D:href> /x </D:href>` or pretty-printed leaves corrupt href/etag/token matching. Emit leaf values with no surrounding whitespace. (MOCK strips inter-tag whitespace with `.replace(/>\s+</g, "><")`.)
- Full-sync semantic: when the stored token is null (first sync or reset) and no 507 paging is pending, every locally cached item NOT reported is deleted (HND 600-614). On a full sync the server must therefore list every resource.

#### 3.3 Invalid or expired sync-token (`HND` 535-564)
Order of checks when the status is not 207:
1. `403` whose body contains EXACTLY the string `<D:error xmlns:D="DAV:"/>`, or `429`: treated as rate limit. Do not refresh, token kept, no error shown.
2. Else if a token is stored and status is 4xx (including 403 and 409): token reset to null, saved, `safeRefresh` runs again. This is a full resync with an empty `<sync-token/>`. A standard `403/409` with `<D:error><D:valid-sync-token/></D:error>` works: it just triggers a full resync. The body is not inspected.
3. Else (token null, or 5xx): `DAV_REPORT_ERROR`. The calendar is flagged disabled-by-error and an error is shown.
- So the server must accept an empty `<sync-token/>` and return a full listing plus a fresh token. A 4xx to the initial empty-token request is fatal for the calendar.
- 5xx never resets the token.

#### 3.4 Fetching items: `calendar-multiget`, not calendar-query
`CalDavMultigetSyncHandler.doMultiGet` (HND 862-915):
- `REPORT <calendar url>`, `Depth: 1`, body:
```
<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop>
<D:href>/path/encoded.ics</D:href>...</C:calendar-multiget>
```
- Batch = 100 hrefs (pref `calendar.caldav.multigetBatchSize`, default 100). Loops until all fetched. Hrefs are popped from the end of the list.
- Hrefs are PATH-ONLY, percent-encoded per segment with `encodeURIComponent` (so `@`, `:` etc. become `%40`, `%3A`).
- Response parsing requires per `<response>`: `href`, non-empty `getetag` AND non-empty `calendar-data` (leaf text, CDATA or escaped both fine). A response with response-level ` 404` status deletes the local item. Anything else (e.g. propstat 404 with no calendar-data) is "unexpected response" and fails the WHOLE multiget (`unhandledErrors`, HND 967-971 and 1125-1135). Return 404 per missing href at the response level, not as an empty propstat.
- Any non-207 multiget response fails the sync cycle.
- Hrefs in the response are URL-decoded (`ensureDecodedPath`) and compared with the decoded hrefs from the listing, so encoding style is free as long as decoded paths match.
- The number of leading path segments equal to the request URI's depth is stripped to derive the item's "location path" (CAL 992-998). Item resources must live directly beneath the calendar collection URL.
- Item with an unchanged etag is skipped (`skipping item with unmodified etag`) unless a listener is waiting (after a PUT, see 4.4). So the ETag MUST change on every content change, and be identical between the sync listing and the multiget.
- An iCalendar body that fails to parse is logged and skipped. The resource with several UIDs: only `items[0]` is used (CAL 965-980). Keep one UID per resource (RFC 4791 requires it anyway).

#### 3.5 No-sync-collection fallback (`CAL.getUpdatedItems` 1466-1514; `HND.CalDavEtagsHandler`)
- `PROPFIND` `Depth: 1` on the calendar. Body props: `getcontenttype, resourcetype, getetag`.
- Resource is considered only if: non-collection, non-empty `getetag`, non-empty `getcontenttype` starting `text/calendar`. Workarounds: `message/rfc822` (Scalix) and `text/vtodo` (Kerio) are remapped to `text/calendar`. In this path there is NO `.ics` suffix fallback: a missing content type means the item is ignored.
- Local items absent from the listing are deleted. Items with new or unknown etags are fetched with multiget.
- ctag compared before each cycle (`CS:getctag`, namespace `http://calendarserver.org/ns/`).

#### 3.6 Time-range filters
None. Thunderbird syncs the entire collection (sync-collection or full listing), stores it in an in-memory or local SQLite cache and filters locally. No `calendar-query`, no `time-range`. Large calendars mean large first syncs.

#### 3.7 Polling interval defaults
- Per-calendar property `refreshInterval` in minutes, default 30 when unset (`calendar/base/src/CalCalendarManager.sys.mjs` 480-500).
- UI options: 1, 5, 15, 30, 60 min, or manual (0) (`calendar-properties-dialog.js` 225-241).
- `nsITimer.TYPE_REPEATING_SLACK`.
- Startup refresh happens for every calendar. With offline cache enabled it is delayed by 15 s (CalCalendarManager 685-690).
- Refresh also fires on going online (`CalCachedCalendar.onOfflineStatusChanged`) and after a certificate exception is added.
- With sync-collection support each poll is one `REPORT` (plus multiget if changes). Without, one depth-0 `PROPFIND` for ctag.
- No server push or polling hints are consumed.

#### 3.8 Cache
`cache.enabled` is checked by default in the creation dialog (`calendar-creation.xhtml` 189-195). With caching, writes are applied locally and replayed to the server asynchronously (`CalCachedCalendar.playbackOfflineItems`). Playback failures are only logged and retried on the next refresh. The user sees no error for a rejected PUT on a cached calendar.


### 4. Writes

#### 4.1 PUT (`CAL.doAdoptItem` 593-689, `doModifyItem` 735-815, `REQ.CalDavItemRequest` 645-693)
- Method PUT, `Content-Type: text/calendar; charset=utf-8`, body = a full VCALENDAR serialised by Thunderbird (see section 6). Also `Accept: text/xml`.
- Create: `If-None-Match: *`. Modify: `If-Match: <etag as last stored>`. The etag is the text from the last multiget `getetag`, sent as stored (including quotes). If the etag is unknown, no precondition header is sent (a "force" overwrite).
- Success statuses (`ItemResponse.ok`, REQ 687-692): 200, 201 or 204. The code says 201 on modify "indicates a server error ... convenient to accept it for now".
- After a successful PUT, Thunderbird IGNORES the response `ETag` header and instead immediately re-fetches the item with `calendar-multiget` for its href (comment CAL 642-646: "Some CalDAV servers will modify items on PUT"). So the server's multiget must return the new etag and the stored data.
- Also on create, the location path is derived from `response.uri` (the final URL after redirects).

#### 4.2 Error handling on PUT
Create (`doAdoptItem`):
- 5xx: `NS_ERROR_NOT_AVAILABLE`, detail `Server Replied with <status>`.
- UID conflict (`uidConflict`, REQ 333-350): 409 or 412 (any), or 4xx with a `{caldav}no-uid-conflict` element in the body, or a bare 400 whose text contains `uid already exists`. The result is `UID_CONFLICT`: the cached calendar remembers the item and, after the next sync, transfers the user's change onto the server copy (`CalCachedCalendar.applyPendingUidConflicts`, line ~560).
- Any other status (e.g. 403, 400, 415, 507): `NS_ERROR_FAILURE`, detail `Server Replied with <status>`. For uncached calendars `reportDavError(DAV_PUT_ERROR)` shows a localised "item-put-error" with the status code and its generic text, and marks the calendar disabled-by-error. The server's response BODY / error text is not displayed anywhere. For cached calendars it is a silent retry loop.
- MOCK style variants of the uid conflict that Thunderbird has been hardened for: Radicale 409 + `<C:no-uid-conflict/>`, SOGo 409 + plain `D:error` text, Sabre/Nextcloud 400 + sabre message, 409 with `no-uid-conflict` containing `D:href` (RFC 4791 5.3.2), 412 when a resource already exists at the URL (`MOCK` 663-713).

Modify (`doModifyItem`):
- 409 or 412 (`conflict`): `promptOverwrite` opens the "calendar-conflicts-dialog" (`PU.promptOverwrite`, 426-444). User choice: overwrite (re-PUT without `If-Match`) or discard (re-fetch with `getUpdatedItem`).
- 5xx: `NS_ERROR_NOT_AVAILABLE`. Other statuses: `NS_ERROR_FAILURE`, same reporting as above.
- 401/403 on PUT are not special-cased: 401 is handled by necko prompting. 403 is a plain failure.
- Every modify increments `X-MOZ-GENERATION` before the PUT (`aNewItem.generation += 1`).

#### 4.3 DELETE (`CAL.doDeleteItem` 838-938, `REQ.CalDavDeleteItemRequest` 698-734)
- `DELETE <href>` with `If-Match: <etag>` when known, none when forced.
- Treated as success: 200, 204, 404 (already gone). 201 etc. are not.
- 409 or 412: follow-up `HEAD <href>`:
  - 404: success (someone else deleted it).
  - 5xx: error.
  - Any other status (including 405 if HEAD is unsupported): user is asked via the conflicts dialog to force-delete (DELETE without `If-Match`).
- So the server SHOULD support `HEAD` on resources and answer 404 for missing ones.
- Safety: it refuses to DELETE if the item URI equals the calendar URI.

#### 4.4 Naming of new resources
- `CAL.getItemLocationPath` (469-476): `<UID with every char not in [a-zA-Z0-9_.-] replaced by "_">.ics`, appended to the calendar URL (percent-encoded per segment).
- New events get `cal.getUUID()`, which is a UUID WITHOUT braces (calUtils.sys.mjs 118-121, "avoid problems with servers that don't support {} in filenames"), so `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.ics`.
- Items that already exist keep the server-provided href. Imported/invited items with a foreign UID get the sanitised-UID filename.
- If the server returns an item at the same href with a different UID (server rewrote UID), Thunderbird deletes the old local one and re-adds (CAL 1001-1011).
- The server must accept an arbitrary `.ics` name chosen by the client, and must NOT require the resource name to equal the UID.

#### 4.5 ETag handling summary
- ETags come from `getetag` in multiget/sync/PROPFIND only; the PUT response ETag is unused.
- Any quoting style works as long as it is stable and identical between listings and multiget. Weak etags are not treated specially (inferred).
- Compared by exact string equality.


### 5. Properties read, and scheduling behaviour

| Property | Where requested | Used for |
|---|---|---|
| `D:resourcetype` | detection (depth 0 and 1), `checkDavResourceType`, etag-listing | calendar vs collection; must contain `C:calendar` |
| `D:displayname` | detection only | calendar name (fallback: URL last segment) |
| `A:calendar-color` (`http://apple.com/ns/ical/`) | detection only | colour; `#RRGGBBAA` alpha stripped |
| `D:current-user-privilege-set` | detection AND every `checkDavResourceType` | read-only flag if none of `write, write-content, bind, all`; see below |
| `C:supported-calendar-component-set` | `checkDavResourceType` | restricts VEVENT/VTODO support |
| `D:supported-report-set` | `checkDavResourceType` | enables sync-collection if it lists `sync-collection` |
| `CS:getctag` | `checkDavResourceType`, then each refresh (only without sync-collection) | change detection |
| `D:getetag`, `D:getcontenttype` | listing / sync / multiget | change detection, content-type filter |
| `C:calendar-data` | multiget | item body |
| `D:current-user-principal`, `D:owner` | detection, `checkDavResourceType` | find principal |
| `C:calendar-home-set` | detection, principal lookup | find calendars |
| `D:principal-collection-set` | scheduling discovery, only if no principal URL known | find principal |
| `C:calendar-user-address-set`, `C:schedule-inbox-URL`, `C:schedule-outbox-URL` | scheduling discovery | organizer address (first `mailto:`), inbox, outbox |
| `DAV:sync-token` property | NEVER requested | - |
| `calendar-description`, `calendar-order`, `calendar-timezone`, `max-resource-size`, `getlastmodified` | NEVER requested | - |

Details:
- Privilege set (`REQ.PropfindResponse.decorators`, 836-838; `CAL` 1705-1713): parsed as the local names of children of each `<privilege>` in the `DAV:` namespace. If the propstat status text contains `404`, the result is null: no read-only decision (writable). An empty 200 set means read-only. In `checkDavResourceType` the flag is only ever SET to read-only (a user's "read only" choice is never cleared). It is re-evaluated at each server-info check, but a persisted `readOnly` is not cleared automatically (inferred from the "Don't clear this.readOnly, only set it" comment). The detection-time flag also applies (PROV 436-441).
- `calendar-color` and `displayname`: only at subscribe time.
- `supported-calendar-component-set` (CAL 1670-1681): read as `<C:comp name="VEVENT"/>` children. If the set is non-empty, the supported types become the intersection of {VEVENT, VTODO} with it. If a calendar lacks `VTODO`, Thunderbird shows it as events-only and refuses to create tasks there with "Server does not support item type" (`CAL` 607-610 and `getProperty("capabilities.tasks.supported")`). `VJOURNAL` is not handled at all. If the prop is missing or empty, both VEVENT and VTODO are assumed.
- Fully ignored: `max-resource-size` (no size pre-check), `calendar-timezone` (it uses its own TZ database), `calendar-description` (never read, never written).
- All property values are matched by namespace URI (not prefix): `DAV:`, `urn:ietf:params:xml:ns:caldav`, `http://calendarserver.org/ns/`, `http://apple.com/ns/ical/`. Elements in other namespaces fall back to their literal prefix. Any prefix choice is fine for those four.
- The parser takes `<response>/<propstat>/<prop>/*` lists and merges ALL propstats of a response regardless of status. Unknown props (404 propstat) appear as empty and are stored as null.

#### 5.1 Scheduling (RFC 6638) behaviour
- Pref `calendar.caldav.sched.enabled` defaults to `false` (`calendar/base/calendar.js` line 98). With it off, `hasScheduling` cannot become true even if the server advertises `calendar-schedule`, so Thunderbird sends invitations by email (iMIP) through the user's mail identity.
- Auto-scheduling is NOT gated by that pref. In `checkServerCaps` (CAL 1784-1807), if the `DAV:` header of the OPTIONS response contains `calendar-auto-schedule`:
  - `hasAutoScheduling = true`.
  - Thunderbird stops sending iMIP itself (`canNotify` true, `itip.transport` = the calendar). It assumes the SERVER delivers invitations, replies and cancellations. Exception: attendees with `SCHEDULE-AGENT=CLIENT` still get email (`sendItems` 2431-2456).
  - The organizer identity is `calendar-user-address-set[0]` (mailto) if the server supplies it, unless the user configured an email identity for the calendar.
- DANGER: advertising `calendar-auto-schedule` without delivering invitations means no invitation is ever sent. Advertise only `calendar-access` unless auto-scheduling is implemented.
- `calendar-schedule` (without the pref) still triggers the principal/inbox/outbox discovery and registers the calendar as a free-busy provider.
- Explicit scheduling path (only if the pref is true and `calendar-schedule` advertised): inbox polling via the same `sync-collection`/PROPFIND code against `schedule-inbox-URL` (`pollInbox`, CAL 2293-2306), `REPLY` items in the inbox processed into the main calendar (`processItipReply`), and outbound `POST` to the outbox. In this snapshot, `sendItems` calls `new CalDavOutboxRequest(...)` with 6 arguments while the constructor takes 7 (the `aResponseMethod` parameter), so this branch appears broken (inferred). Do not rely on it.
- Inbox polling is skipped if `schedule-inbox-URL` equals the calendar URL (SOGo quirk, `mShouldPollInbox = false`, CAL 1981-1985).


### 6. iCalendar expectations

#### 6.1 What Thunderbird writes (`CAL.getSerializedItem` 2528-2536; `calendar/base/src/CalIcsSerializer.sys.mjs`)
- A fresh `VCALENDAR` for each PUT: `PRODID:-//Mozilla.org/NONSGML Mozilla Calendar V1.1//EN`, `VERSION:2.0` (`calItemUtils.sys.mjs` 446-451). Unknown VCALENDAR-level properties from the server's copy are not written back (`CALSCALE`, `X-WR-CALNAME` etc. are lost on the first edit).
- Then every non-UTC, non-floating TZID used gets a generated `VTIMEZONE` (CalICSService `addTimezoneReference`, 549-558), produced from Thunderbird's built-in ICU timezone database (`TimezoneDatabase.cpp` lines 127-142). So PUT bodies always contain VTIMEZONE for TZID-bearing dates.
- Then the master `VEVENT`/`VTODO` followed by each recurrence exception as additional components in the SAME resource (`cal.iterate.items` in `calIteratorUtils.sys.mjs` 25-35).
- Alarms: `VALARM` children (ACTION, TRIGGER, DESCRIPTION required default text, `REPEAT/DURATION`, attachments). Acknowledgement and snooze are stored with Thunderbird's own X- properties (below); NO RFC 9074 `ACKNOWLEDGED` property is read or written (grep found nothing).
- On Google URLs only: the DESCRIPTION is rewritten to bare HTML and `X-MOZ-GOOGLE-HTML-DESCRIPTION:TRUE` is added (REQ 1259-1280).

#### 6.2 X- properties and round-tripping
| Property | Source | Meaning | Round-trip need |
|---|---|---|---|
| `X-MOZ-GENERATION` | `CalItemBase.generation`, +1 on each modify | local revision counter | cosmetic; ignored in content comparison (`calItemUtils` 260-290) |
| `X-MOZ-LASTACK` | VEVENT/VTODO level and VALARM level (CalItemBase 883, 1134; CalAlarm 380, 463) | time the alarm was last dismissed | PRESERVE. If lost, dismissed alarms re-fire after the next sync, because Thunderbird re-fetches after every PUT and treats the server copy as truth |
| `X-MOZ-SNOOZE-TIME` and `X-MOZ-SNOOZE-TIME-<recurrenceId nativeTime>` | `CalAlarmService.sys.mjs` 328-380, 591-594 | snooze-until, per occurrence for recurring items | PRESERVE (same reason) |
| `X-MOZ-FAKED-MASTER` | `CalIcsParser` (89) | marks a master fabricated for an orphan exception; internal | unneeded on the wire |
| `X-MOZ-SEND-INVITATIONS`, `X-MOZ-SEND-INVITATIONS-UNDISCLOSED` | event dialog | whether to send invitations | cosmetic |
| `X-MOZ-INVITED-ATTENDEE`, `X-MOZ-RECEIVED-SEQUENCE`, `X-MOZ-RECEIVED-DTSTAMP` | iTIP processing | internal | optional |
| `X-MOZ-GOOGLE-HTML-DESCRIPTION` | Google only | - | - |
- Rule: Thunderbird keeps every property it does not model in a property bag, with parameters, and writes it back. A server that stores the iCalendar verbatim and returns it unchanged is the safest. Normalising or stripping unknown properties from stored data affects alarms (acknowledgement and snooze state) first.
- The server may add its own X- properties. They survive Thunderbird's PUT because of the property-bag behaviour above.

#### 6.3 VTIMEZONE: required or not?
- Not required in server responses. Parsing resolves a `TZID` by name through Thunderbird's own timezone service (`CalTimezoneService.getTimezone`, 99-137) which uses the built-in ICU database and also maps old `/mozilla.org/<date>/Continent/City` TZIDs. The `VTIMEZONE` block in a received file is not what defines the zone (the ical.js `TimezoneService.get` is patched to the database, CalTimezoneService 54-55). Inferred: a `VTIMEZONE` present for a known IANA TZID is harmless.
- A TZID that is not known to the database (non-IANA such as Windows names like "W. Europe Standard Time", or custom): logs `Could not find definition for <tzid>`, the date is treated as a "phantom timezone", and the user gets an alert plus a console error (`CalIcsParser.checkTimezone` 235-260, and the `tzErrors` alert at 130-145 of the same file). Servers should emit IANA TZIDs, or normalise.
- Floating times and UTC (`Z`) are fine.

#### 6.4 Recurrence exceptions
- Master and all overrides (`RECURRENCE-ID`) must be in ONE resource sharing the UID (RFC 4791). The parser attaches exceptions to the master (`modifyException`, CalIcsParser 80-100).
- An orphan exception without a master gets a fabricated master carrying `RDATE`s (`X-MOZ-FAKED-MASTER`). Servers should return complete resources.
- `RDATE`, `EXDATE`, `RRULE`, `EXRULE` are supported; `RELATED-TO` is promoted; `ATTENDEE`, `ORGANIZER`, `ATTACH`, `CATEGORIES` are promoted.

#### 6.5 VTODO
- By default Thunderbird assumes both VEVENT and VTODO (CAL 66-68). It narrows this ONLY if `supported-calendar-component-set` is non-empty and lacks VTODO. No fallback or probing otherwise.
- Without a restriction, task creation in the UI goes to the server as VTODO. A server that rejects VTODO should advertise `<C:comp name="VEVENT"/>` only, so the UI disables tasks and the user sees a clear refusal. Otherwise a rejected task PUT hits the generic error path (4.2).
- `VJOURNAL` and `VFREEBUSY` components on the wire are put in `extraComponents` by the parser and not shown.


### 7. Free-busy

- No `free-busy-query` REPORT anywhere. Free-busy is a schedule-outbox `POST` (`REQ.CalDavFreeBusyRequest` 1102-1156; `CAL.getFreeBusyIntervals` 2123-2219).
- Used when the user looks at attendee availability in the event dialog. It is registered with `cal.freeBusyService` only if `OPTIONS` advertised `calendar-auto-schedule` or `calendar-schedule` (CAL 1793-1802).
- Preconditions: `outboxUrl` and at least one `calendar-user-address-set` entry known, found through `schedule-outbox-URL`/`calendar-user-address-set` on the principal. Only one calendar per server+realm queries (`firstInRealm`).
- Request: `POST <outbox url>`, `Content-Type: text/calendar; charset=utf-8`, headers `Originator: <first calendar-user-address>` and `Recipient: <attendee address>` (mailto lower-cased). Body: a `VCALENDAR` with `METHOD:REQUEST` and a `VFREEBUSY` (UID, DTSTAMP, DTSTART/DTEND in UTC, ORGANIZER, ATTENDEE with PARTSTAT=NEEDS-ACTION, ROLE=REQ-PARTICIPANT, CUTYPE=INDIVIDUAL).
- Response expected: HTTP 200 with an XML `<C:schedule-response><C:response><C:recipient><D:href>..</D:href></C:recipient><C:request-status>2.0;Success</C:request-status><C:calendar-data>VFREEBUSY...</C:calendar-data>`. Request-status must start with `2`. `FREEBUSY` periods with `FBTYPE` of FREE, BUSY, BUSY-UNAVAILABLE, BUSY-TENTATIVE are mapped; the gaps between range start/end and the returned DTSTART/DTEND are marked UNKNOWN.
- Any other status or a parse failure is silently treated as "no data".
- If we do not implement scheduling, simply do not advertise `calendar-schedule` / `calendar-auto-schedule`. Thunderbird then makes no free-busy attempt at all.


### 8. Server-specific workarounds in the code (what it tolerates, and chokes on)

| Server | Workaround | Location |
|---|---|---|
| Google (`apidata.googleusercontent.com/caldav/v2/<user>/user`) | OAuth; description sent as bare HTML with `X-MOZ-GOOGLE-HTML-DESCRIPTION`; one-time forced resync pref `calendar.caldav.googleResync`; 403 body exactly `<D:error xmlns:D="DAV:"/>` or 429 treated as rate limit (not refreshed, token kept) | `PROV` 250-270; `REQ` 1259-1280; `CalCalendarManager` 645-711; `HND` 535-544 |
| Apple iCloud | content type missing in sync responses (use `.ics` suffix); status 200 on every item; status entries for directories | `HND` 686-698, 726-729, 750-759 |
| Apple iCal Server | `text/plain` content type on resources | `HND` 687-697 |
| Scalix | `message/rfc822` content type | `HND` 357-361 |
| Kerio | `text/vtodo` content type | `HND` 362-365 |
| SOGo | calendar URL equals inbox URL; 409 + plain-text `D:error` on UID conflict; items must not be deleted from the "inbox" when it IS the calendar | `CAL` 774-777, 1981-1985; `MOCK` 673-680 |
| Nextcloud / Sabre | bare HTTP 400 "Calendar object with uid already exists" (old versions); newer sends 409 | `REQ` 326-350 |
| Radicale | 409 with `<C:no-uid-conflict/>` | `MOCK` 666-672 |
| Fastmail | app password saved login used instead of OAuth | `SES` 59-68 |
| Cosmo | trailing `?ticket=...` URL params kept and appended to every URL | `CAL` 349-373, 1623-1625 |
| Servers that need the old `Depth` header on sync-collection | both `Depth: 1` and `<sync-level>1</sync-level>` are sent | `HND` 463-470 |
| Servers that put a different path prefix on hrefs than on the request URL | leading path components stripped by count | `CAL` 992-998 |
| Servers that modify items on PUT | forced re-fetch after every PUT | `CAL` 642-646, 769-773 |
| Servers that return 201 on a modify | accepted as success | `REQ` 687-692 |
No Zimbra, Baikal, DAViCal or Bedework special cases exist in this code (grep). Radicale appears only in test fixtures.

What Thunderbird chokes on (summary of failure modes seen above):
- 401/403 on detection probes: abort detection with an auth error. 403 on `/.well-known/caldav` aborts too.
- Missing `resourcetype` in a depth-0/1 PROPFIND response: TypeError, the method fails.
- `OPTIONS` on the calendar's parent collection returning non-2xx/404: silent permanent non-sync.
- Responses with a bad structure in multiget (propstat 404 for `calendar-data`, no etag): whole multiget fails.
- Padding whitespace inside `href` / `getetag` / `sync-token` leaf elements.
- A `calendar-auto-schedule` claim without real delivery: invitations are never sent.
- Non-IANA TZIDs: alert and phantom time zones.
- A 4xx for the initial empty-token `sync-collection`: calendar disabled.

## Conclusion

Server requirements for Thunderbird, from the source above. Not yet exercised against a live Thunderbird (HUMAN-CHECKS). Since M3 the bridge's adapter implements them and the tests below assert them against Thunderbird's wire shapes, not against Thunderbird.

### MUST
1. Answer unauthenticated requests with `401` + `WWW-Authenticate: Basic realm="..."` (Digest works but adds a synchronous `OPTIONS` that must return exactly 200). Do not use 403 for "not logged in".
2. `/.well-known/caldav` must redirect (301/302/307/308) to the principal or DAV root within the same origin, or answer a `207` itself. It must not answer 403. `PROPFIND` on it must be accepted.
3. `PROPFIND Depth:0` on the entered URL, on `/`, and on the principal, with `text/xml; charset=utf-8` bodies, must return `207` with `D:resourcetype` always present, plus `D:current-user-principal` (on `/` or the root target) and `C:calendar-home-set` (on the principal). Property matching is by namespace, any prefix.
4. `PROPFIND Depth:1` on the home set must list each calendar with `resourcetype` containing both `D:collection` and `C:calendar`, `displayname`, `current-user-privilege-set` and optionally `A:calendar-color`. Every response must have a `resourcetype` element, even if empty.
5. `PROPFIND Depth:0` on each calendar (trailing slash) must return, with 207: `resourcetype`, `supported-report-set` (list `sync-collection` and `calendar-multiget`, `calendar-query` for other clients), `supported-calendar-component-set` as `<C:comp name="VEVENT"/>` (add VTODO only if tasks work), `current-user-privilege-set`, `CS:getctag` (stable fallback), `owner`, `current-user-principal`.
6. `OPTIONS` on BOTH the calendar-home parent URL and the calendar URL must return 2xx with a `DAV:` header (e.g. `1, 2, 3, calendar-access`). A non-2xx/non-404 on the parent silently disables the calendar.
7. `REPORT sync-collection` (empty token and valid token):
   - `Depth: 1` header, `<sync-level>1</sync-level>`, props `getcontenttype` + `getetag`.
   - The response is a 207 with per-resource `href`, `getetag`, `getcontenttype: text/calendar; charset=utf-8` (a `;component=VEVENT` suffix is fine, MOCK does it), and a top-level `<sync-token>`.
   - Deletions are `<response><status>HTTP/1.1 404 Not Found</status><href>...</href></response>`.
   - An empty token must return the full listing. A new token must be returned even if nothing changed.
   - An invalid/expired token must answer 4xx (403 or 409 + `valid-sync-token` is fine). Thunderbird then re-syncs with an empty token.
   - Large result sets may be paged with a `507` response element and an intermediate token.
8. `REPORT calendar-multiget` (`Depth: 1`, `D:getetag` + `C:calendar-data`, path-only percent-encoded hrefs, up to 100 per request):
   - Return `207` with one `<response>` per requested href: `href`, `getetag`, `calendar-data`.
   - A missing href gets a response-level `404` status, not an empty propstat.
9. Resources are `.ics`, one UID per resource, all recurrence overrides inside the same resource, located directly under the calendar collection URL. Accept any client-chosen filename (Thunderbird uses `<uuid>.ics` or the sanitised UID).
10. `PUT`:
    - `If-None-Match: *` on create, `If-Match: <etag>` on update; `Content-Type: text/calendar; charset=utf-8`.
    - Answer 201 (create) / 204 (update). 200 is also accepted.
    - 412 on a failed precondition (If-Match mismatch, or If-None-Match: * when the URL exists).
    - 409 + `<C:no-uid-conflict>` for a UID already present at another URL.
11. `DELETE` with `If-Match`: 204. 404 for a missing resource (treated as success). 412 when the etag mismatches. `HEAD` on a resource must give 200/404 (404 ends the delete flow).
12. ETags: change on every content change, identical in listings, multiget and `getetag`; no whitespace around the leaf values in XML.
13. Store iCalendar text verbatim, preserving all unknown X- properties and parameters, in particular `X-MOZ-LASTACK` (event and VALARM level) and `X-MOZ-SNOOZE-TIME*`, `X-MOZ-GENERATION`.
14. Accept request bodies sent as `Content-Type: text/xml` (not only `application/xml`) on PROPFIND/REPORT, and the ignored headers `Accept: text/xml`, `Accept-Charset: utf-8,*;q=0.1`.

### SHOULD
- Return `current-user-privilege-set` accurately (`D:read` only for read-only calendars). Missing/404 means writable, an empty set means read-only.
- Return `getcontenttype` on every resource in every listing (the PROPFIND fallback path ignores resources without it).
- Support `PROPFIND Depth:1` on the calendar (`getcontenttype, resourcetype, getetag`) and `CS:getctag` as the fallback path used when `sync-collection` is not advertised; useful for test coverage.
- Offer IANA TZIDs in stored data. A `VTIMEZONE` block in responses is optional (Thunderbird uses its own database) but harmless and helps other clients.
- Support HTTPS with a real certificate. Self-signed works but forces a user-confirmed exception dialog. Plain `http://localhost:PORT/` works when the URL is typed with `http://`; a bare hostname becomes `https://`.
- Provide `D:owner` or `current-user-principal` on the calendar so the principal is found without extra steps.
- Return `supported-calendar-component-set` with VEVENT only if VTODO is unsupported, so the UI refuses tasks early.
- Keep response times small for the depth-0 and OPTIONS calls: these happen on every startup for each calendar, plus every 30 min by default (1/5/15/60 min or manual selectable).
- Return `Content-Type: text/xml` or `application/xml` with a `charset` for 207 bodies; the charset is honoured.

### AVOID
- Advertising `calendar-auto-schedule` (or `calendar-schedule`) unless the server really delivers iTIP. `calendar-auto-schedule` makes Thunderbird stop emailing invitations. `calendar-schedule` triggers scheduling discovery and free-busy attempts.
- 403 for anything other than a genuine permission failure: on detection it aborts, on sync-collection with a stored token it resets the token and re-syncs. Do not return exactly `<D:error xmlns:D="DAV:"/>` with 403 (read as rate limiting: refresh skipped, token kept).
- 4xx on the first (empty-token) `sync-collection`, `OPTIONS` or depth-0 PROPFIND: disables the calendar. 5xx on those is only a temporary failure.
- Normalising, re-serialising or stripping unknown properties of stored iCalendar. At minimum it makes dismissed alarms re-fire.
- Pretty-printed XML with whitespace inside `href`, `getetag`, `sync-token` or `calendar-data` leaf elements.
- Responses with elements Thunderbird cannot classify during sync (a `text/calendar` item without `getetag`, or odd statuses on non-404 non-calendar items): these abort the whole sync as "unhandled errors".
- Multi-UID resources, nested collections inside a calendar, or items outside the calendar path (the location-path calculation assumes direct children).
- Relying on error text or error bodies to reach the user: Thunderbird shows only a localised generic message plus the HTTP status, never the server's text.
- Relying on `calendar-query`, `time-range`, `free-busy-query`, MKCALENDAR, PROPPATCH or `calendar-color`/`displayname` changes propagating to existing Thunderbird subscriptions: none of that is sent or re-read.
- Weak dependence on `ETag` response header after PUT: Thunderbird ignores it and re-fetches by multiget.

### Test seam
`calendar/test/CalDAVServer.sys.mjs` is Thunderbird's own mock server and the best available "executable spec" of the exact wire shapes accepted: well-known 301 to principal, principal, home set (two responses), calendar depth-0 / depth-1, sync-collection with 507 paging and 404 deletions, multiget, PUT 204 / 412 / the five UID-conflict variants, DELETE 204, Basic auth with 401 + `WWW-Authenticate`. Related unit tests: `calendar/test/unit/test_caldav_requests.js`, `test_caldav_session.js`, `test_caldav_uidCollision.js`, `test_caldav_offline_modify_playback.js`; `calendar/test/unit/providers/test_caldavCalendar_uncached.js` and `test_caldavCalendar_cached.js`, `calendar/test/browser/browser_calDAV_discovery.js`, `calendar/test/browser/providers/browser_caldavCalendar_noSyncToken.js`.

### Not determined from the source (inferred or unknown)
- Whether necko sends Basic credentials preemptively after the first challenge on the same path prefix, and any http-vs-https policy in necko: Gecko code, not in comm-central.
- How the ical.js parser treats a `VTIMEZONE` block whose TZID differs from the database definition (appears ignored).
- The exact localized wording shown for PUT/DELETE errors (it is in `calendar.ftl`, not read).

### Answers to SPEC questions

- **E11, does Thunderbird need VTIMEZONE in server responses?** No (§6.3 above): it resolves a `TZID` by name through its own ICU-based timezone database. A `VTIMEZONE` for a known IANA TZID is harmless; a non-IANA TZID (Windows names) triggers an alert and a phantom zone. Thunderbird itself always writes a generated `VTIMEZONE` in its PUT bodies. Source-verified only.
- **§9 Thunderbird as reference client**: its exact requests and failure modes are now documented from source (sections 1 to 8); replayable traces from a live session are still pending.

## What the bridge does (M3)

Server behaviour per MUST/SHOULD/AVOID item, with the contract test that asserts it (`internal/caldav/contract_test.go` on a stub service, `report_test.go` on the real service over the fake Proton). All [verified: the named test]; every one is a test of our server against the shapes read above, none is a Thunderbird run. Deviations from the list are marked and ruled.

| Item | Bridge behaviour | Test |
|---|---|---|
| MUST 1 | 401 with `WWW-Authenticate: Basic realm="pcb", charset="UTF-8"` for missing or wrong credentials, never 403; a blocked IP (5 failures per minute) gets 503 + `Retry-After` in whole seconds, **not** 429 (M3-R26: a 4xx on the first PROPFIND disables the calendar) | `TestMust01UnauthenticatedRequestsGet401WithTheBasicChallenge`, `TestMust01BlockedClientsGet503WithWholeSecondsRetryAfter` |
| MUST 2 | `/.well-known/caldav` behind auth, 308 to the principal | `TestMust02WellKnownRedirectsToThePrincipal` |
| MUST 3 | Depth 0 on `/`, the entered URL and the principal: `resourcetype` always, `current-user-principal`, `calendar-home-set`, matched by namespace | `TestMust03DiscoveryFromTheRootFindsPrincipalAndHomeSet` (+ the go-webdav client, E17: `TestGoWebdavClientDiscoversAndReads`) |
| MUST 4 | home set Depth 1 lists every calendar: collection + calendar, name, privileges, colour; 503 + `Retry-After` while every calendar is pending (M3-R30) | `TestMust04HomeSetDepth1ListsEveryCalendar`, `TestHomeSetIs503WhileEveryCalendarIsPending` |
| MUST 5 | calendar Depth 0: `supported-report-set` is exactly sync-collection + calendar-multiget (no calendar-query: it is not implemented), VEVENT only, `getctag`, `owner`, `current-user-principal` | `TestMust05CalendarDepth0CarriesTheServerInfo` |
| MUST 6 | OPTIONS on any path: 200 + `DAV: 1, 3, calendar-access`, **without auth** (M3-R23: it reveals nothing, and Digest-style probes need it) | `TestMust06OptionsAnswers200WithTheDAVHeaderWithoutAuth` |
| MUST 7 | sync-collection: empty token lists everything with a token; delta; deletions as response-level 404 per UID; a new token even when nothing changed; an invalid token is 403 with `<D:error><D:valid-sync-token/></D:error>`, never the bare `<D:error/>`; no 507 paging [source: M3-R23, listings carry only href and ETag; no test asserts the absence] | `TestMust07SyncCollectionWithAnEmptyTokenListsEverything`, `TestMust07SyncCollectionServesTheDeltaAndDeletionsAs404`, `TestMust07InvalidSyncTokenIs403ValidSyncToken`, `TestSyncCollectionReportsDeletionsPerUID`, `TestSyncTokenOfAnotherEpochIsRefused`, `TestSyncLevelMustBe1OrInfinite` |
| MUST 8 | multiget: one response per href, missing ones response-level 404, percent-encoded hrefs decoded exactly; more than 1 000 hrefs is 413 (not 403) | `TestMust08MultigetAnswersEveryHrefAndMissingOnesWith404`, `TestMust08MultigetRefusesMoreThan1000Hrefs`, `TestHrefNamesDecodeExactly`, `TestPathVariantsResolveExactly` |
| MUST 9 | one `<uid>.ics` per UID directly under its calendar; the href escapes the UID (M3-R13) | `TestMust09ObjectsAreICSResourcesDirectlyUnderTheirCalendar`, `TestHrefNamesDecodeExactly` |
| MUST 10, 11 | **M3 refuses writes**: PUT and DELETE answer 403 `DAV:need-privileges`; HEAD answers 200/404. Writes arrive in M4 | `TestMust10And11WritesAreRefusedUntilM4AndHeadAnswers` |
| MUST 12 | ETags identical in listings, multiget and GET, and changing with content | `TestMust12ETagsAgreeAcrossListingsAndReads`, `TestMust12ETagsChangeWithContentAndAgree` |
| MUST 13 | iCalendar bytes served as rendered (CRLF, `X-MOZ-*` kept) | `TestMust13CalendarDataIsServedByteForByte` |
| MUST 14 | `text/xml` bodies and Thunderbird's `Accept` headers accepted | `TestMust14AcceptsTextXMLBodiesAndThunderbirdHeaders` |
| SHOULD privileges | owned normal calendars advertise read + write although writes are refused (M3-R1: Thunderbird keeps its read-only flag from subscription, E08 §5, so no one subscribes to an M3-only server read-only); other calendars read only. M4 must derive this from member permissions (M3-R15) | `TestShouldPrivilegesFollowReadOnly` |
| SHOULD content type, Depth 1 fallback | `getcontenttype` and `getetag` on every object; Depth 1 on a calendar works, with `calendar-data` when asked | `TestShouldDepth1ListingCarriesContentTypeAndETagOnEveryObject`, `TestDepth1ListingServesCalendarDataWhenAsked` |
| AVOID scheduling | no `calendar-schedule` / `calendar-auto-schedule` in `DAV:` | `TestMust06OptionsAnswers200WithTheDAVHeaderWithoutAuth` |
| AVOID 403 | 403 only for genuine refusals (writes, infinite Depth `propfind-finite-depth`, unknown REPORT `supported-report`, invalid token) | `TestAvoid403OutsideGenuineRefusals`, `TestPropfindDepthInfinityIs403FiniteDepth`, `TestAvoidUnsupportedReportsAre403SupportedReport` |
| AVOID 4xx on first sync / OPTIONS / Depth 0 | service unavailable (logged out, no cache, Proton 5xx) is 503 + `Retry-After` 30 (M3-R22); a pending calendar is 503 and left out of the home set (M3-R25); unknown calendars 404 | `TestAvoid4xxWhenTheServiceIsUnavailable`, `TestPendingCalendarsAre503AndLeftOutOfTheHomeSet`, `TestPendingCalendarIs503UntilTheFirstPassReachesIt`, `TestUnknownCalendarsAre404`, `TestCancelledRequestsAreNotReportedAsFailures` |
| AVOID normalising iCalendar | merge keeps unknown properties byte for byte (M3 `internal/ical`); an undecryptable or unparsable event is a placeholder VEVENT (`X-PCB-PLACEHOLDER:1`) and never fails a report (M3-R4) | `TestMust13CalendarDataIsServedByteForByte`, `TestUndecryptableEventDoesNotFailTheReport` |
| AVOID whitespace in leaves | no indentation in XML | `TestAvoidWhitespaceInLeafElements` |
| AVOID unclassifiable sync responses | every sync item has etag + type or is a 404 | `TestAvoidUnclassifiableSyncResponses` |
| AVOID error text in bodies | bodies carry no error text, Proton text or other users' IDs | `TestAvoidErrorTextInBodies` |
| AVOID calendar-query, PROPPATCH, MKCALENDAR | PROPPATCH and MKCALENDAR 405 with `Allow`; calendar-query, free-busy-query 403 `supported-report` | `TestAvoidProppatchAndMkcalendarAre405`, `TestAvoidUnsupportedReportsAre403SupportedReport` |
| Hardening beyond the list | bodies over 8 MiB 413; XML with a DOCTYPE refused; at most 64 property names and 64 attributes per element; fuzzed parsers | `TestBodiesOver8MiBAre413`, `TestXMLWithADoctypeIsRefused`, `TestPropertyListsOver64NamesAre400`, `TestElementsWithOver64AttributesAre400`, `TestXMLElementAndDepthLimits`, `FuzzReportBody`, `FuzzPropfindBody` |

Not covered by a test of ours: VTIMEZONE is not served (none, E11 answered from source); the SHOULD "fast depth-0 and OPTIONS" has no timing test; the `getcontenttype` property carries `text/calendar; charset=utf-8; component=vevent` and GET answers `text/calendar; charset=utf-8` [source: internal/caldav/propfind.go:220, server.go:207; no test asserts the suffix].

Still to confirm live (research list, HUMAN-CHECKS step 6): preemptive Basic after the first challenge, 308 versus 301 on detection, `readOnly` persistence when M4 changes the privileges, the self-signed exception flow over the network, non-IANA TZIDs and orphan exceptions from real data, VALARM rendering from `Notifications`, first-sync cost on thousands of events, shared calendars.

## Feeds

M3 CalDAV read and M4 CalDAV writes (A1 adapter: SPEC §9; the MUST/SHOULD/AVOID list above becomes its contract tests, with `MOCK` as the wire-shape reference), E11 (VTIMEZONE, answered below), E17 (go-webdav audit, done: `E17-go-webdav-audit.md`), SPEC §16 CalDAV test plan.
