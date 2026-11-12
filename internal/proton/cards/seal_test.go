package cards_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// writeKinds are the part kinds the bridge writes, in order.
var writeKinds = []ical.PartKind{ical.SharedSigned, ical.SharedEncrypted, ical.CalendarSigned, ical.CalendarEncrypted, ical.Attendees}

// sealRig is a rig with the write keys: the signer (r.addr, the member
// address's key alone in its ring), the primary calendar key alone in its
// ring (r.calKR) and the retired calendar key.
type sealRig struct {
	*rig
	retired *crypto.KeyRing
}

func newSealRig(t *testing.T) *sealRig {
	t.Helper()
	r := newRig(t)
	return &sealRig{rig: r, retired: r.f.RetiredCalendarKeyRing(r.cal)}
}

// create are the keys of a create: no session keys.
func (r *sealRig) create() cards.SealKeys {
	return cards.SealKeys{Signer: r.addr, CalendarPrimary: r.calKR}
}

// frag is a fragment in Proton's form (constraints): UID and DTSTAMP first,
// CRLF, no VERSION/PRODID, no CRLF after the last line.
func frag(uid string, lines ...string) []byte {
	all := append([]string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261111T090000Z"}, lines...)
	return []byte(strings.Join(append(all, "END:VEVENT", "END:VCALENDAR"), "\r\n"))
}

// fullParts holds every kind, each with a line of its own, valid for the
// fake's routing.
func fullParts(uid string) map[ical.PartKind][]byte {
	return map[ical.PartKind][]byte{
		ical.SharedSigned:      frag(uid, "DTSTART:20261112T090000Z", "DTEND:20261112T100000Z", "SEQUENCE:0", "ORGANIZER;CN=Me:mailto:me@pcb.test"),
		ical.SharedEncrypted:   frag(uid, "SUMMARY:Lunch", "DESCRIPTION:café 日本"),
		ical.CalendarSigned:    frag(uid, "STATUS:CONFIRMED", "TRANSP:OPAQUE"),
		ical.CalendarEncrypted: frag(uid, "COMMENT:private note"),
		ical.Attendees:         frag(uid, "ATTENDEE;CN=Ann;X-PM-TOKEN=tokA:mailto:ann@pcb.test"),
	}
}

func mustSeal(t *testing.T, parts map[ical.PartKind][]byte, k cards.SealKeys) events.WireEvent {
	t.Helper()
	we, err := cards.Seal(parts, k)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return we
}

func newSessionKey(t *testing.T) *crypto.SessionKey {
	t.Helper()
	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

// wrap is sk's key packet to kr, base64.
func wrap(t *testing.T, kr *crypto.KeyRing, sk *crypto.SessionKey) string {
	t.Helper()
	kp, err := kr.EncryptSessionKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(kp)
}

// unwrap opens a base64 key packet with kr.
func unwrap(b64 string, kr *crypto.KeyRing) (*crypto.SessionKey, error) {
	kp, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	return kr.DecryptSessionKey(kp)
}

// rowJSON is a row as Proton serves it, holding the sealed parts, author
// and key packets ("" leaves one out).
func rowJSON(t *testing.T, we events.WireEvent, author, sharedKP, calKP string) json.RawMessage {
	t.Helper()
	m := map[string]any{
		"ID": "row1", "Author": author, "SharedKeyPacket": sharedKP, "CalendarKeyPacket": calKP,
		"SharedEvents": we.SharedEventContent, "CalendarEvents": we.CalendarEventContent,
		"AttendeesEvents": we.AttendeesEventContent, "PersonalEvents": []any{},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// decrypt opens an encrypted part's Data with sk.
func decrypt(p events.Part, sk *crypto.SessionKey) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return nil, err
	}
	msg, err := sk.Decrypt(data)
	if err != nil {
		return nil, err
	}
	return msg.GetBinary(), nil
}

// types are the part Types of a list, in order.
func types(l events.PartList) []int {
	out := []int{}
	for _, p := range l {
		out = append(out, p.Type)
	}
	return out
}

// The ical codec and cards number the part kinds alike: the write flow
// passes cards.Open's parts to ical.Patch and its output to Seal.
func TestPartKindsMatchCardKinds(t *testing.T) {
	t.Parallel()
	want := []cards.Kind{cards.SharedSigned, cards.SharedEncrypted, cards.CalendarSigned, cards.CalendarEncrypted, cards.Attendees}
	for i, k := range writeKinds {
		if int(k) != int(want[i]) {
			t.Errorf("ical %s is %d, cards %s is %d", k, int(k), want[i], int(want[i]))
		}
	}
}

// randomFragment is a fragment in Proton's form holding up to six random
// lines, folded at 75 octets on rune boundaries, with multi-byte UTF-8,
// escapes, tabs and trailing spaces.
func randomFragment(rng *rand.Rand, uid string) []byte {
	names := []string{"SUMMARY", "DESCRIPTION", "LOCATION", "X-MOZ-GENERATION", "COMMENT", "ATTENDEE;CN=A;X-PM-TOKEN=t"}
	alphabet := []string{"a", "Z", "0", " ", `\n`, `\,`, ";", ":", "=", "\t", "é", "日", "\U0001F600"}
	var lines []string
	for range rng.IntN(7) {
		var b strings.Builder
		b.WriteString(names[rng.IntN(len(names))] + ":")
		for range rng.IntN(160) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		lines = append(lines, fold(b.String())...)
	}
	return frag(uid, lines...)
}

// hasLine reports whether a fragment from frag holds a line besides UID
// and DTSTAMP (nil: no fragment).
func hasLine(f []byte) bool { return bytes.Count(f, []byte("\r\n")) > 5 }

// fold splits a content line into lines of at most 75 octets, continuation
// lines starting with a space, never inside a rune.
func fold(line string) []string {
	var out []string
	limit := 75
	for len(line) > limit {
		cut := limit
		for !utf8.RuneStart(line[cut]) {
			cut--
		}
		out = append(out, line[:cut])
		line = " " + line[cut:]
	}
	return append(out, line)
}

// Open(Seal(x)) gives x back: each part sent, and no other, byte for byte,
// signed by us and verified, for creates (fresh keys whose packets open with
// the primary calendar key only) and updates (the given keys), over random
// fragment sets (seeded: no flakes).
func TestOpenSealRoundTrip(t *testing.T) {
	t.Parallel()
	r := newSealRig(t)
	rng := rand.New(rand.NewPCG(2026, 1111))
	for i := range 60 {
		uid := fmt.Sprintf("rt%d@pcb.test", i)
		parts := map[ical.PartKind][]byte{}
		for _, k := range writeKinds {
			if rng.IntN(4) != 0 {
				parts[k] = randomFragment(rng, uid)
			}
		}
		if len(parts) == 0 {
			parts[ical.SharedSigned] = randomFragment(rng, uid)
		}
		_, usesShared := parts[ical.SharedEncrypted]
		if _, ok := parts[ical.Attendees]; ok {
			usesShared = true
		}
		_, usesCal := parts[ical.CalendarEncrypted]
		// A key is proven ours by a part under it holding a random line (all
		// of them encrypted-only properties), not by UID and DTSTAMP alone.
		provenShared := hasLine(parts[ical.SharedEncrypted]) || hasLine(parts[ical.Attendees])
		provenCal := hasLine(parts[ical.CalendarEncrypted])

		// A create mints; an update gives every key a part uses, and maybe
		// one no part uses.
		k := r.create()
		if rng.IntN(2) == 0 {
			k.Update = true
			if usesShared || rng.IntN(2) == 0 {
				k.Shared = newSessionKey(t)
			}
			if usesCal || rng.IntN(2) == 0 {
				k.Calendar = newSessionKey(t)
			}
		}
		we := mustSeal(t, parts, k)

		// The row carries a key packet for each key a part uses: the one
		// Seal minted, else the given key's.
		rowPacket := func(given *crypto.SessionKey, minted string, used bool) string {
			switch {
			case !used:
				if minted != "" {
					t.Errorf("set %d: a key packet for a key no part uses", i)
				}
				return ""
			case given != nil:
				if minted != "" {
					t.Errorf("set %d: a key packet for a given key", i)
				}
				return wrap(t, r.calKR, given)
			}
			if minted == "" {
				t.Fatalf("set %d: no key packet for a minted key", i)
			}
			if _, err := unwrap(minted, r.retired); err == nil {
				t.Errorf("set %d: a minted key packet opens with the retired calendar key", i)
			}
			return minted
		}
		sharedKP := rowPacket(k.Shared, we.SharedKeyPacket, usesShared)
		calKP := rowPacket(k.Calendar, we.CalendarKeyPacket, usesCal)

		ev, err := cards.Open(rowJSON(t, we, me, sharedKP, calKP), r.calKR, nil, r.own)
		if err != nil {
			t.Fatalf("set %d: Open: %v", i, err)
		}
		if len(ev.Parts) != len(parts) {
			t.Fatalf("set %d: %d parts opened, %d sealed", i, len(ev.Parts), len(parts))
		}
		for _, p := range ev.Parts {
			want, ok := parts[ical.PartKind(p.Kind)]
			switch {
			case !ok:
				t.Errorf("set %d: a %s that was not sealed", i, p.Kind)
			case !bytes.Equal(p.Text, want):
				t.Errorf("set %d: %s opens as\n%q\nsealed\n%q", i, p.Kind, p.Text, want)
			case !p.Signed || !p.Own || !p.Verified || p.Author != me:
				t.Errorf("set %d: %s: Signed %v Own %v Verified %v Author %q", i, p.Kind, p.Signed, p.Own, p.Verified, p.Author)
			}
		}

		shared, cal, err := ev.SessionKeys()
		wantErr := (usesShared && !provenShared) || (usesCal && !provenCal)
		if errors.Is(err, cards.ErrUntrustedSessionKey) != wantErr || (err != nil && !wantErr) {
			t.Fatalf("set %d: SessionKeys error %v, want untrusted %v", i, err, wantErr)
		}
		wantShared, wantCal := usesShared && provenShared, usesCal && provenCal
		if (shared != nil) != wantShared || (cal != nil) != wantCal {
			t.Fatalf("set %d: SessionKeys gives shared %v calendar %v; want %v %v", i, shared != nil, cal != nil, wantShared, wantCal)
		}
		if k.Shared != nil && shared != nil && !bytes.Equal(shared.Key, k.Shared.Key) {
			t.Errorf("set %d: the shared key opened is not the one given", i)
		}
		if k.Calendar != nil && cal != nil && !bytes.Equal(cal.Key, k.Calendar.Key) {
			t.Errorf("set %d: the calendar key opened is not the one given", i)
		}
	}
}

// Every signature is a binary-mode (0x00) detached signature by the signer
// over the exact bytes: a signed part's Data is the fragment itself, an
// encrypted part's Data is one SEIPD packet whose plaintext is the
// fragment, and the same text with LF line ends (which a text-mode
// signature would accept) does not verify.
func TestSealSignsBinaryExactBytes(t *testing.T) {
	t.Parallel()
	r := newSealRig(t)
	parts := fullParts("bin@pcb.test")
	parts[ical.SharedEncrypted] = frag("bin@pcb.test", "SUMMARY:trailing space ", "DESCRIPTION:café")
	we := mustSeal(t, parts, r.create())

	shared, err := unwrap(we.SharedKeyPacket, r.calKR)
	if err != nil {
		t.Fatal(err)
	}
	calSK, err := unwrap(we.CalendarKeyPacket, r.calKR)
	if err != nil {
		t.Fatal(err)
	}
	groups := []struct {
		name  string
		list  events.PartList
		types []int
		kinds []ical.PartKind
		key   *crypto.SessionKey
	}{
		{"SharedEventContent", we.SharedEventContent, []int{2, 3}, []ical.PartKind{ical.SharedSigned, ical.SharedEncrypted}, shared},
		{"CalendarEventContent", we.CalendarEventContent, []int{2, 3}, []ical.PartKind{ical.CalendarSigned, ical.CalendarEncrypted}, calSK},
		// The attendees part is under the shared session key (research B.1).
		{"AttendeesEventContent", we.AttendeesEventContent, []int{3}, []ical.PartKind{ical.Attendees}, shared},
	}
	other := r.f.OtherAddressKeyRing()
	for _, g := range groups {
		if got := types(g.list); !slices.Equal(got, g.types) {
			t.Fatalf("%s holds Types %v, want %v", g.name, got, g.types)
		}
		for i, p := range g.list {
			want := parts[g.kinds[i]]
			text := []byte(p.Data)
			if p.Type == events.PartEncrypted {
				one(t, p.Data, g.key)
				if text, err = decrypt(p, g.key); err != nil {
					t.Fatalf("%s Type 3 does not decrypt with its session key: %v", g.name, err)
				}
			}
			if !bytes.Equal(text, want) {
				t.Errorf("%s Type %d carries\n%q\nwant the fragment\n%q", g.name, p.Type, text, want)
			}
			sig, err := crypto.NewPGPSignatureFromArmored(p.Signature)
			if err != nil {
				t.Fatalf("%s Type %d: the signature is not armored: %v", g.name, p.Type, err)
			}
			pk, err := packet.Read(bytes.NewReader(sig.GetBinary()))
			if s, ok := pk.(*packet.Signature); err != nil || !ok || s.SigType != packet.SigTypeBinary {
				t.Errorf("%s Type %d: not a binary-mode signature packet (%T, %v)", g.name, p.Type, pk, err)
			}
			if err := r.addr.VerifyDetached(crypto.NewPlainMessage(want), sig, 0); err != nil {
				t.Errorf("%s Type %d: the signature does not verify over the fragment: %v", g.name, p.Type, err)
			}
			lf := bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
			if r.addr.VerifyDetached(crypto.NewPlainMessage(lf), sig, 0) == nil {
				t.Errorf("%s Type %d: the signature also verifies the LF text (not over the exact bytes)", g.name, p.Type)
			}
			if other.VerifyDetached(crypto.NewPlainMessage(want), sig, 0) == nil {
				t.Errorf("%s Type %d: the signature verifies with another address's key", g.name, p.Type)
			}
		}
	}
	if _, err := decrypt(we.AttendeesEventContent[0], calSK); err == nil {
		t.Error("the attendees part decrypts with the calendar session key")
	}
}

// one checks that b64 is exactly one integrity-protected SEIPD packet (tag
// 18, not the legacy tag 9): the data packet only, no key packet before or
// after it, and under sk a literal data packet, not compressed.
func one(t *testing.T, b64 string, sk *crypto.SessionKey) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("Data is not base64: %v", err)
	}
	pr := packet.NewReader(bytes.NewReader(data))
	p, err := pr.Next()
	if err != nil {
		t.Fatalf("Data holds no packet: %v", err)
	}
	se, ok := p.(*packet.SymmetricallyEncrypted)
	if !ok || !se.IntegrityProtected {
		t.Fatalf("Data is a %T (integrity protected: %v), want a SEIPD packet", p, ok && se.IntegrityProtected)
	}
	cipher, err := sk.GetCipherFunc()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := se.Decrypt(cipher, sk.Key)
	if err != nil {
		t.Fatalf("the SEIPD packet does not decrypt: %v", err)
	}
	inner, err := packet.Read(rc)
	if lit, ok := inner.(*packet.LiteralData); err != nil || !ok {
		t.Errorf("the SEIPD packet holds a %T (%v), want literal data, not compressed", inner, err)
	} else if _, err := io.Copy(io.Discard, lit.Body); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil { // the rest, to the MDC
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("the SEIPD packet fails its integrity check: %v", err)
	}
	if p, err := pr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Data holds a packet after the SEIPD one: %T, %v", p, err)
	}
}

// packets counts the OpenPGP packets of a base64 key packet and returns the
// first.
func packets(t *testing.T, b64 string) (int, packet.Packet) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("key packet is not base64: %v", err)
	}
	pr := packet.NewReader(bytes.NewReader(data))
	var first packet.Packet
	n := 0
	for {
		p, err := pr.Next()
		if errors.Is(err, io.EOF) {
			return n, first
		}
		if err != nil {
			t.Fatalf("key packet: %v", err)
		}
		if n == 0 {
			first = p
		}
		n++
	}
}

// A create's key packets are one PKESK each, of a fresh AES-256 key, to the
// primary calendar key: they open with it and not with the retired key.
// Seal refuses a recipient ring of more than one key (one PKESK per key, a
// retired one among them) and a signer ring of more than one key (gopenpgp
// would sign with whichever comes first).
func TestSealKeyPacketOpensWithThePrimaryOnly(t *testing.T) {
	t.Parallel()
	r := newSealRig(t)
	parts := fullParts("kp@pcb.test")
	we := mustSeal(t, parts, r.create())

	var opened []*crypto.SessionKey
	for name, kp := range map[string]string{"SharedKeyPacket": we.SharedKeyPacket, "CalendarKeyPacket": we.CalendarKeyPacket} {
		if kp == "" {
			t.Fatalf("a create has no %s", name)
		}
		if n, p := packets(t, kp); n != 1 {
			t.Errorf("%s holds %d packets, want one PKESK", name, n)
		} else if _, ok := p.(*packet.EncryptedKey); !ok {
			t.Errorf("%s is a %T, want a PKESK", name, p)
		}
		sk, err := unwrap(kp, r.calKR)
		if err != nil {
			t.Fatalf("%s does not open with the primary calendar key: %v", name, err)
		}
		if sk.Algo != "aes256" || len(sk.Key) != 32 {
			t.Errorf("%s holds a %s key of %d bytes, want AES-256", name, sk.Algo, len(sk.Key))
		}
		if _, err := unwrap(kp, r.retired); err == nil {
			t.Errorf("%s opens with the retired calendar key", name)
		}
		opened = append(opened, sk)
	}
	if bytes.Equal(opened[0].Key, opened[1].Key) {
		t.Error("the shared and calendar session keys are the same key")
	}
	if _, err := cards.Open(rowJSON(t, we, me, we.SharedKeyPacket, we.CalendarKeyPacket), r.retired, nil, r.own); !errors.Is(err, cards.ErrUndecryptable) {
		t.Errorf("Open with the retired calendar key: %v, want ErrUndecryptable", err)
	}
	// A second create mints other keys.
	again, err := unwrap(mustSeal(t, parts, r.create()).SharedKeyPacket, r.calKR)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again.Key, opened[0].Key) || bytes.Equal(again.Key, opened[1].Key) {
		t.Error("a second create reuses a session key")
	}

	both, err := r.calKR.Copy()
	if err != nil {
		t.Fatal(err)
	}
	if err := both.AddKey(r.retired.GetKeys()[0]); err != nil {
		t.Fatal(err)
	}
	twoSigners, err := r.addr.Copy()
	if err != nil {
		t.Fatal(err)
	}
	if err := twoSigners.AddKey(r.f.OtherAddressKeyRing().GetKeys()[0]); err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		name  string
		parts map[ical.PartKind][]byte
		k     cards.SealKeys
	}{
		{"two recipient keys", parts, cards.SealKeys{Signer: r.addr, CalendarPrimary: both}},
		{"no recipient for a create", parts, cards.SealKeys{Signer: r.addr}},
		{"a create given a session key", parts, cards.SealKeys{Signer: r.addr, CalendarPrimary: r.calKR, Shared: newSessionKey(t)}},
		{"two signer keys", parts, cards.SealKeys{Signer: twoSigners, CalendarPrimary: r.calKR}},
		{"no signer", parts, cards.SealKeys{CalendarPrimary: r.calKR}},
		{"a personal part", map[ical.PartKind][]byte{ical.SharedSigned: frag("x"), ical.PartKind(cards.Personal): frag("x")}, r.create()},
		{"an unknown kind", map[ical.PartKind][]byte{ical.SharedSigned: frag("x"), ical.PartKind(-1): frag("x")}, r.create()},
		{"an empty fragment", map[ical.PartKind][]byte{ical.SharedSigned: {}}, r.create()},
	}
	for _, c := range refused {
		if we, err := cards.Seal(c.parts, c.k); err == nil {
			t.Errorf("%s: sealed (%d shared parts), want an error", c.name, len(we.SharedEventContent))
		}
	}
}

// An update gives the row's session keys: the parts are encrypted under
// them and no key packet is sent (research B.5), and no recipient is
// needed. An update never mints (ruling M4-R15): a part needing a key the
// row lacks is ErrNeedsNewSessionKey, while a missing key no part needs is
// fine. A kind absent from the input is not emitted; its list is [] on the
// wire.
func TestUpdateReusesSessionKeysAndSendsNoPacket(t *testing.T) {
	t.Parallel()
	r := newSealRig(t)
	parts := fullParts("up@pcb.test")
	shared, calSK := newSessionKey(t), newSessionKey(t)

	we := mustSeal(t, parts, cards.SealKeys{Signer: r.addr, CalendarPrimary: r.calKR, Shared: shared, Calendar: calSK, Update: true})
	raw, err := json.Marshal(we)
	if err != nil {
		t.Fatal(err)
	}
	if we.SharedKeyPacket != "" || we.CalendarKeyPacket != "" || bytes.Contains(raw, []byte("KeyPacket")) {
		t.Errorf("an update sends key packets: %s", raw)
	}
	for _, c := range []struct {
		p    events.Part
		sk   *crypto.SessionKey
		want []byte
	}{
		{we.SharedEventContent[1], shared, parts[ical.SharedEncrypted]},
		{we.CalendarEventContent[1], calSK, parts[ical.CalendarEncrypted]},
		{we.AttendeesEventContent[0], shared, parts[ical.Attendees]},
	} {
		if text, err := decrypt(c.p, c.sk); err != nil || !bytes.Equal(text, c.want) {
			t.Errorf("a part does not decrypt to its fragment under the given key: %v", err)
		}
	}

	// A key a part needs and the row lacks: refused, whichever key, with a
	// recipient at hand or not.
	for name, k := range map[string]cards.SealKeys{
		"calendar key missing":  {Signer: r.addr, CalendarPrimary: r.calKR, Shared: shared, Update: true},
		"shared key missing":    {Signer: r.addr, CalendarPrimary: r.calKR, Calendar: calSK, Update: true},
		"no keys, no recipient": {Signer: r.addr, Update: true},
	} {
		if we, err := cards.Seal(parts, k); !errors.Is(err, cards.ErrNeedsNewSessionKey) || we.SharedKeyPacket != "" || we.CalendarKeyPacket != "" {
			t.Errorf("%s: error %v, key packets %v %v; want ErrNeedsNewSessionKey and none", name, err, we.SharedKeyPacket != "", we.CalendarKeyPacket != "")
		}
	}

	// No calendar encrypted part: no calendar key is minted.
	noCal := map[ical.PartKind][]byte{}
	for k, v := range parts {
		if k != ical.CalendarEncrypted {
			noCal[k] = v
		}
	}
	none := mustSeal(t, noCal, cards.SealKeys{Signer: r.addr, CalendarPrimary: r.calKR, Shared: shared, Update: true})
	if none.SharedKeyPacket != "" || none.CalendarKeyPacket != "" {
		t.Errorf("no calendar encrypted part: key packets %q %q, want none", none.SharedKeyPacket, none.CalendarKeyPacket)
	}
	if got := types(none.CalendarEventContent); !slices.Equal(got, []int{2}) {
		t.Errorf("CalendarEventContent holds Types %v, want [2]", got)
	}

	// Only the shared signed part: the other lists are empty, sent as [].
	only := mustSeal(t, map[ical.PartKind][]byte{ical.SharedSigned: parts[ical.SharedSigned]}, cards.SealKeys{Signer: r.addr, Update: true})
	raw, err = json.Marshal(only)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"CalendarEventContent":[],"AttendeesEventContent":[]`)) || bytes.Contains(raw, []byte("null")) {
		t.Errorf("a shared-signed-only event encodes as %s", raw)
	}
	if got := types(only.SharedEventContent); !slices.Equal(got, []int{2}) {
		t.Errorf("SharedEventContent holds Types %v, want [2]", got)
	}
}

// Event.SessionKeys returns a key only when every encrypted part under it
// is ours and verified and at least one of them holds a property no signed
// part of ours holds (research G, ruling M4-R14): a substituted key packet
// would make the update readable by the server, which holds our cleartext
// signed parts and our signatures over them, and so can replay one as an
// encrypted part under a key it chose (review C1). Each key is judged alone;
// signed parts do not count; a key packet no encrypted part uses proves
// nothing; a row without a key packet has no key to reuse (nil, no error).
func TestUntrustedSessionKeyIsRefused(t *testing.T) {
	t.Parallel()
	r := newSealRig(t)
	const uid = "trust@pcb.test"
	parts := fullParts(uid)
	shared, calSK := newSessionKey(t), newSessionKey(t)
	keys := cards.SealKeys{Signer: r.addr, Shared: shared, Calendar: calSK, Update: true}
	we := mustSeal(t, parts, keys)
	keys.Signer = r.f.OtherAddressKeyRing()
	forged := mustSeal(t, parts, keys) // the same parts signed by another key
	keys.Signer = r.addr
	sharedKP, calKP := wrap(t, r.calKR, shared), wrap(t, r.calKR, calSK)

	// swap returns we with one part's signature taken from forged, or
	// removed (Type 1) with unsign.
	swap := func(group string, i int, unsign bool) events.WireEvent {
		c := we
		lists := map[string]*events.PartList{"shared": &c.SharedEventContent, "calendar": &c.CalendarEventContent, "attendees": &c.AttendeesEventContent}
		from := map[string]events.PartList{"shared": forged.SharedEventContent, "calendar": forged.CalendarEventContent, "attendees": forged.AttendeesEventContent}
		l := append(events.PartList(nil), *lists[group]...)
		if unsign {
			l[i].Type, l[i].Signature = 1, ""
		} else {
			l[i].Signature = from[group][i].Signature
		}
		*lists[group] = l
		return c
	}
	signedOnly := mustSeal(t, map[ical.PartKind][]byte{ical.SharedSigned: parts[ical.SharedSigned], ical.CalendarSigned: parts[ical.CalendarSigned]}, cards.SealKeys{Signer: r.addr})
	noCalPart := mustSeal(t, map[ical.PartKind][]byte{ical.SharedSigned: parts[ical.SharedSigned], ical.SharedEncrypted: parts[ical.SharedEncrypted], ical.CalendarSigned: parts[ical.CalendarSigned]}, keys)
	// Encrypted parts of ours holding UID and DTSTAMP only prove nothing...
	bare := mustSeal(t, map[ical.PartKind][]byte{
		ical.SharedSigned: parts[ical.SharedSigned], ical.SharedEncrypted: frag(uid),
		ical.CalendarSigned: parts[ical.CalendarSigned], ical.CalendarEncrypted: frag(uid),
	}, keys)
	// ...but one part under the key that does is enough.
	oneProves := mustSeal(t, map[ical.PartKind][]byte{
		ical.SharedSigned: parts[ical.SharedSigned], ical.SharedEncrypted: frag(uid), ical.Attendees: parts[ical.Attendees],
		ical.CalendarSigned: parts[ical.CalendarSigned], ical.CalendarEncrypted: parts[ical.CalendarEncrypted],
	}, keys)

	// The server's replay (review C1): our cleartext signed part, with our
	// signature, encrypted under a key it chose and wrapped to our calendar
	// key.
	replay := func(signed events.Part, evil *crypto.SessionKey) events.Part {
		data, err := evil.Encrypt(crypto.NewPlainMessage([]byte(signed.Data)))
		if err != nil {
			t.Fatal(err)
		}
		return events.Part{Type: events.PartEncrypted, Data: base64.StdEncoding.EncodeToString(data), Signature: signed.Signature}
	}
	evilShared, evilCal := newSessionKey(t), newSessionKey(t)
	evilKP, evilCalKP := wrap(t, r.calKR, evilShared), wrap(t, r.calKR, evilCal)
	attack := events.WireEvent{
		SharedEventContent:    events.PartList{we.SharedEventContent[0], replay(we.SharedEventContent[0], evilShared)},
		CalendarEventContent:  events.PartList{we.CalendarEventContent[0], replay(we.CalendarEventContent[0], evilCal)},
		AttendeesEventContent: events.PartList{},
	}
	// The same with a calendar signed part of UID and DTSTAMP only, which
	// the codec writes for an event with no STATUS or TRANSP.
	minimal := mustSeal(t, map[ical.PartKind][]byte{ical.CalendarSigned: frag(uid)}, cards.SealKeys{Signer: r.addr}).CalendarEventContent[0]
	attackMinimal := events.WireEvent{
		SharedEventContent:    events.PartList{we.SharedEventContent[0], replay(minimal, evilShared)},
		CalendarEventContent:  events.PartList{minimal, replay(minimal, evilCal)},
		AttendeesEventContent: events.PartList{},
	}

	cases := []struct {
		name                         string
		raw                          json.RawMessage
		wantShared, wantCal, wantErr bool
		allVerified                  bool // every part verifies: only the M4-R14 proof can refuse
	}{
		{"all ours and verified", rowJSON(t, we, me, sharedKP, calKP), true, true, false, true},
		{"replayed shared and calendar signed parts", rowJSON(t, attack, me, evilKP, evilCalKP), false, false, true, true},
		{"replayed UID-and-DTSTAMP calendar signed part", rowJSON(t, attackMinimal, me, evilKP, evilCalKP), false, false, true, true},
		{"encrypted parts of UID and DTSTAMP only", rowJSON(t, bare, me, sharedKP, calKP), false, false, true, true},
		{"one proving part under the key is enough", rowJSON(t, oneProves, me, sharedKP, calKP), true, true, false, true},
		{"a signed part forged: encrypted parts decide", rowJSON(t, swap("shared", 0, false), me, sharedKP, calKP), true, true, false, false},
		{"shared encrypted part forged", rowJSON(t, swap("shared", 1, false), me, sharedKP, calKP), false, true, true, false},
		{"attendees part forged", rowJSON(t, swap("attendees", 0, false), me, sharedKP, calKP), false, true, true, false},
		{"calendar encrypted part forged", rowJSON(t, swap("calendar", 1, false), me, sharedKP, calKP), true, false, true, false},
		{"calendar encrypted part unsigned", rowJSON(t, swap("calendar", 1, true), me, sharedKP, calKP), true, false, true, false},
		{"attendees part unsigned", rowJSON(t, swap("attendees", 0, true), me, sharedKP, calKP), false, true, true, false},
		{"not our author", rowJSON(t, we, "stranger@pcb.test", sharedKP, calKP), false, false, true, false},
		{"key packets no encrypted part uses", rowJSON(t, signedOnly, me, sharedKP, calKP), false, false, true, true},
		// No calendar key packet and no calendar encrypted part: nothing to
		// reuse, nothing untrusted.
		{"no calendar key at all", rowJSON(t, noCalPart, me, sharedKP, ""), true, false, false, true},
	}
	for _, c := range cases {
		ev, err := cards.Open(c.raw, r.calKR, nil, r.own)
		if err != nil {
			t.Fatalf("%s: Open: %v", c.name, err)
		}
		for _, p := range ev.Parts {
			if c.allVerified && !p.Verified {
				t.Fatalf("%s: the %s does not verify; the case needs every part verified", c.name, p.Kind)
			}
		}
		gotShared, gotCal, err := ev.SessionKeys()
		if errors.Is(err, cards.ErrUntrustedSessionKey) != c.wantErr || (err != nil && !c.wantErr) {
			t.Errorf("%s: error %v, want ErrUntrustedSessionKey %v", c.name, err, c.wantErr)
		}
		if c.wantShared != (gotShared != nil && bytes.Equal(gotShared.Key, shared.Key)) || (!c.wantShared && gotShared != nil) {
			t.Errorf("%s: shared key returned %v, want %v", c.name, gotShared != nil, c.wantShared)
		}
		if c.wantCal != (gotCal != nil && bytes.Equal(gotCal.Key, calSK.Key)) || (!c.wantCal && gotCal != nil) {
			t.Errorf("%s: calendar key returned %v, want %v", c.name, gotCal != nil, c.wantCal)
		}
		if err != nil && strings.Contains(err.Error(), "Lunch") {
			t.Errorf("%s: the error quotes the card: %v", c.name, err)
		}
	}

	// A shared key that reaches us through an AddressKeyPacket alone (no
	// SharedKeyPacket) is the row's shared key too, judged the same way.
	raw := editRaw(t, rowJSON(t, we, me, "", calKP), func(m map[string]any) {
		m["AddressKeyPacket"], m["AddressID"] = wrap(t, r.addr, shared), "addr1"
	})
	ev, err := cards.Open(raw, r.calKR, map[string]*crypto.KeyRing{"addr1": r.addr}, r.own)
	if err != nil {
		t.Fatal(err)
	}
	gotShared, _, err := ev.SessionKeys()
	if err != nil || gotShared == nil || !bytes.Equal(gotShared.Key, shared.Key) {
		t.Errorf("a shared key from an AddressKeyPacket: returned %v, error %v; want the key", gotShared != nil, err)
	}
}

// syncEntryEvent is a sync entry's Event: the sealed card plus the row
// fields Task 5 will add (Permissions, Attendees), as the fake requires.
type syncEntryEvent struct {
	events.WireEvent
	Permissions int
	Attendees   []any
}

// syncAnswer is the sync endpoint's answer.
type syncAnswer struct {
	Code      int
	Responses []struct {
		Index    int
		Response struct {
			Code  int
			Error string
			Event struct{ ID string }
		}
	}
}

// syncOne sends one entry to the calendar's sync endpoint through the API
// client and returns its code, error text and row ID.
func syncOne(t *testing.T, c *api.Client, f *protontest.Fake, calID string, entry map[string]any) (int, string, string) {
	t.Helper()
	var ans syncAnswer
	body := map[string]any{"MemberID": f.MemberID(calID), "Events": []any{entry}}
	if err := c.Put(t.Context(), "/calendar/v1/"+url.PathEscape(calID)+"/events/sync", body, &ans); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(ans.Responses) != 1 {
		t.Fatalf("sync: %d responses, want one", len(ans.Responses))
	}
	res := ans.Responses[0].Response
	return res.Code, res.Error, res.Event.ID
}

// authorOf opens raw with own knowing the row's author only.
func authorOf(t *testing.T, raw json.RawMessage, kr *crypto.KeyRing) func(string) *crypto.KeyRing {
	t.Helper()
	var row struct{ Author string }
	if err := json.Unmarshal(raw, &row); err != nil || row.Author == "" {
		t.Fatalf("the row has no Author: %v", err)
	}
	return func(email string) *crypto.KeyRing {
		if email == row.Author {
			return kr
		}
		return nil
	}
}

// The fake's sync endpoint, a strict oracle of Proton's rules (rulings
// M4-R2, M4-R3), takes what Seal makes: a create, then an update that
// reuses the opened session keys and sends no key packet (the fake refuses
// any here). Signed with the second address's key, a first-address
// calendar refuses the create (2001), while a second-address calendar takes
// it: the signer choice matters and Seal keeps it.
func TestFakeSyncAcceptsSealedEvents(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{Write: protontest.WriteOptions{RefuseNewKeyPacketsOnUpdate: true}})
	c := api.New(f.Session(t, f.Username), api.Options{
		BaseURL: f.URL, Limiter: rate.NewLimiter(rate.Inf, 1), HTTP: &http.Client{Timeout: 10 * time.Second},
	})
	cal := f.AddCalendar(f.Username, protontest.Calendar{Name: "W"})
	calKR, signer := f.CalendarKeyRing(cal), f.AddressKeyRing()
	parts := fullParts("contract@pcb.test")

	we := mustSeal(t, parts, cards.SealKeys{Signer: signer, CalendarPrimary: calKR})
	code, msg, id := syncOne(t, c, f, cal, map[string]any{"Overwrite": 0, "Event": syncEntryEvent{we, 1, []any{}}})
	if code != 1000 || id == "" {
		t.Fatalf("create: code %d (%s), row %q; want 1000 and a row", code, msg, id)
	}
	row, err := events.Get(t.Context(), c, cal, id)
	if err != nil {
		t.Fatal(err)
	}
	own := authorOf(t, row.Raw, signer)
	ev, err := cards.Open(row.Raw, calKR, nil, own)
	if err != nil {
		t.Fatalf("Open the created row: %v", err)
	}
	for _, p := range ev.Parts {
		if !bytes.Equal(p.Text, parts[ical.PartKind(p.Kind)]) || !p.Verified {
			t.Errorf("created %s: Verified %v, text\n%q", p.Kind, p.Verified, p.Text)
		}
	}
	shared, calSK, err := ev.SessionKeys()
	if err != nil || shared == nil || calSK == nil {
		t.Fatalf("SessionKeys of our own row: %v", err)
	}
	var before struct{ SharedKeyPacket, CalendarKeyPacket string }
	if err := json.Unmarshal(row.Raw, &before); err != nil {
		t.Fatal(err)
	}

	parts[ical.SharedEncrypted] = frag("contract@pcb.test", "SUMMARY:Dinner")
	up := mustSeal(t, parts, cards.SealKeys{Signer: signer, CalendarPrimary: calKR, Shared: shared, Calendar: calSK, Update: true})
	if code, msg, _ := syncOne(t, c, f, cal, map[string]any{"ID": id, "Event": syncEntryEvent{up, 1, []any{}}}); code != 1000 {
		t.Fatalf("update: code %d (%s), want 1000", code, msg)
	}
	row, err = events.Get(t.Context(), c, cal, id)
	if err != nil {
		t.Fatal(err)
	}
	var after struct{ SharedKeyPacket, CalendarKeyPacket string }
	if err := json.Unmarshal(row.Raw, &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("the update changed the row's key packets")
	}
	ev, err = cards.Open(row.Raw, calKR, nil, own)
	if err != nil {
		t.Fatalf("Open the updated row: %v", err)
	}
	if got := part(t, ev, cards.SharedEncrypted); !bytes.Equal(got.Text, parts[ical.SharedEncrypted]) || !got.Verified {
		t.Errorf("updated shared encrypted part: Verified %v, text %q", got.Verified, got.Text)
	}

	// The second address's key: refused on this calendar...
	wrong := mustSeal(t, parts, cards.SealKeys{Signer: f.OtherAddressKeyRing(), CalendarPrimary: calKR})
	if code, msg, _ := syncOne(t, c, f, cal, map[string]any{"Overwrite": 0, "Event": syncEntryEvent{wrong, 1, []any{}}}); code != 2001 ||
		!strings.Contains(msg, "signed using the address key") {
		t.Errorf("signed by the second address: code %d (%s), want 2001 and the address-key refusal", code, msg)
	}
	// ...and right on the second address's calendar, which refuses the first's.
	cal2 := f.AddCalendar(f.Username, protontest.Calendar{Name: "Second", ToSecondAddress: true})
	cal2KR := f.CalendarKeyRing(cal2)
	right := mustSeal(t, parts, cards.SealKeys{Signer: f.OtherAddressKeyRing(), CalendarPrimary: cal2KR})
	if code, msg, _ := syncOne(t, c, f, cal2, map[string]any{"Overwrite": 0, "Event": syncEntryEvent{right, 1, []any{}}}); code != 1000 {
		t.Errorf("second address's calendar, its own key: code %d (%s), want 1000", code, msg)
	}
	first := mustSeal(t, parts, cards.SealKeys{Signer: signer, CalendarPrimary: cal2KR})
	if code, _, _ := syncOne(t, c, f, cal2, map[string]any{"Overwrite": 0, "Event": syncEntryEvent{first, 1, []any{}}}); code != 2001 {
		t.Errorf("second address's calendar, the first address's key: code %d, want 2001", code)
	}
}
