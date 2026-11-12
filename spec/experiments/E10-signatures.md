# E10 — Which keys sign which parts, and the member passphrase signature

Status: **source-based; the code relies on it since M3 (read) and M4 (write gate, sealing); live run pending** (`spec/HUMAN-CHECKS.md` §6, steps 6.1.4, W1, 6.4.4). Marks: `[source]` read in code, `[claim: X live]` a third-party author's observation, `[inferred]` our reading. Evidence: `raw/m4-research.md` §B.2, §B.3, §G ("research G"). Feeds SPEC §5.3, ruling R17 (M0) and M4-R1.

## Question

1. Which key signs each part of an event Proton serves (shared signed, shared encrypted, calendar signed, calendar encrypted, attendees, personal), on our own calendars? Does Proton or its web client ever sign a part of ours with another key than the author address's?
2. Does the member passphrase `Signature` of a calendar we own verify, over the decrypted passphrase, with our member's address key (R17)? Only that signature binds the calendar key to us: without it, the server could serve a calendar key it chose, and we would encrypt events to it (research G).
3. Do the parts the bridge writes (binary-mode detached signatures by the primary key of our member's address, key packets to the primary calendar key only) read back as ours in Proton and in the web app?

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| Every part is signed by the member's address key; another address's key is refused with 2001 "Provide data signed using the address key" | research B.3, A.4 (`carbonate/docs/ARCHITECTURE.md:615-617`; `caloxide/protonmail/calendar.go:1060-1063`) | [claim: carbonate, caloxide live] |
| Signed parts: detached signature over the exact fragment bytes; encrypted parts: detached signature over the plaintext | research B.2 (`proton-cal/docs/crypto.md:316-335`) | [claim: proton-cal live] |
| Binary vs text mode: carbonate signs in text mode and works; proton-cal and cal-gateway sign in binary | research B.2 | [claim: carbonate live] for text; [source] for the others |
| `MemberPassphrases[].Signature` is verified by go-proton-api with the address keyring over the decrypted passphrase; carbonate uses that helper on its own calendars | research G (`go-proton-api/calendar_types.go:119-140`; `carbonate/internal/proton/client.go:510-518`) | [source]; [claim: carbonate live] |
| cal-gateway and caloxide skip that check: it fails on calendars another member shared (signed by the inviter's key) | research G | [source] |

## What the code assumes

Read side (M3, SPEC §5.3): every part authored by one of our addresses is verified with that address's keys; a bad or missing signature on a shared or calendar part of ours marks the VEVENT `X-PCB-INTEGRITY:unverified` and `pcb serve` counts it (`unverified` in its `render counters` log line). Parts authored by other addresses are not verified (no public-key lookup).

Write gate (rulings R17, M4-R1, M4-R39; `internal/proton/calendar/unlock.go`):
- `PassphraseVerified`: the bootstrap's member passphrase signature verifies, over the decrypted passphrase, with **our member's address** keyring (binary message). [verified: `TestPassphraseSignatureVerifiedWithOurAddressKey`, `TestStrippedOrReplayedPassphraseSignatureIsNotVerified`, `TestBadPassphraseSignatureLeavesTheCalendarReadOnly`]
- A signature served and checked that does not verify closes the gate (403 `need-privileges`, read-only privileges). No signature, or our address keyring not unlocked, leaves the gate **undecided**: writes answer 503 and the collection keeps the last decided privileges, none before any decision (M4-R39, M4-R40). [verified: `TestUnverifiableSignatureLeavesTheGateUndecided`, `TestGateVerdicts`, `TestUndecidedWriteGateIs503`]
- `pcb calendar list` shows `read_only` false only when the gate is open.

Sealing (M4, `internal/proton/cards/seal.go`):
- The signer is the primary key of our member's address (`Access.AddressID`), alone in a one-key ring. An address without an unlocked primary key leaves no signer; two calendar keys flagged primary (or none unlocked) leave no recipient for key packets; either leaves the gate undecided (503 on a write). [verified: `TestSignerIsThePrimaryKeyOfTheAddress`, `TestAnomalousPrimaryFlagsFailClosed`]
- Signed parts: armored detached **binary-mode** signature (sigclass 0x00) over the exact fragment bytes; encrypted parts: SEIPD under the session key, `Data` base64 of the data packet only, `Signature` detached over the plaintext. [verified: `TestSealSignsBinaryExactBytes`, `TestOpenSealRoundTrip`]
- A create's key packets are wrapped to the calendar key whose bootstrap `Flags` has bit 2 (primary), alone. [verified: `TestSealKeyPacketOpensWithThePrimaryOnly`, `TestPrimaryKeyIsTheFlaggedOne`]
- A write is answered success only when every part of the rows read back is ours and verifies (`partsHold`, M4-R32). [verified: `TestReadBackComparesPlaintext`]
- A write to an event any part of which is not ours, unsigned or unverified is refused (`pcb:not-ours`, `pcb:unverified`; M4-R18, M4-R19).

The fake Proton verifies every signature with the member's address key and refuses another address's (2001), so the offline suite cannot tell which key real Proton expects: that is what this experiment settles.

## Live run that settles it

`spec/HUMAN-CHECKS.md` §6, on the test account:
1. **6.1.4**: `bin/pcb calendar list --json`: the owned default calendar `read_only` false means our member has the Write bit and the passphrase signature verified with our address key (question 2). If `read_only` is true, the lab probe there reads the member `Permissions` and whether a `Signature` is served, which tells E14's bit from this signature.
2. **W1**: on an event pcb created, the shared signed part's signature is `sigclass 0x00` (binary) and its `SharedKeyPacket` holds exactly one PKESK (one recipient: the primary calendar key). The 201 itself means the read-back verified every part as ours (question 3).
3. **6.3 B1**: the web app opens pcb's events without an error (question 3, Proton's own client).
4. **6.4.4**: `pcb serve`'s `render counters` line never shows `unverified` above 0 over the account's own rows, the web app's included (question 1: Proton's client signs our parts with our address key).
5. A calendar shared by another member (the cal-gateway/caloxide case): needs a second test account; skipped and said so.

Results: not run yet. Record one line per step: `[verified: live]` or what differed.

## Decision it feeds

- A failed step 1 with the Write bit set and a signature served means R17's assumption is wrong for owned calendars: every calendar stays read only. Then find how Proton signs the passphrase (which key, which message) before writes can be enabled; never relax the check to "no signature needed".
- `unverified` above 0 on rows written by the web app means Proton's client signs some part with another key: SPEC §5.3's policy must then name that part, and the write side's `pcb:unverified` refusal would refuse those events (fail closed, logged).
- The M3 parked items (bind each part's `Author` to its signing key; refuse unsigned cleartext attendees or personal parts; `OPEN.md`) wait for step 4.
