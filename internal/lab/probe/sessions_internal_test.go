package probe

import (
	"errors"
	"strings"
	"testing"
)

func TestRevokeAnswerNeverHoldsTheWholeUID(t *testing.T) {
	t.Parallel()
	si := SessionInfo{ID: "abcd… (24 chars)", uid: "abcdefghijklmnopqrstuvwx"}
	a := revokeAnswer(errors.New("DELETE /auth/v4/sessions/abcdefghijklmnopqrstuvwx: refresh tokens: proton session ended"), si)
	if strings.Contains(a.Note, si.uid) || !strings.Contains(a.Note, si.ID) || a.Status != 0 {
		t.Errorf("note %q (status %d): want the redacted ID in place of the UID, and no status", a.Note, a.Status)
	}
}
