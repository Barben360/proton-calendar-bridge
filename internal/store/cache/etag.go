package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ETag is the entity tag of a raw Proton row: "v1-" plus the first 32 hex
// characters of the SHA-256 of its canonical JSON (decoded with UseNumber so
// numbers keep their digits, re-encoded with sorted keys and no HTML
// escaping). Two encodings of one object share a tag.
func ETag(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("canonicalise row: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("canonicalise row: trailing data after the JSON value")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("canonicalise row: %w", err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return `"v1-` + hex.EncodeToString(sum[:])[:32] + `"`, nil
}
