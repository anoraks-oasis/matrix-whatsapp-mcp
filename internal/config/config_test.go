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
