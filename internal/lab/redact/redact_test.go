package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

const armor = "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQdGBF9abcdefghijklmnop\n=abcd\n-----END PGP PRIVATE KEY BLOCK-----"

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, b)
	}
	return m
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A fixture shaped like /auth/v4 and /calendar/v1/{id}/events answers.
func TestRedactNeverLeaksKnownSecretFields(t *testing.T) {
	secrets := map[string]string{
		"AccessToken": "ACCESSTOKENVALUE-s3cr3t", "RefreshToken": "REFRESHVALUE-s3cr3t",
		"UID": "uidvalue-s3cr3t", "ServerProof": "serverproofvalue-s3cr3t",
		"ClientProof": "clientproofvalue-s3cr3t", "ClientEphemeral": "clientephvalue-s3cr3t",
		"SRPSession": "srpsessionvalue-s3cr3t", "Salt": "saltvalue-s3cr3t",
		"KeySalt": "keysaltvalue-s3cr3t", "PrivateKey": armor,
		"Passphrase": "passphrasevalue-s3cr3t", "Token": "tokenvalue-s3cr3t",
		"HumanVerificationToken": "hvvalue-s3cr3t", "SessionKey": "sessionkeyvalue-s3cr3t",
		"KeyPacket": "keypacketvalue-s3cr3t", "SharedKeyPacket": "sharedkpvalue-s3cr3t",
		"CalendarKeyPacket": "calkpvalue-s3cr3t", "Data": "datavalue-s3cr3t",
		"Signature": "-----BEGIN PGP SIGNATURE-----\n\nsigsig\n-----END PGP SIGNATURE-----",
		"Armored":   "armoredvalue-s3cr3t", "TOTP": "totpvalue-s3cr3t",
		"Password": "hunter2-s3cr3t", "Email": "jane@example.test", "Address": "addrvalue-s3cr3t",
	}
	// Auth answer: every listed key at top level, with odd casing for some.
	auth := map[string]any{"Code": 1000, "Scopes": []string{"calendar"}}
	for k, v := range secrets {
		key := k
		if len(k)%2 == 0 {
			key = strings.ToLower(k) // case-insensitive match
		}
		auth[key] = v
	}
	// Events answer: secrets nested in objects inside arrays.
	events := map[string]any{
		"Code": 1000,
		"Events": []any{map[string]any{
			"ID":         "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIj==",
			"CalendarID": "cal-1", "SharedEvents": []any{map[string]any{
				"Type": 2, "Data": "datainarray-s3cr3t", "Signature": "siginarray-s3cr3t",
				"SharedKeyPacket": "skpinarray-s3cr3t",
			}},
			"CalendarEvents": []any{map[string]any{"Data": "calarray-s3cr3t", "Author": map[string]any{"Email": "nested@example.test"}}},
		}},
	}
	for name, doc := range map[string]any{"auth": auth, "events": events} {
		out, err := JSON(mustJSON(t, doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := string(out)
		for k, v := range secrets {
			for _, frag := range strings.Split(v, "\n") {
				if len(frag) > 8 && strings.Contains(s, frag) {
					t.Errorf("%s: value of %s leaked: %q\n%s", name, k, frag, s)
				}
			}
		}
		for _, leak := range []string{"datainarray", "siginarray", "skpinarray", "calarray", "nested@example"} {
			if strings.Contains(s, leak) {
				t.Errorf("%s: nested secret %q leaked", name, leak)
			}
		}
	}

	out, _ := JSON(mustJSON(t, auth))
	m := parse(t, out)
	if m["Code"] != float64(1000) {
		t.Errorf("Code must stay, got %v", m["Code"])
	}
	if m["AccessToken"] != "<redacted:len=23>" {
		t.Errorf("AccessToken = %v", m["AccessToken"])
	}

	out, _ = JSON(mustJSON(t, events))
	ev := parse(t, out)["Events"].([]any)[0].(map[string]any)
	if ev["CalendarID"] != "cal-1" {
		t.Errorf("CalendarID must stay, got %v", ev["CalendarID"])
	}
	// R10: keys ending in ID keep their value even when it looks like long base64.
	if !strings.HasPrefix(ev["ID"].(string), "AbCdEf") {
		t.Errorf("event ID must stay, got %v", ev["ID"])
	}
}

func TestRedactCatchesArmorAnywhere(t *testing.T) {
	doc := map[string]any{
		"Code":    1000,
		"Unknown": armor,
		"List":    []any{"fine", "prefix text\n" + armor},
		"Deep":    map[string]any{"x": []any{map[string]any{"Whatever": armor}}},
	}
	out, err := JSON(mustJSON(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"BEGIN PGP", "lQdGBF9", "=abcd", "END PGP"} {
		if strings.Contains(string(out), frag) {
			t.Errorf("armor fragment %q leaked:\n%s", frag, out)
		}
	}
	if !strings.Contains(string(out), "fine") {
		t.Error("harmless string must stay")
	}
}

func TestRedactHeuristics(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	b64 := strings.Repeat("QUJD", 10) // 40 chars
	wrapped := strings.Repeat("QUJD", 5) + "\n" + strings.Repeat("QUJD", 5)
	doc := map[string]any{
		"A": jwt, "B": b64, "C": wrapped,
		"Short": "QUJDQUJD", "Text": "Team meeting about the quarterly plan, room 4",
		"Num": 12345, "Bool": true, "Nil": nil, "Error": "Invalid access token",
		"ParentID": b64, "UID": b64,
	}
	out, err := JSON(mustJSON(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, out)
	for _, k := range []string{"A", "B", "C", "UID"} {
		if !strings.HasPrefix(m[k].(string), "<redacted:len=") {
			t.Errorf("%s not redacted: %v", k, m[k])
		}
	}
	if m["ParentID"] != b64 {
		t.Errorf("ID-suffixed key must keep base64 value (R10), got %v", m["ParentID"])
	}
	for k, want := range map[string]any{"Short": "QUJDQUJD", "Text": "Team meeting about the quarterly plan, room 4",
		"Num": float64(12345), "Bool": true, "Nil": nil, "Error": "Invalid access token"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}

func TestRedactNonStringSecretValuesAndDeterminism(t *testing.T) {
	in := []byte(`{"b":1,"TOTP":123456,"Password":{"a":"x"},"Address":[{"Email":"a@b.c"}],"a":2}`)
	out1, err := JSON(in)
	if err != nil {
		t.Fatal(err)
	}
	out2, _ := JSON(in)
	if string(out1) != string(out2) {
		t.Error("output not deterministic")
	}
	s := string(out1)
	for _, leak := range []string{"123456", "a@b.c", `"x"`} {
		if strings.Contains(s, leak) {
			t.Errorf("%q leaked: %s", leak, s)
		}
	}
	if strings.Index(s, `"a"`) > strings.Index(s, `"b"`) {
		t.Errorf("keys not sorted: %s", s)
	}
}

func TestRedactRejectsInvalidJSON(t *testing.T) {
	for _, in := range []string{``, `not json`, `{"a":"b"} trailing`, `{"a":`, `{"a":1}{"Token":"leak-xyz"}`} {
		out, err := JSON([]byte(in))
		if err == nil {
			t.Errorf("%q accepted, output %s", in, out)
		}
		if out != nil {
			t.Errorf("%q: output must be nil on error", in)
		}
	}
}

func TestRedactMultilineValueIsOneValue(t *testing.T) {
	// A private key split over lines must be redacted as a whole, under any key.
	raw := "{\"Whatever\":\"line1secretpart\\nline2secretpart\\n-----BEGIN PGP PRIVATE KEY BLOCK-----\\nxyz\"}"
	out, err := JSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secretpart") {
		t.Errorf("leaked: %s", out)
	}
}

func TestRedactFailClosedKeysAndValues(t *testing.T) {
	hex32 := "9f86d081884c7d659a2feaa0c55ad015"
	doc := map[string]any{
		"Foo": hex32, "Username": "jane", "TwoFactorCode": "123456", "Secret": "s",
		"Cookie": "c", "RefreshTokenExpiry": "short", "SrpSalt": "x", "MyPassword2": "y",
		"Code": 1000, "Error": "Invalid access token", "CalendarID": "cal-1",
		"Armory": "-----BEGIN PGP MESSAGE-----\n\nabc\n=abcd\n-----END PGP MESSAGE-----",
		"Crc":    "=abcd=efgh=ijkl=mnop=qrst",
		"Plain":  "short text", "Num": 7,
	}
	m := parse(t, func() []byte {
		o, err := JSON(mustJSON(t, doc))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}())
	for _, k := range []string{"Foo", "Username", "TwoFactorCode", "Secret", "Cookie", "RefreshTokenExpiry", "SrpSalt", "MyPassword2", "Armory", "Crc"} {
		if s, _ := m[k].(string); !strings.HasPrefix(s, "<redacted:len=") {
			t.Errorf("%s not redacted: %v", k, m[k])
		}
	}
	for k, want := range map[string]any{"Code": float64(1000), "Error": "Invalid access token", "CalendarID": "cal-1", "Plain": "short text", "Num": float64(7)} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}

func TestRedactTwentyCharThreshold(t *testing.T) {
	m := parse(t, func() []byte {
		o, err := JSON([]byte(`{"A":"` + strings.Repeat("a", 20) + `","B":"` + strings.Repeat("a", 19) + `","XID":"` + strings.Repeat("a", 30) + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}())
	if !strings.HasPrefix(m["A"].(string), "<redacted") || m["B"] != strings.Repeat("a", 19) || m["XID"] != strings.Repeat("a", 30) {
		t.Errorf("got %v", m)
	}
}

func TestRedactHidesAuthorEmail(t *testing.T) {
	out, err := JSON([]byte(`{"Code":1000,"CalendarEvents":[{"Type":2,"Author":"jane@example.test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "jane@example.test") || !strings.Contains(string(out), "<redacted:len=17>") {
		t.Errorf("Author must be redacted whole: %s", out)
	}
}
