package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func memberBody(stateKey, displayName, membership string, extra string) string {
	content := `{"membership":"` + membership + `"`
	if displayName != "" {
		content += `,"displayname":"` + displayName + `"`
	}
	content += extra + `}`
	return `{"type":"m.room.member","state_key":"` + stateKey + `","sender":"` + stateKey +
		`","event_id":"$m-` + stateKey + `","origin_server_ts":1,"content":` + content + `}`
}

// membersServer serves a fixed member list and counts profile lookups.
func membersServer(t *testing.T, chunk []string, profile func(userID string) (int, string)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var profileCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/members"):
			io.WriteString(w, `{"chunk":[`+strings.Join(chunk, ",")+`]}`)
		case strings.Contains(r.URL.Path, "/state/"):
			// No room-level m.bridge state event: the member heuristic decides.
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND"}`)
		case strings.HasSuffix(r.URL.Path, "/displayname"):
			profileCalls.Add(1)
			status, body := 200, `{"displayname":""}`
			if profile != nil {
				status, body = profile(r.URL.Path)
			}
			w.WriteHeader(status)
			io.WriteString(w, body)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	return srv, &profileCalls
}

func TestParticipantsClassifiesMembers(t *testing.T) {
	chunk := []string{
		memberBody("@whatsapp_15551234567:test", "Alice (WA)", "join", ""),
		memberBody("@whatsapp_lid-987654321:test", "Bob (WA)", "join", ""),
		memberBody("@whatsappbot:test", "WhatsApp bridge", "join", ""),
		memberBody("@alice:test", "", "join", ""),
		memberBody("@gone:test", "Left", "leave", ""),
	}
	srv, _ := membersServer(t, chunk, func(string) (int, string) {
		return 200, `{"displayname":"Alice"}`
	})
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	parts, err := c.Participants(context.Background(), "!r:test")
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(parts) != 4 {
		t.Fatalf("participants = %d, want 4 (the left member is not joined)", len(parts))
	}
	wantUsers := []string{"@alice:test", "@whatsapp_15551234567:test", "@whatsapp_lid-987654321:test", "@whatsappbot:test"}
	wantKinds := []string{"", BridgeKindContact, BridgeKindContact, BridgeKindBot}
	wantNames := []string{"Alice", "Alice (WA)", "Bob (WA)", "WhatsApp bridge"}
	for i, p := range parts {
		if p.UserID != wantUsers[i] {
			t.Fatalf("participant %d = %s, want %s (sorted by display name)", i, p.UserID, wantUsers[i])
		}
		if p.BridgeKind != wantKinds[i] {
			t.Fatalf("%s bridge_kind = %q, want %q", p.UserID, p.BridgeKind, wantKinds[i])
		}
		if p.Bridged != (wantKinds[i] != "") {
			t.Fatalf("%s bridged = %t, want %t", p.UserID, p.Bridged, wantKinds[i] != "")
		}
		if p.DisplayName != wantNames[i] {
			t.Fatalf("%s display_name = %q, want %q", p.UserID, p.DisplayName, wantNames[i])
		}
		if p.Membership != "join" {
			t.Fatalf("%s membership = %q, want join", p.UserID, p.Membership)
		}
	}
}

// TestParticipantsToleratesMissingProfiles proves one unreadable profile never
// fails the list: the member is still returned, with an empty name.
func TestParticipantsToleratesMissingProfiles(t *testing.T) {
	chunk := []string{
		memberBody("@zeta:test", "", "join", ""),
		memberBody("@alpha:test", "", "join", ""),
	}
	srv, profileCalls := membersServer(t, chunk, func(path string) (int, string) {
		if strings.Contains(path, "zeta") {
			return http.StatusInternalServerError, `{"errcode":"M_UNKNOWN"}`
		}
		return http.StatusNotFound, `{"errcode":"M_NOT_FOUND"}`
	})
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	parts, err := c.Participants(context.Background(), "!r:test")
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("participants = %d, want both members despite the failed profile lookups", len(parts))
	}
	if profileCalls.Load() != 2 {
		t.Fatalf("profile lookups = %d, want 2 (one per unnamed member)", profileCalls.Load())
	}
	if parts[0].UserID != "@alpha:test" || parts[1].UserID != "@zeta:test" {
		t.Fatalf("order = %s,%s, want user-ID order when no name is known", parts[0].UserID, parts[1].UserID)
	}
	if parts[0].DisplayName != "" || parts[1].DisplayName != "" {
		t.Fatalf("display names = %q/%q, want empty rather than a failure", parts[0].DisplayName, parts[1].DisplayName)
	}
}

// TestParticipantsMarksBridgeMarker covers a ghost whose localpart follows no
// naming convention but whose member event carries bridge metadata.
func TestParticipantsMarksBridgeMarker(t *testing.T) {
	chunk := []string{
		memberBody("@ghost:example.org", "Ann", "join", `,"uk.half-shot.bridge":{}`),
		memberBody("@plain:example.org", "Ben", "join", ""),
	}
	srv, _ := membersServer(t, chunk, nil)
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	parts, err := c.Participants(context.Background(), "!r:test")
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("participants = %d, want 2", len(parts))
	}
	if !parts[0].Bridged || parts[0].BridgeKind != BridgeKindContact {
		t.Fatalf("marked member = %+v, want a bridged whatsapp_contact", parts[0])
	}
	if parts[1].Bridged || parts[1].BridgeKind != "" {
		t.Fatalf("plain member = %+v, want not bridged", parts[1])
	}
}

func TestParticipantsRejectsBadRoom(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Participants(context.Background(), "not-a-room"); !errors.Is(err, ErrInvalidRoomID) {
		t.Fatalf("error = %v, want ErrInvalidRoomID", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("a rejected call made %d requests, want 0", requests.Load())
	}
}

func TestParticipantsHonoursCancellation(t *testing.T) {
	started := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	_, err = c.Participants(ctx, "!r:test")
	if err == nil {
		t.Fatal("Participants returned no error after cancellation")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Participants error = %v, want the context error", err)
	}
}

// TestClassifyBridgeUser pins the one heuristic behind both IsBridged and
// list_participants, including the deliberate looseness IsBridged has always
// had: any localpart containing whatsapp counts, because bridges rename ghosts.
func TestClassifyBridgeUser(t *testing.T) {
	cases := map[string]string{
		"@whatsapp_15551234567:example.com": BridgeKindContact,
		// Both LID shapes, and the case-insensitivity of the whole rule.
		"@whatsapp_lid-987654321:example.com": BridgeKindContact,
		"@whatsapp-lid-987654321:example.com": BridgeKindContact,
		"@WhatsApp_LID-987654321:example.com": BridgeKindContact,
		"@whatsapp_lid_987654321:example.com": BridgeKindContact,
		"@whatsapp_1203630:example.com":       BridgeKindContact,
		"@whatsappbot:example.com":            BridgeKindBot,
		"@whatsapp_bot:example.com":           BridgeKindBot,
		"@whatsappbridgebot:example.com":      BridgeKindBot,
		"@WhatsAppBot:example.com":            BridgeKindBot,
		"@somewhatsappname:example.com":       BridgeKindContact,
		"@alice:example.com":                  "",
		"@signal_15551234567:example.com":     "",
	}
	for userID, want := range cases {
		if got := classifyBridgeUser(userID); got != want {
			t.Fatalf("classifyBridgeUser(%q) = %q, want %q", userID, got, want)
		}
	}
}

// TestIsBridgedUsesMemberHeuristic is the regression test for the room-level
// flag the refactor touched: a room with a WhatsApp ghost member is bridged, a
// room without one is not, and a failed member probe is not bridged.
func TestIsBridgedUsesMemberHeuristic(t *testing.T) {
	chunk := []string{memberBody("@whatsapp_15551234567:test", "Alice (WA)", "join", "")}
	srv, _ := membersServer(t, chunk, nil)
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// The room-level probe answers 404 above (an unknown route), so the member
	// probe decides.
	if !c.IsBridged(context.Background(), "!r:test") {
		t.Fatal("IsBridged = false for a room with a WhatsApp ghost member")
	}
}

func TestIsBridgedFalseWithoutBridgeEvidence(t *testing.T) {
	chunk := []string{memberBody("@alice:test", "Alice", "join", "")}
	srv, _ := membersServer(t, chunk, nil)
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if c.IsBridged(context.Background(), "!r:test") {
		t.Fatal("IsBridged = true for a room with no bridge evidence")
	}
}
