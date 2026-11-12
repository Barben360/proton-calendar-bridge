# E09 — Decrypt cost per 1 000 events

Offline half **measured** (2026-11-11, synthetic rows from the fake Proton); live half **pending** (test-account rows, cold start). Feeds the C2 design question of SPEC §7.2: is decrypting on demand cheap enough, or is a sealed decrypted cache needed? Earlier source figures: `raw/m2-feed-research.md` (E09), caloxide's claimed ~1.4 ms per event without signature checks.

## Offline measurement

`BenchmarkOpen` (`internal/proton/cards/cards_test.go`): `cards.Open` over 1 000 distinct synthetic rows served by `protontest`, cycled; one op = one row.

```
go test -run '^$' -bench BenchmarkOpen -benchtime 3000x -count 5 ./internal/proton/cards/
```

| Condition | ns/op | B/op | allocs/op |
|---|---|---|---|
| Machine quiet | 193 000 – 266 000 | ~96 100 | 627 |
| Machine loaded (load average ~21 on 24 threads), `-cpu 1` | 286 000 – 311 000 | ~96 060 | 627 |
| Machine loaded, default GOMAXPROCS | 328 000 – 396 000 | ~96 120 | 627 |

So about **0.2–0.4 s of CPU per 1 000 events, on one goroutine**, signatures checked.

- Machine: AMD Ryzen AI 9 HX PRO 370 (12 cores / 24 threads, laptop), Linux, go1.27.3, no `-race`. The open is single-threaded; nothing is parallelised.
- Row shape: every part present (6): shared signed and calendar signed (cleartext + detached signature), shared encrypted, calendar encrypted and attendees (SEIPD under a session key, signature over the plaintext), personal (armored message). Per row: 2 PKESK unwraps (x25519), 1 armored x25519 message, 4 AES data packets, 6 signature checks. About 1 KB of iCalendar text per row.
- Where the time goes (CPU profile of `Open`): signature verification ~41 %, session-key unwrap (x25519 PKESK) ~28 %, personal armored message ~15 %, AES data ~3 %, the rest JSON decoding and allocation.

## Caveats

- The fake's address key is go-proton-api's precomputed **RSA-1024** key (fastKeys), so verification here is RSA-1024. Real address keys are Ed25519 (similar or cheaper) or RSA-2048/4096 (verification grows with the modulus, still well under a millisecond). Real calendar keys are x25519, as in the fake.
- Real rows often carry fewer parts (no calendar encrypted, attendees or personal part on a plain event), so the decrypt share is an upper figure; RSA-4096 authors would raise the verification share.
- Synthetic content is ~1 KB per row; long descriptions or attachments only add AES time, which is ~3 % here.

## M3 as built

`service.Local.Serve` renders rows on demand through `cards.Open` and the `internal/ical` merge, into an in-memory LRU of rendered objects (fixed at 2 000 entries, `lruCapacity`; not configurable, counted by objects, shared by all calendars; ruling in the M3 plan, docs corrected by the initial-version audit); nothing decrypted reaches the disk [verified: `TestUndecryptableEventDoesNotFailTheReport` serves a report through the real service over the fake Proton; the LRU size and the no-disk rule are [claim]: no test of their own in this task]. No new timing was taken: `BenchmarkOpen` above is the only measurement, and no benchmark covers the merge, the render or a whole cold start. A cold-start figure (login to the first full render of every calendar) needs the live half.

## Live half (pending)

With the test account (`spec/HUMAN-CHECKS.md`, after the CAPTCHA step): time `cards.Open` over the account's real cached rows, and the cold start (login, key unlock, calendar bootstrap, first full render of every calendar). Record the address key algorithm and the parts mix per row.

## Reading so far

At 0.2–0.4 ms per event, a 1 000-event calendar renders in well under a second of CPU, far below the network cost of the initial listing. On-demand decryption with an in-memory LRU of rendered objects (M3 plan) is enough; no decrypted cache on disk is needed, which matches §7.2 "nothing decrypted is ever written". The live half confirms or revises this.
