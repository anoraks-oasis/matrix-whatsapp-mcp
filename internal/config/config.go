// Package config loads and validates the server configuration from the
// environment. Validation errors name the offending variable and never echo
// its value, so a misplaced secret cannot leak through an error message.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Transport selects how the MCP server is exposed.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
)

const (
	defaultHTTPAddr         = ":8080"
	defaultCryptoDBPath     = "matrix-crypto.db"
	defaultBridgeAckTimeout = time.Second

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
	// AutoJoinInvites makes the client accept room invitations without a
	// manual join.
	AutoJoinInvites bool
	// InviteAllowlist restricts AutoJoinInvites to invitations from these
	// user IDs. Empty means accept invitations from anyone.
	InviteAllowlist []string
	// AllowlistFile is the room-ID file unioned into AllowedSendRooms. It is
	// always a path; the file itself need not exist.
	AllowlistFile string
	// BridgeAckTimeout is how long SendText waits for a bridge notice that
	// reports the message was not bridged. Zero disables the wait.
	BridgeAckTimeout time.Duration
}

// ReadOnly reports whether sending is disabled by the environment allow-list
// alone. The server's effective allow-list also includes AllowlistFile, so this
// is only a lower bound: config.ReadOnly can be true while a file-added room is
// still sendable.
func (c *Config) ReadOnly() bool { return len(c.AllowedSendRooms) == 0 }

// InviteAllowed reports whether an invitation from userID may be accepted
// automatically. An empty allow-list accepts any inviter; entries are compared
// case-insensitively because the localpart is what people get wrong when
// copying a user ID by hand.
func (c *Config) InviteAllowed(userID string) bool {
	if len(c.InviteAllowlist) == 0 {
		return true
	}
	for _, allowed := range c.InviteAllowlist {
		if strings.EqualFold(allowed, userID) {
			return true
		}
	}
	return false
}

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
	return fmt.Sprintf("Config{Homeserver:%s UserID:%s AccessToken:%s E2EEEnabled:%t CryptoDBPath:%s PickleKey:%s TriggerWebhook:%s AllowedSendRooms:%d Transport:%s HTTPAddr:%s BridgeAckTimeout:%s AutoJoinInvites:%t InviteAllowlist:%d AllowlistFile:%s}",
		c.Homeserver, c.UserID, redact(c.AccessToken), c.E2EEEnabled, c.CryptoDBPath, redact(c.PickleKey),
		redactURL(c.TriggerWebhook), len(c.AllowedSendRooms), c.Transport, c.HTTPAddr, c.BridgeAckTimeout,
		c.AutoJoinInvites, len(c.InviteAllowlist), c.AllowlistFile)
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
		Homeserver:    required("MATRIX_HOMESERVER"),
		UserID:        required("MATRIX_USER_ID"),
		AccessToken:   required("MATRIX_ACCESS_TOKEN"),
		CryptoDBPath:  get("MATRIX_CRYPTO_DB_PATH"),
		PickleKey:     get("MATRIX_PICKLE_KEY"),
		Transport:     TransportStdio,
		HTTPAddr:      defaultHTTPAddr,
		AllowlistFile: allowlistFilePath(getenv),

		BridgeAckTimeout: defaultBridgeAckTimeout,
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

	if v := get("MATRIX_BRIDGE_ACK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			fail("MATRIX_BRIDGE_ACK_TIMEOUT", ErrInvalid, "must be a Go duration such as 1s or 500ms, or 0 to disable")
		} else {
			cfg.BridgeAckTimeout = d
		}
	}

	if v := get("MATRIX_AUTO_JOIN_INVITES"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			fail("MATRIX_AUTO_JOIN_INVITES", ErrInvalid, "must be a boolean")
		}
		cfg.AutoJoinInvites = b
	}

	for _, u := range strings.Split(get("MATRIX_INVITE_ALLOWLIST"), ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !validUserID(u) {
			fail("MATRIX_INVITE_ALLOWLIST", ErrInvalid, "entries must be user IDs like @alice:example.com")
			break
		}
		cfg.InviteAllowlist = append(cfg.InviteAllowlist, u)
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

// allowlistFilePath resolves MATRIX_ALLOWLIST_FILE, defaulting to
// $XDG_CONFIG_HOME/matrix-whatsapp-mcp/allow.txt with ~/.config as the XDG
// fallback. An empty result means no home directory could be determined, in
// which case only ALLOWED_SEND_ROOMS applies.
func allowlistFilePath(getenv func(string) string) string {
	if p := strings.TrimSpace(getenv("MATRIX_ALLOWLIST_FILE")); p != "" {
		return p
	}
	base := strings.TrimSpace(getenv("XDG_CONFIG_HOME"))
	if base == "" {
		home := strings.TrimSpace(getenv("HOME"))
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "matrix-whatsapp-mcp", "allow.txt")
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
