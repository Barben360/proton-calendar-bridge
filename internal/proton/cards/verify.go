package cards

import "github.com/ProtonMail/gopenpgp/v2/crypto"

// verify reports whether armoredSig is a valid detached signature of text by
// a key of kr (nil kr: not ours, so not verifiable). The signature covers
// the plaintext, binary or text mode alike: the exact bytes are checked, not
// a re-canonicalised string. Time is not checked (verifyTime 0): an old event
// signed with a key since expired still verifies.
func verify(text []byte, armoredSig string, kr *crypto.KeyRing) bool {
	if kr == nil || armoredSig == "" {
		return false
	}
	sig, err := crypto.NewPGPSignatureFromArmored(armoredSig)
	if err != nil {
		return false
	}
	return kr.VerifyDetached(crypto.NewPlainMessage(text), sig, 0) == nil
}
