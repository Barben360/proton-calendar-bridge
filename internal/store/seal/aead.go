package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// sealAEAD encrypts plaintext under key with AES-256-GCM, authenticating
// aad. The output is nonce || ciphertext, the format both sealers store.
func sealAEAD(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize(), gcm.NonceSize()+len(plaintext)+gcm.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("random nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// openAEAD decrypts what sealAEAD produced; any failure is ErrCorrupt.
func openAEAD(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrCorrupt
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrCorrupt
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
