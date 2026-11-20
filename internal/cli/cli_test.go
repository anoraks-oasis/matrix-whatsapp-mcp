package cli

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
)

// fakeRooms is a RoomService with no homeserver behind it.
type fakeRooms struct {
	mu       sync.Mutex
	rooms    []matrix.Room
	roomsErr error
	bridged  map[string]bool
	userID   string
	deviceID string
	whoErr   error
	closed   bool
	// probeRemaining records, per bridged probe, how long its context had left;
	// a probe without a deadline records 0.
	probeRemaining []time.Duration
}

func (f *fakeRooms) JoinedRooms(context.Context) ([]matrix.Room, error) { return f.rooms, f.roomsErr }

func (f *fakeRooms) IsBridged(ctx context.Context, roomID string) bool {
	var remaining time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	f.mu.Lock()
	f.probeRemaining = append(f.probeRemaining, remaining)
	f.mu.Unlock()
	return f.bridged[roomID]
}

// probes returns how long each bridged probe's context had left.
func (f *fakeRooms) probes() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.probeRemaining...)
}

func (f *fakeRooms) WhoAmI(context.Context) (string, string, error) {
	return f.userID, f.deviceID, f.whoErr
}

func (f *fakeRooms) Close() error { f.closed = true; return nil }

// fakeBus is a process edge set: buffers for both streams, a map for the
// environment, and a fake client.
type fakeBus struct {
	deps        Deps
	out, errOut bytes.Buffer
	svc         *fakeRooms
	env         map[string]string
	dir         string
}

func newBus(t *testing.T) *fakeBus {
	t.Helper()
	dir := t.TempDir()
	b := &fakeBus{
		svc: &fakeRooms{bridged: map[string]bool{}},
		dir: dir,
		env: map[string]string{
			"MATRIX_HOMESERVER":     "https://matrix.example.com",
			"MATRIX_USER_ID":        "@bot:example.com",
			"MATRIX_ACCESS_TOKEN":   "test-token",
			"MATRIX_ALLOWLIST_FILE": filepath.Join(dir, "allow.txt"),
			"HOME":                  dir,
			"XDG_CONFIG_HOME":       "",
		},
	}
	b.deps = Deps{
		Stdout:     &b.out,
		Stderr:     &b.errOut,
		Getenv:     func(key string) string { return b.env[key] },
		NewClient:  func(*config.Config, *log.Logger) (RoomService, error) { return b.svc, nil },
		HTTPClient: &http.Client{Timeout: time.Second},
	}
	return b
}

func (b *fakeBus) run(t *testing.T, args ...string) int {
	t.Helper()
	return Run(context.Background(), args, b.deps)
}

func (b *fakeBus) allowlistPath() string { return b.env["MATRIX_ALLOWLIST_FILE"] }

func twoRooms() []matrix.Room {
	return []matrix.Room{
		{ID: "!family:example.com", Name: "Family WhatsApp"},
		{ID: "!cafe:example.com", Name: "Cafe"},
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	b := newBus(t)
	if code := b.run(t, "nonsense"); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(b.errOut.String(), `unknown command "nonsense"`) {
		t.Fatalf("stderr = %q, want it to name the unknown command", b.errOut.String())
	}
	if b.out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing on a usage error", b.out.String())
	}
}

func TestRunWithoutCommandIsUsage(t *testing.T) {
	b := newBus(t)
	if code := b.run(t); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(b.errOut.String(), "Usage:") {
		t.Fatalf("stderr = %q, want usage text", b.errOut.String())
	}
}

func TestRunHelp(t *testing.T) {
	b := newBus(t)
	if code := b.run(t, "help"); code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	for _, want := range []string{"rooms allow", "doctor", "Enrolling a room"} {
		if !strings.Contains(b.out.String(), want) {
			t.Fatalf("help output is missing %q:\n%s", want, b.out.String())
		}
	}
}

func TestParseSelector(t *testing.T) {
	ok := []struct {
		args  []string
		query string
		index int
	}{
		{[]string{"Family WhatsApp"}, "Family WhatsApp", 0},
		{[]string{"Family", "WhatsApp"}, "Family WhatsApp", 0},
		{[]string{"--index", "2"}, "", 2},
		{[]string{"--index=3"}, "", 3},
		{[]string{"!room:example.com"}, "!room:example.com", 0},
	}
	for _, tc := range ok {
		sel, err := parseSelector(tc.args)
		if err != nil {
			t.Fatalf("parseSelector(%v) = %v, want no error", tc.args, err)
		}
		if sel.query != tc.query || sel.index != tc.index {
			t.Fatalf("parseSelector(%v) = {query:%q index:%d}, want {query:%q index:%d}",
				tc.args, sel.query, sel.index, tc.query, tc.index)
		}
	}

	bad := [][]string{
		nil,
		{},
		{"--index"},
		{"--index", "0"},
		{"--index", "-1"},
		{"--index", "two"},
		{"--index=", ""},
		{"Family", "--index", "1"},
		{"--query", "Family"},
		{"-x"},
	}
	for _, args := range bad {
		if _, err := parseSelector(args); err == nil {
			t.Fatalf("parseSelector(%v) = nil error, want a usage error", args)
		}
	}
}

func TestResolveRooms(t *testing.T) {
	rooms := []matrix.Room{
		{ID: "!family:example.com", Name: "Family WhatsApp"},
		{ID: "!family2:example.com", Name: "Family Photos"},
		{ID: "!cafe:example.com", Name: "Cafe"},
		{ID: "!opaque:example.com", Name: "!opaque:example.com"},
	}

	cases := []struct {
		name  string
		sel   selector
		want  string
		isErr bool
	}{
		{"exact room ID", selector{query: "!cafe:example.com"}, "!cafe:example.com", false},
		{"exact name", selector{query: "cafe"}, "!cafe:example.com", false},
		{"exact name is case-insensitive", selector{query: "CAFE"}, "!cafe:example.com", false},
		{"exact name beats a longer substring match", selector{query: "Family WhatsApp"}, "!family:example.com", false},
		{"substring", selector{query: "photos"}, "!family2:example.com", false},
		{"room ID substring", selector{query: "!opaque"}, "!opaque:example.com", false},
		{"index", selector{index: 3, hasIndex: true}, "!cafe:example.com", false},
		{"index out of range", selector{index: 9, hasIndex: true}, "", true},
		{"ambiguous", selector{query: "family"}, "", true},
		{"no match", selector{query: "nothing like it"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolve(rooms, tc.sel)
			if tc.isErr {
				if err == nil {
					t.Fatalf("resolve(%+v) = %q, want an error", tc.sel, got.ID)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%+v) = %v", tc.sel, err)
			}
			if got.ID != tc.want {
				t.Fatalf("resolve(%+v) = %q, want %q", tc.sel, got.ID, tc.want)
			}
		})
	}
}

func TestResolveAmbiguityListsCandidates(t *testing.T) {
	rooms := []matrix.Room{
		{ID: "!a:example.com", Name: "Family WhatsApp"},
		{ID: "!b:example.com", Name: "Family Photos"},
	}
	_, err := resolve(rooms, selector{query: "family"})
	if err == nil {
		t.Fatal("resolve of an ambiguous query returned no error")
	}
	for _, want := range []string{"!a:example.com", "!b:example.com", "Family WhatsApp", "Family Photos", "refusing to guess"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ambiguity error %q is missing %q", err, want)
		}
	}
}

func TestRoomsTable(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()
	b.svc.bridged["!family:example.com"] = true
	b.env["ALLOWED_SEND_ROOMS"] = "!cafe:example.com"

	if code := b.run(t, "rooms"); code != exitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, b.errOut.String())
	}
	out := b.out.String()
	for _, want := range []string{"#", "NAME", "ROOM ID", "BRIDGED", "SEND", "Family WhatsApp", "!family:example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table is missing %q:\n%s", want, out)
		}
	}
	// Row numbers start at 1 and match the listing order.
	familyLine := lineContaining(t, out, "Family WhatsApp")
	cafeLine := lineContaining(t, out, "Cafe")
	if !strings.HasPrefix(familyLine, "  1") {
		t.Fatalf("first row = %q, want it numbered 1", familyLine)
	}
	if !strings.HasPrefix(cafeLine, "  2") {
		t.Fatalf("second row = %q, want it numbered 2", cafeLine)
	}
	if !strings.HasSuffix(strings.TrimRight(familyLine, " "), "no") {
		t.Fatalf("family row = %q, want send no (it is not allow-listed)", familyLine)
	}
	if !strings.HasSuffix(strings.TrimRight(cafeLine, " "), "yes") {
		t.Fatalf("cafe row = %q, want send yes (ALLOWED_SEND_ROOMS)", cafeLine)
	}
	if !strings.Contains(out, "2 room(s), 1 send-allowed") {
		t.Fatalf("footer is missing the counts:\n%s", out)
	}
	if !strings.Contains(out, "1 from ALLOWED_SEND_ROOMS, 0 from "+b.allowlistPath()) {
		t.Fatalf("footer is missing the allowlist origins:\n%s", out)
	}
	if !strings.Contains(out, "not created yet") {
		t.Fatalf("footer should say the file does not exist yet:\n%s", out)
	}
}

func TestRoomsEmpty(t *testing.T) {
	b := newBus(t)
	if code := b.run(t, "rooms"); code != exitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(b.out.String(), "no joined rooms") {
		t.Fatalf("output = %q, want the empty-state message", b.out.String())
	}
}

func TestRoomsAllowByNameWritesFile(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()

	if code := b.run(t, "rooms", "allow", "cafe"); code != exitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, b.errOut.String())
	}
	data, err := os.ReadFile(b.allowlistPath())
	if err != nil {
		t.Fatalf("allowlist file was not written: %v", err)
	}
	if !strings.Contains(string(data), "!cafe:example.com") {
		t.Fatalf("allowlist file = %q, want it to contain the room", data)
	}
	if !strings.Contains(b.out.String(), "allowed") || !strings.Contains(b.out.String(), "no restart needed") {
		t.Fatalf("stdout = %q, want a confirmation that no restart is needed", b.out.String())
	}
}

func TestRoomsAllowByIndex(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()

	if code := b.run(t, "rooms", "allow", "--index", "1"); code != exitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, b.errOut.String())
	}
	data, err := os.ReadFile(b.allowlistPath())
	if err != nil {
		t.Fatalf("allowlist file was not written: %v", err)
	}
	if !strings.Contains(string(data), "!family:example.com") {
		t.Fatalf("allowlist file = %q, want the first listed room", data)
	}
}

func TestRoomsAllowTwiceIsIdempotent(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()

	for range 2 {
		if code := b.run(t, "rooms", "allow", "Cafe"); code != exitOK {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, b.errOut.String())
		}
	}
	if n := strings.Count(readFile(t, b.allowlistPath()), "!cafe:example.com"); n != 1 {
		t.Fatalf("allowlist file lists the room %d times, want 1", n)
	}
	if !strings.Contains(b.out.String(), "already in") {
		t.Fatalf("stdout = %q, want the second run to say it was already listed", b.out.String())
	}
}

func TestRoomsAllowAmbiguousRefusesAndWritesNothing(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = []matrix.Room{
		{ID: "!a:example.com", Name: "Family WhatsApp"},
		{ID: "!b:example.com", Name: "Family Photos"},
	}

	if code := b.run(t, "rooms", "allow", "family"); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	for _, want := range []string{"!a:example.com", "!b:example.com"} {
		if !strings.Contains(b.errOut.String(), want) {
			t.Fatalf("stderr = %q, want it to list candidate %s", b.errOut.String(), want)
		}
	}
	if _, err := os.Stat(b.allowlistPath()); !os.IsNotExist(err) {
		t.Fatalf("an ambiguous allow wrote %s", b.allowlistPath())
	}
}

func TestRoomsDenyRemovesFromFile(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()
	if code := b.run(t, "rooms", "allow", "!cafe:example.com"); code != exitOK {
		t.Fatalf("setup allow failed with %d", code)
	}

	if code := b.run(t, "rooms", "deny", "cafe"); code != exitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, b.errOut.String())
	}
	if data := readFile(t, b.allowlistPath()); strings.Contains(data, "!cafe:example.com") {
		t.Fatalf("allowlist file = %q, want the room removed", data)
	}
	if !strings.Contains(b.out.String(), "denied") {
		t.Fatalf("stdout = %q, want a denial confirmation", b.out.String())
	}
}

func TestRoomsDenyEnvOnlyRoomFails(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()
	b.env["ALLOWED_SEND_ROOMS"] = "!cafe:example.com"

	if code := b.run(t, "rooms", "deny", "cafe"); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(b.errOut.String(), "ALLOWED_SEND_ROOMS") {
		t.Fatalf("stderr = %q, want it to point at ALLOWED_SEND_ROOMS", b.errOut.String())
	}
}

func TestRoomsDenyUnlistedRoomIsNoOp(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()

	if code := b.run(t, "rooms", "deny", "cafe"); code != exitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(b.out.String(), "nothing to deny") {
		t.Fatalf("stdout = %q, want a no-op message", b.out.String())
	}
}

func TestRoomsAllowRoomAlreadyInEnv(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()
	b.env["ALLOWED_SEND_ROOMS"] = "!cafe:example.com"

	if code := b.run(t, "rooms", "allow", "cafe"); code != exitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(b.out.String(), "already sendable through ALLOWED_SEND_ROOMS") {
		t.Fatalf("stdout = %q, want it to mention ALLOWED_SEND_ROOMS", b.out.String())
	}
}

func TestRoomsSelectorAndIndexTogetherIsUsageError(t *testing.T) {
	b := newBus(t)
	b.svc.rooms = twoRooms()
	if code := b.run(t, "rooms", "allow", "cafe", "--index", "1"); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

func TestRoomsSurfacesClientError(t *testing.T) {
	b := newBus(t)
	b.svc.roomsErr = io.ErrUnexpectedEOF

	if code := b.run(t, "rooms"); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(b.errOut.String(), "error:") {
		t.Fatalf("stderr = %q, want an error line", b.errOut.String())
	}
}

func versionsServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/versions" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		io.WriteString(w, `{"versions":["v1.1","v1.11"]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoctorHealthy(t *testing.T) {
	b := newBus(t)
	srv := versionsServer(t, http.StatusOK)
	b.env["MATRIX_HOMESERVER"] = srv.URL
	b.env["ALLOWED_SEND_ROOMS"] = "!cafe:example.com"
	b.svc.rooms = twoRooms()
	b.svc.bridged["!family:example.com"] = true
	b.svc.userID = "@bot:example.com"
	b.svc.deviceID = "DEVICE"
	if err := os.WriteFile(b.allowlistPath(), []byte("!family:example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := b.run(t, "doctor"); code != exitOK {
		t.Fatalf("exit code = %d, want 0\n%s\n%s", code, b.out.String(), b.errOut.String())
	}
	out := b.out.String()
	for _, want := range []string{
		"config", "homeserver", srv.URL, "@bot:example.com", "stdio",
		"[ok  ]", "versions", "whoami", "joined rooms", "2 joined, 1 bridged",
		"send allowlist", "2 room(s)", "crypto db",
		"joined rooms", b.allowlistPath(),
		"all 5 checks passed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "test-token") {
		t.Fatal("doctor printed the access token")
	}
}

func TestDoctorFailsWhenVersionsUnreachable(t *testing.T) {
	b := newBus(t)
	srv := versionsServer(t, http.StatusInternalServerError)
	b.env["MATRIX_HOMESERVER"] = srv.URL
	b.svc.userID = "@bot:example.com"

	if code := b.run(t, "doctor"); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(b.out.String(), "[fail] versions") {
		t.Fatalf("doctor output = %q, want a failed versions check", b.out.String())
	}
	if !strings.Contains(b.out.String(), "check(s) failed") {
		t.Fatalf("doctor output = %q, want a failure summary", b.out.String())
	}
}

func TestDoctorFailsOnTokenUserMismatch(t *testing.T) {
	b := newBus(t)
	srv := versionsServer(t, http.StatusOK)
	b.env["MATRIX_HOMESERVER"] = srv.URL
	b.svc.userID = "@someone-else:example.com"

	if code := b.run(t, "doctor"); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(b.out.String(), "the token belongs to @someone-else:example.com") {
		t.Fatalf("doctor output = %q, want the mismatch spelled out", b.out.String())
	}
}

func TestDoctorWarnsWhenCryptoDBIsNotWritable(t *testing.T) {
	b := newBus(t)
	srv := versionsServer(t, http.StatusOK)
	b.env["MATRIX_HOMESERVER"] = srv.URL
	b.env["MATRIX_E2EE_ENABLED"] = "true"
	b.env["MATRIX_PICKLE_KEY"] = "pickle-key"
	// A regular file where a directory is required: the crypto store cannot be
	// created next to it.
	notADir := filepath.Join(b.dir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.env["MATRIX_CRYPTO_DB_PATH"] = filepath.Join(notADir, "crypto.db")
	b.svc.userID = "@bot:example.com"

	if code := b.run(t, "doctor"); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	out := b.out.String()
	if !strings.Contains(out, "[warn] crypto db") {
		t.Fatalf("doctor output = %q, want a crypto db warning", out)
	}
	if !strings.Contains(out, "the server cannot start") {
		t.Fatalf("doctor output = %q, want the consequence spelled out", out)
	}
}

func TestDoctorReportsReadOnlyAllowlist(t *testing.T) {
	b := newBus(t)
	srv := versionsServer(t, http.StatusOK)
	b.env["MATRIX_HOMESERVER"] = srv.URL
	b.svc.userID = "@bot:example.com"

	if code := b.run(t, "doctor"); code != exitOK {
		t.Fatalf("exit code = %d, want 0\n%s", code, b.out.String())
	}
	if !strings.Contains(b.out.String(), "read-only") {
		t.Fatalf("doctor output = %q, want it to report read-only mode for an empty allowlist", b.out.String())
	}
}

func TestDoctorRejectsArguments(t *testing.T) {
	b := newBus(t)
	if code := b.run(t, "doctor", "--fix"); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
}

func lineContaining(t *testing.T, text, needle string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no line contains %q in:\n%s", needle, text)
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestRoomsProbePhaseIsBounded is R8's CLI half: the bridged probes run under
// one overall deadline, so a large account with a slow homeserver cannot spend
// the whole 30s command budget probing and end with no table at all. Reaching
// IsBridged without a deadline would mean the phase is unbounded again.
func TestRoomsProbePhaseIsBounded(t *testing.T) {
	if probeTimeout <= 0 || probeTimeout >= commandTimeout {
		t.Fatalf("probeTimeout = %s, want it inside the %s command budget", probeTimeout, commandTimeout)
	}

	b := newBus(t)
	b.svc.rooms = twoRooms()
	if code := b.run(t, "rooms"); code != exitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, b.errOut.String())
	}

	probes := b.svc.probes()
	if len(probes) != len(twoRooms()) {
		t.Fatalf("recorded %d bridged probes for %d rooms, want one per room", len(probes), len(twoRooms()))
	}
	for i, remaining := range probes {
		if remaining <= 0 {
			t.Fatalf("probe %d ran without a deadline, want the probe phase bounded", i)
		}
		// The command's own 30s deadline reaches this context too, so the bound
		// under test is the shorter one: anything above it means the phase is
		// running on the command budget alone.
		if remaining > probeTimeout {
			t.Fatalf("probe %d had %s left, want at most the %s probe bound", i, remaining, probeTimeout)
		}
	}
	if strings.Contains(b.errOut.String(), "cut off") {
		t.Fatalf("stderr = %q, want no cut-off note when every probe finished", b.errOut.String())
	}
}
