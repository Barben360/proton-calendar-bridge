//go:build !windows

package seal

// protect is Windows' CryptProtectData: there is none here.
func protect([]byte) ([]byte, error) { return nil, ErrNoDPAPI }

// unprotect is Windows' CryptUnprotectData: there is none here.
func unprotect([]byte) ([]byte, error) { return nil, ErrNoDPAPI }
