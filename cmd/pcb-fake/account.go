package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
)

// account is the fake's test account, read from the --account file: the
// password and the TOTP secret never come from argv.
type account struct {
	Username   string `toml:"username"`    // default "pcbfake"
	Password   string `toml:"password"`    // required
	TOTPSecret string `toml:"totp_secret"` // base32; set: the account has 2FA
}

const defaultUsername = "pcbfake"

// readAccount reads path: a regular file only its owner can read or write
// (mode 0600 or stricter), strict TOML. Its errors never echo a value.
func readAccount(path string) (account, error) {
	fix := func(err error, hint string) error {
		return &exitcode.Error{Code: exitcode.MustAct, Err: err, Hint: hint}
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return account{}, fix(fmt.Errorf("account file: %w", err), "write it: username, password and totp_secret, mode 0600")
	}
	if !fi.Mode().IsRegular() {
		return account{}, fix(fmt.Errorf("account file %s is not a regular file", path), "use a plain file, mode 0600")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return account{}, fix(fmt.Errorf("account file %s has mode %#o: others can read it", path, perm), "chmod 600 "+path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return account{}, fix(fmt.Errorf("account file: %w", err), "make it readable by this user, mode 0600")
	}
	var a account
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return account{}, fix(fmt.Errorf("account file %s: unknown key", path), "keep username, password and totp_secret only")
		}
		return account{}, fix(fmt.Errorf("account file %s is not valid TOML", path), `write lines such as password = "..."`)
	}
	if a.Password == "" {
		return account{}, fix(fmt.Errorf("account file %s has no password", path), `add password = "..."`)
	}
	if a.Username == "" {
		a.Username = defaultUsername
	}
	if a.TOTPSecret != "" {
		if _, err := otp.Parse("otpauth://totp/pcb-fake?secret=" + a.TOTPSecret); err != nil {
			return account{}, fix(fmt.Errorf("account file %s: totp_secret is not a base32 secret", path), "use a base32 secret, or leave it out for no 2FA")
		}
	}
	return a, nil
}
