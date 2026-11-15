package seal

// SetKeyDirOwner replaces the owner lookup of a new key's directory and
// returns a restore function.
func SetKeyDirOwner(f func(string) (string, error)) (restore func()) {
	old := keyDirOwner
	keyDirOwner = f
	return func() { keyDirOwner = old }
}

// ProtectWith is CryptProtectData of data with entropy and flags, as the
// DPAPI sealer calls it with its own.
func ProtectWith(data, entropy []byte, flags uint32) ([]byte, error) {
	return protectWith(data, entropy, flags)
}

// Unprotect is the DPAPI sealer's CryptUnprotectData (its entropy).
func Unprotect(blob []byte) ([]byte, error) { return unprotect(blob) }

// DPAPIEntropy is the entropy the DPAPI sealer protects its key with.
func DPAPIEntropy() []byte { return dpapiEntropy }
