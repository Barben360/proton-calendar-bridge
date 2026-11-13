package output_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, b)
	}
	return m
}

func TestJSONEnvelopeOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(&buf, "version", map[string]string{"version": "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	m := decode(t, buf.Bytes())
	if m["schema_version"] != float64(2) || m["command"] != "version" || m["ok"] != true {
		t.Fatalf("bad envelope: %v", m)
	}
	if m["data"].(map[string]any)["version"] != "dev" {
		t.Fatalf("bad data: %v", m["data"])
	}
	if _, has := m["error"]; has {
		t.Fatalf("success envelope has an error key: %v", m)
	}
}

func TestJSONEnvelopeCarriesTypedError(t *testing.T) {
	var buf bytes.Buffer
	err := &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("solve the captcha"), Hint: "open the URL"}
	if werr := output.Write(&buf, "login", nil, err); werr != nil {
		t.Fatal(werr)
	}
	m := decode(t, buf.Bytes())
	e := m["error"].(map[string]any)
	if m["ok"] != false || e["code"] != float64(10) || e["message"] != "solve the captcha" || e["hint"] != "open the URL" {
		t.Fatalf("bad error envelope: %v", m)
	}
	if _, has := m["data"]; has {
		t.Fatalf("error envelope has a data key: %v", m)
	}
}

func TestJSONEnvelopePlainErrorIsBlocked(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(&buf, "x", nil, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	e := decode(t, buf.Bytes())["error"].(map[string]any)
	if e["code"] != float64(20) || e["message"] != "boom" {
		t.Fatalf("bad error: %v", e)
	}
	if _, has := e["hint"]; has {
		t.Fatalf("empty hint was serialised: %v", e)
	}
}
