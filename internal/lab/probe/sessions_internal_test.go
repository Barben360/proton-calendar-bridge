package probe

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRevokeAnswerNeverHoldsTheWholeUID(t *testing.T) {
	t.Parallel()
	si := SessionInfo{ID: "abcd… (24 chars)", uid: "abcdefghijklmnopqrstuvwx"}
	a := revokeAnswer(errors.New("DELETE /auth/v4/sessions/abcdefghijklmnopqrstuvwx: refresh tokens: proton session ended"), si)
	if strings.Contains(a.Note, si.uid) || !strings.Contains(a.Note, si.ID) || a.Status != 0 {
		t.Errorf("note %q (status %d): want the redacted ID in place of the UID, and no status", a.Note, a.Status)
	}
}

func TestMessageCutsOnARuneBoundary(t *testing.T) {
	t.Parallel()
	m := message(strings.Repeat("é", 200))
	if !utf8.ValidString(m) || utf8.RuneCountInString(m) != maxMessage+1 || !strings.HasSuffix(m, "…") {
		t.Errorf("message = %q (%d runes, valid %v); want %d runes and an ellipsis", m, utf8.RuneCountInString(m), utf8.ValidString(m), maxMessage)
	}
}
