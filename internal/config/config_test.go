package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// domainlessRoomID has the shape the live WhatsApp bridge hands out: no
// ":server" part. It must survive both Load and ValidRoomID.
const domainlessRoomID = "!UBRW6WycBKpUJu3m4HP0RCMsGnUp_EwSZa2RLXHe34Y"

func envFrom(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func minimalEnv(overrides map[string]string) func(string) string {
	pairs := map[string]string{
		"MATRIX_HOMESERVER":   "https://matrix.example.com",
		"MATRIX_USER_ID":      "@bot:example.com",
		"MATRIX_ACCESS_TOKEN": "test-token",
	}
	for k, v := range overrides {
		pairs[k] = v
	}
	return envFrom(pairs)
}

func TestLoadAcceptsDomainlessAllowedSendRooms(t *testing.T) {
	cfg, err := LoadFrom(minimalEnv(map[string]string{
		"ALLOWED_SEND_ROOMS": domainlessRoomID + ", !other:example.com",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.AllowedSendRooms) != 2 {
		t.Fatalf("AllowedSendRooms = %v, want 2 entries", cfg.AllowedSendRooms)
	}
	if !cfg.SendAllowed(domainlessRoomID) {
		t.Fatalf("SendAllowed(%q) = false, want true", domainlessRoomID)
	}
	if cfg.ReadOnly() {
		t.Fatal("ReadOnly() = true with allow-listed rooms")
	}
}

func TestLoadRejectsMalformedAllowedSendRooms(t *testing.T) {
	for _, entry := range []string{"not-a-room", "!a/b", "!a b", "!a?b", "!a#b", "!" + strings.Repeat("x", 255)} {
		_, err := LoadFrom(minimalEnv(map[string]string{"ALLOWED_SEND_ROOMS": entry}))
		if err == nil {
			t.Fatalf("LoadFrom(ALLOWED_SEND_ROOMS=%q) = nil error, want a validation error", entry)
		}
		var verr *VarError
		if !errors.As(err, &verr) || verr.Var != "ALLOWED_SEND_ROOMS" {
			t.Fatalf("LoadFrom(ALLOWED_SEND_ROOMS=%q) = %v, want a ALLOWED_SEND_ROOMS VarError", entry, err)
		}
	}
}

func TestValidRoomID(t *testing.T) {
	valid := []string{
		domainlessRoomID,
		"!abc:example.com",
		"!" + strings.Repeat("x", 254),
	}
	for _, roomID := range valid {
		if !ValidRoomID(roomID) {
			t.Errorf("ValidRoomID(%q) = false, want true", roomID)
		}
	}
	invalid := []string{
		"",
		"not-a-room",
		"!",
		"!a/b",
		"!a b",
		"!a\tb",
		"!a?b",
		"!a#b",
		"!" + strings.Repeat("x", 255),
	}
	for _, roomID := range invalid {
		if ValidRoomID(roomID) {
			t.Errorf("ValidRoomID(%q) = true, want false", roomID)
		}
	}
}

func TestLoadBridgeAckTimeout(t *testing.T) {
	cfg, err := LoadFrom(minimalEnv(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.BridgeAckTimeout != time.Second {
		t.Fatalf("default BridgeAckTimeout = %v, want 1s", cfg.BridgeAckTimeout)
	}

	for raw, want := range map[string]time.Duration{
		"0":     0,
		"250ms": 250 * time.Millisecond,
		"2s":    2 * time.Second,
	} {
		cfg, err := LoadFrom(minimalEnv(map[string]string{"MATRIX_BRIDGE_ACK_TIMEOUT": raw}))
		if err != nil {
			t.Fatalf("LoadFrom(MATRIX_BRIDGE_ACK_TIMEOUT=%q): %v", raw, err)
		}
		if cfg.BridgeAckTimeout != want {
			t.Fatalf("BridgeAckTimeout for %q = %v, want %v", raw, cfg.BridgeAckTimeout, want)
		}
	}

	for _, raw := range []string{"abc", "-1s", "5", "1s500", "two seconds"} {
		_, err := LoadFrom(minimalEnv(map[string]string{"MATRIX_BRIDGE_ACK_TIMEOUT": raw}))
		if err == nil {
			t.Fatalf("LoadFrom(MATRIX_BRIDGE_ACK_TIMEOUT=%q) = nil error, want a typed config error", raw)
		}
		var verr *VarError
		if !errors.As(err, &verr) || verr.Var != "MATRIX_BRIDGE_ACK_TIMEOUT" {
			t.Fatalf("LoadFrom(MATRIX_BRIDGE_ACK_TIMEOUT=%q) = %v, want a MATRIX_BRIDGE_ACK_TIMEOUT VarError", raw, err)
		}
	}

	// The message names the variable but never echoes the rejected value.
	for _, raw := range []string{"abc", "-1s", "1s500", "two seconds"} {
		_, err := LoadFrom(minimalEnv(map[string]string{"MATRIX_BRIDGE_ACK_TIMEOUT": raw}))
		if strings.Contains(err.Error(), raw) {
			t.Fatalf("config error for %q echoes the value: %v", raw, err)
		}
	}
}

func TestLoadEnrollmentDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/tester")

	cfg, err := LoadFrom(minimalEnv(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.AutoJoinInvites {
		t.Fatal("AutoJoinInvites default = true, want false")
	}
	if len(cfg.InviteAllowlist) != 0 {
		t.Fatalf("InviteAllowlist default = %v, want empty", cfg.InviteAllowlist)
	}
	want := "/home/tester/.config/matrix-whatsapp-mcp/allow.txt"
	if cfg.AllowlistFile != want {
		t.Fatalf("AllowlistFile = %q, want %q", cfg.AllowlistFile, want)
	}
}

func TestAllowlistFilePath(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "explicit path wins",
			env:  map[string]string{"MATRIX_ALLOWLIST_FILE": "/etc/mw/rooms.txt", "XDG_CONFIG_HOME": "/xdg"},
			want: "/etc/mw/rooms.txt",
		},
		{
			name: "xdg config home",
			env:  map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			want: "/xdg/matrix-whatsapp-mcp/allow.txt",
		},
		{
			name: "home fallback",
			env:  map[string]string{"HOME": "/home/tester", "XDG_CONFIG_HOME": ""},
			want: "/home/tester/.config/matrix-whatsapp-mcp/allow.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", "")
			if got := allowlistFilePath(envFrom(tc.env)); got != tc.want {
				t.Fatalf("allowlistFilePath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadAutoJoinInvites(t *testing.T) {
	for raw, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "false": false, "0": false} {
		cfg, err := LoadFrom(minimalEnv(map[string]string{"MATRIX_AUTO_JOIN_INVITES": raw}))
		if err != nil {
			t.Fatalf("LoadFrom(MATRIX_AUTO_JOIN_INVITES=%q): %v", raw, err)
		}
		if cfg.AutoJoinInvites != want {
			t.Fatalf("AutoJoinInvites for %q = %t, want %t", raw, cfg.AutoJoinInvites, want)
		}
	}

	for _, raw := range []string{"yes-please", "on?", "2"} {
		_, err := LoadFrom(minimalEnv(map[string]string{"MATRIX_AUTO_JOIN_INVITES": raw}))
		if err == nil {
			t.Fatalf("LoadFrom(MATRIX_AUTO_JOIN_INVITES=%q) = nil error, want a validation error", raw)
		}
		var verr *VarError
		if !errors.As(err, &verr) || verr.Var != "MATRIX_AUTO_JOIN_INVITES" {
			t.Fatalf("LoadFrom(MATRIX_AUTO_JOIN_INVITES=%q) = %v, want a MATRIX_AUTO_JOIN_INVITES VarError", raw, err)
		}
	}
}

func TestLoadInviteAllowlist(t *testing.T) {
	cfg, err := LoadFrom(minimalEnv(map[string]string{
		"MATRIX_INVITE_ALLOWLIST": " @alice:example.com , @bob:example.com ,, ",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.InviteAllowlist) != 2 {
		t.Fatalf("InviteAllowlist = %v, want 2 entries", cfg.InviteAllowlist)
	}
	if !cfg.InviteAllowed("@alice:example.com") || !cfg.InviteAllowed("@BOB:example.com") {
		t.Fatalf("InviteAllowed rejected a listed inviter: %v", cfg.InviteAllowlist)
	}
	if cfg.InviteAllowed("@mallory:example.com") {
		t.Fatal("InviteAllowed accepted an unlisted inviter with a non-empty allow-list")
	}

	for _, entry := range []string{"alice", "@alice", "alice@example.com"} {
		_, err := LoadFrom(minimalEnv(map[string]string{"MATRIX_INVITE_ALLOWLIST": entry}))
		if err == nil {
			t.Fatalf("LoadFrom(MATRIX_INVITE_ALLOWLIST=%q) = nil error, want a validation error", entry)
		}
		var verr *VarError
		if !errors.As(err, &verr) || verr.Var != "MATRIX_INVITE_ALLOWLIST" {
			t.Fatalf("LoadFrom(MATRIX_INVITE_ALLOWLIST=%q) = %v, want a MATRIX_INVITE_ALLOWLIST VarError", entry, err)
		}
	}
}

func TestInviteAllowedWithEmptyAllowlist(t *testing.T) {
	cfg, err := LoadFrom(minimalEnv(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !cfg.InviteAllowed("@anyone:anywhere.example") {
		t.Fatal("InviteAllowed with an empty allow-list = false, want true (accept from anyone)")
	}
}
