// Package config loads and validates the server configuration from the
// environment. Validation errors name the offending variable and never echo
// its value, so a misplaced secret cannot leak through an error message.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// Transport selects how the MCP server is exposed.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
)

const (
	defaultHTTPAddr     = ":8080"
	defaultCryptoDBPath = "matrix-crypto.db"

	// maxRoomIDLen is the Matrix spec's 255-byte cap on room IDs.
	maxRoomIDLen = 255
)

var (
	// ErrRequired is wrapped by a VarError when a mandatory variable is unset.
	ErrRequired = errors.New("required but not set")
	// ErrInvalid is wrapped by a VarError when a variable has a malformed value.
	ErrInvalid = errors.New("invalid value")
)

// VarError reports a problem with one environment variable. Reason is a fixed
// string and never contains the variable's value.
type VarError struct {
	Var    string
	Reason string
	Err    error
}

func (e *VarError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("config: %s: %v", e.Var, e.Err)
	}
	return fmt.Sprintf("config: %s: %v: %s", e.Var, e.Err, e.Reason)
}

func (e *VarError) Unwrap() error { return e.Err }

// Config is the validated runtime configuration.
type Config struct {
	Homeserver       string
	UserID           string
	AccessToken      string
	E2EEEnabled      bool
	CryptoDBPath     string
	PickleKey        string
	TriggerWebhook   string
	AllowedSendRooms []string
	Transport        Transport
	HTTPAddr         string
}

// ReadOnly reports whether sending is disabled. It is true unless at least one
// room is allow-listed, so the default fails closed.
func (c *Config) ReadOnly() bool { return len(c.AllowedSendRooms) == 0 }

// SendAllowed reports whether sending to roomID is permitted.
func (c *Config) SendAllowed(roomID string) bool {
	for _, r := range c.AllowedSendRooms {
		if r == roomID {
			return true
		}
	}
	return false
}

// ValidRoomID reports whether roomID is a well-formed Matrix room ID: a "!"
// sigil, at most 255 bytes, and no whitespace, control characters, or "/",
// "?", "#". A ":server" part is deliberately not required, because bridges
// hand out room IDs without one.
func ValidRoomID(roomID string) bool {
	if len(roomID) < 2 || len(roomID) > maxRoomIDLen || roomID[0] != '!' {
		return false
	}
	for _, r := range roomID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
		switch r {
		case '/', '?', '#':
			return false
		}
	}
	return true
}

// String implements fmt.Stringer with secrets redacted.
func (c *Config) String() string {
	return fmt.Sprintf("Config{Homeserver:%s UserID:%s AccessToken:%s E2EEEnabled:%t CryptoDBPath:%s PickleKey:%s TriggerWebhook:%s AllowedSendRooms:%d Transport:%s HTTPAddr:%s}",
		c.Homeserver, c.UserID, redact(c.AccessToken), c.E2EEEnabled, c.CryptoDBPath, redact(c.PickleKey),
		redactURL(c.TriggerWebhook), len(c.AllowedSendRooms), c.Transport, c.HTTPAddr)
}

// GoString implements fmt.GoStringer so %#v cannot leak secrets either.
func (c *Config) GoString() string { return c.String() }

func redact(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<redacted>"
}

func redactURL(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<set>"
}

// Load reads the configuration from the process environment. All problems are
// reported together as a joined error whose members are *VarError values.
func Load() (*Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom is Load with an injectable environment lookup.
func LoadFrom(getenv func(string) string) (*Config, error) {
	var errs []error
	fail := func(name string, kind error, reason string) {
		errs = append(errs, &VarError{Var: name, Reason: reason, Err: kind})
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	required := func(name string) string {
		v := get(name)
		if v == "" {
			fail(name, ErrRequired, "")
		}
		return v
	}

	cfg := &Config{
		Homeserver:   required("MATRIX_HOMESERVER"),
		UserID:       required("MATRIX_USER_ID"),
		AccessToken:  required("MATRIX_ACCESS_TOKEN"),
		CryptoDBPath: get("MATRIX_CRYPTO_DB_PATH"),
		PickleKey:    get("MATRIX_PICKLE_KEY"),
		Transport:    TransportStdio,
		HTTPAddr:     defaultHTTPAddr,
	}

	if cfg.Homeserver != "" && !validHTTPURL(cfg.Homeserver) {
		fail("MATRIX_HOMESERVER", ErrInvalid, "must be an absolute http(s) URL")
	}
	if cfg.UserID != "" && !validUserID(cfg.UserID) {
		fail("MATRIX_USER_ID", ErrInvalid, "must look like @localpart:server")
	}

	if v := get("MATRIX_E2EE_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			fail("MATRIX_E2EE_ENABLED", ErrInvalid, "must be a boolean")
		}
		cfg.E2EEEnabled = b
	}
	if cfg.E2EEEnabled {
		if cfg.PickleKey == "" {
			fail("MATRIX_PICKLE_KEY", ErrRequired, "needed when MATRIX_E2EE_ENABLED is true")
		}
		if cfg.CryptoDBPath == "" {
			cfg.CryptoDBPath = defaultCryptoDBPath
		}
	}

	if v := get("TRIGGER_WEBHOOK_URL"); v != "" {
		if !validHTTPURL(v) {
			fail("TRIGGER_WEBHOOK_URL", ErrInvalid, "must be an absolute http(s) URL")
		}
		cfg.TriggerWebhook = v
	}

	for _, r := range strings.Split(get("ALLOWED_SEND_ROOMS"), ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !ValidRoomID(r) {
			fail("ALLOWED_SEND_ROOMS", ErrInvalid, "entries must be room IDs like !abcdef:example.com")
			break
		}
		cfg.AllowedSendRooms = append(cfg.AllowedSendRooms, r)
	}

	if v := strings.ToLower(get("MCP_TRANSPORT")); v != "" {
		switch Transport(v) {
		case TransportStdio, TransportHTTP:
			cfg.Transport = Transport(v)
		default:
			fail("MCP_TRANSPORT", ErrInvalid, "must be stdio or http")
		}
	}
	if v := get("HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func validHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func validUserID(s string) bool {
	if !strings.HasPrefix(s, "@") {
		return false
	}
	local, server, ok := strings.Cut(s[1:], ":")
	return ok && local != "" && server != ""
}
