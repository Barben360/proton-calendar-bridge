// Package config loads config.toml (SPEC §14): strict TOML where an unknown
// key or an invalid value is refused, never silently replaced by a default.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// Defaults of the Proton identity.
const (
	DefaultBaseURL    = "https://mail-api.proton.me"
	DefaultAppVersion = "Other"
	// DefaultListen is where pcb serve listens for CalDAV: loopback only.
	DefaultListen = "127.0.0.1:1080"
)

// Config is the content of config.toml.
type Config struct {
	Proton  Proton  `toml:"proton"`
	Secrets Secrets `toml:"secrets"`
	CalDAV  CalDAV  `toml:"caldav"`
	Log     Log     `toml:"log"`
	MCP     MCP     `toml:"mcp"`
}

// MCP is the MCP adapter's configuration (design §5.9).
type MCP struct {
	Remote   bool   `toml:"remote"`   // serve /mcp on the [caldav] listener, for token holders
	Timezone string `toml:"timezone"` // IANA zone for times given without one; empty: the process's zone
}

// Location is the zone of Timezone, the process's zone when it is empty
// (Load validated it; one that no longer loads is the process's too).
func (m MCP) Location() *time.Location {
	if m.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// validate refuses a zone that is not an IANA name, without echoing it.
// "Local" loads as the process's zone and a path loads a file: neither is
// a zone name.
func (m MCP) validate() error {
	if m.Timezone == "" {
		return nil
	}
	if _, err := time.LoadLocation(m.Timezone); err != nil || m.Timezone == "Local" || strings.HasPrefix(m.Timezone, "/") {
		return errors.New("mcp.timezone must be an IANA zone name, such as Europe/Paris, or empty for the process's zone")
	}
	return nil
}

// Log is the logging of pcb serve.
type Log struct {
	Level string `toml:"level"` // debug, info, warn or error (default info); pcb serve --log-level overrides it
}

// ParseLevel maps a level name (lower case only) to its slog level. The
// error never echoes s.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, errors.New("must be one of debug, info, warn, error")
}

// CalDAV is the listener of pcb serve. A loopback bind serves plain HTTP;
// any other bind serves TLS, with TLSCert and TLSKey (set together) or, when
// both are empty, a self-signed certificate pcb generates and keeps.
type CalDAV struct {
	Listen    string   `toml:"listen"`    // HOST:PORT, the host explicit (0.0.0.0 or [::] for every interface)
	Hostnames []string `toml:"hostnames"` // names clients use, put in the generated certificate; the first one is in the printed URL
	TLSCert   string   `toml:"tls_cert"`  // absolute path of a PEM certificate (chain)
	TLSKey    string   `toml:"tls_key"`   // absolute path of its PEM private key
}

// Proton is how pcb presents itself to Proton.
type Proton struct {
	BaseURL    string `toml:"base_url"`    // API host, https (http only on loopback)
	AppVersion string `toml:"app_version"` // x-pm-appversion header
}

// Secrets locates the key that seals the stored secrets.
type Secrets struct {
	KeyFile string `toml:"key_file"` // absolute path
	Backend string `toml:"backend"`  // auto (default), keyfile or secret-service: what seals the secrets of a new state
}

// Path returns where config.toml lives.
func Path(d paths.Dirs) string { return filepath.Join(d.Config, "config.toml") }

// Defaults returns the configuration used when config.toml is absent or
// leaves a key out.
func Defaults(d paths.Dirs) Config {
	return Config{
		Proton:  Proton{BaseURL: DefaultBaseURL, AppVersion: DefaultAppVersion},
		Secrets: Secrets{KeyFile: filepath.Join(d.Config, "secrets.key"), Backend: "auto"},
		CalDAV:  CalDAV{Listen: DefaultListen},
		Log:     Log{Level: "info"},
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
	switch c.Secrets.Backend {
	case "auto", "keyfile", "secret-service":
	default:
		return errors.New("[secrets] backend must be auto, keyfile or secret-service")
	}
	if _, err := ParseLevel(c.Log.Level); err != nil {
		return fmt.Errorf("log.level %w", err)
	}
	if err := c.MCP.validate(); err != nil {
		return err
	}
	return c.CalDAV.validate()
}

func (c CalDAV) validate() error {
	if err := ValidateListen(c.Listen); err != nil {
		return fmt.Errorf("caldav.listen %w", err)
	}
	if c.Hostnames != nil && len(c.Hostnames) == 0 {
		return errors.New("caldav.hostnames must not be an empty list (leave the key out instead)")
	}
	for i, h := range c.Hostnames {
		if net.ParseIP(h) == nil && !validDNSName(h) {
			return fmt.Errorf("caldav.hostnames entry %d must be a DNS name or an IP address", i+1)
		}
	}
	switch {
	case c.TLSCert != "" && c.TLSKey == "":
		return errors.New("caldav.tls_key must be set with caldav.tls_cert")
	case c.TLSKey != "" && c.TLSCert == "":
		return errors.New("caldav.tls_cert must be set with caldav.tls_key")
	case c.TLSCert != "" && !filepath.IsAbs(c.TLSCert):
		return errors.New("caldav.tls_cert must be an absolute path")
	case c.TLSKey != "" && !filepath.IsAbs(c.TLSKey):
		return errors.New("caldav.tls_key must be an absolute path")
	}
	return nil
}

// ValidateListen checks a listen address the way [caldav] listen is checked
// (pcb serve --listen uses it too): HOST:PORT, the host an IP address or a
// DNS name (never empty: 0.0.0.0 or [::] say "every interface"), the port
// 0 to 65535 (0: any free port). The message never echoes the value.
func ValidateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be HOST:PORT, such as 127.0.0.1:1080")
	}
	if host == "" {
		return errors.New("must name its host (0.0.0.0 or [::] for every interface)")
	}
	if net.ParseIP(host) == nil && !validDNSName(host) {
		return errors.New("must have an IP address or a DNS name as its host")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || strings.ContainsAny(port, "+-") {
		return errors.New("must have a port number from 0 to 65535")
	}
	return nil
}

// validDNSName reports whether s is a DNS name: dot-separated labels of
// 1 to 63 letters, digits or hyphens, no label starting or ending with a
// hyphen, 253 characters at most.
func validDNSName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
			if !alnum && r != '-' {
				return false
			}
		}
	}
	return true
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
