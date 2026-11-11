// Package config loads config.toml (SPEC §14): strict TOML where an unknown
// key or an invalid value is refused, never silently replaced by a default.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// Defaults of the Proton identity.
const (
	DefaultBaseURL    = "https://mail-api.proton.me"
	DefaultAppVersion = "Other"
)

// Config is the content of config.toml.
type Config struct {
	Proton  Proton  `toml:"proton"`
	Secrets Secrets `toml:"secrets"`
}

// Proton is how pcb presents itself to Proton.
type Proton struct {
	BaseURL    string `toml:"base_url"`    // API host, https (http only on loopback)
	AppVersion string `toml:"app_version"` // x-pm-appversion header
}

// Secrets locates the key that seals the stored secrets.
type Secrets struct {
	KeyFile string `toml:"key_file"` // absolute path
}

// Path returns where config.toml lives.
func Path(d paths.Dirs) string { return filepath.Join(d.Config, "config.toml") }

// Defaults returns the configuration used when config.toml is absent or
// leaves a key out.
func Defaults(d paths.Dirs) Config {
	return Config{
		Proton:  Proton{BaseURL: DefaultBaseURL, AppVersion: DefaultAppVersion},
		Secrets: Secrets{KeyFile: filepath.Join(d.Config, "secrets.key")},
	}
}

// Load reads path over the defaults. A missing file gives the defaults; any
// other problem is an *exitcode.Error (MustAct, hint "fix <path>") whose
// message names the key or position at fault and never echoes a value.
func Load(path string, d paths.Dirs) (Config, error) {
	cfg := Defaults(d)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, refuse(path, fmt.Errorf("read %s: %w", path, err))
	}
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, refuse(path, describe(err, data))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, refuse(path, err)
	}
	return cfg, nil
}

func refuse(path string, err error) error {
	return &exitcode.Error{Code: exitcode.MustAct, Err: err, Hint: "fix " + path}
}

// describe turns a decoder error into a message with the key or the position
// but not the offending text, which could be a secret.
func describe(err error, data []byte) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		if len(strict.Errors) > 0 {
			return fmt.Errorf("unknown key %s", strings.Join(strict.Errors[0].Key(), "."))
		}
		return errors.New("unknown key")
	}
	var derr *toml.DecodeError
	if errors.As(err, &derr) {
		line, col := derr.Position()
		msg := fmt.Sprintf("invalid TOML at line %d, column %d", line, col)
		if key := keyAt(data, line); key != "" {
			msg += " (key " + key + ")"
		}
		return errors.New(msg)
	}
	return errors.New("invalid TOML")
}

// keyAt returns the text left of the "=" on the 1-based line, the key and
// never the value; "" when the line has no "=".
func keyAt(data []byte, line int) string {
	lines := strings.Split(string(data), "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	key, _, ok := strings.Cut(lines[line-1], "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}

func (c Config) validate() error {
	if err := validAppVersion(c.Proton.AppVersion); err != nil {
		return fmt.Errorf("proton.app_version %w", err)
	}
	if err := validBaseURL(c.Proton.BaseURL); err != nil {
		return fmt.Errorf("proton.base_url %w", err)
	}
	if !filepath.IsAbs(c.Secrets.KeyFile) {
		return errors.New("secrets.key_file must be an absolute path")
	}
	return nil
}

func validAppVersion(v string) error {
	if v == "" {
		return errors.New("must not be empty")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func validBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("must be an absolute URL with a host")
	}
	if u.User != nil {
		return errors.New("must not contain credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "127.0.0.1", "::1", "localhost":
			return nil
		}
		return errors.New("must be https (http only for 127.0.0.1, ::1 and localhost)")
	}
	return errors.New("must start with https://")
}
