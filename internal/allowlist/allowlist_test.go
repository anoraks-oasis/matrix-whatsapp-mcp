package allowlist

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	roomA = "!aaa:example.com"
	roomB = "!bbb:example.com"
	roomC = "!ccc:example.com"
)

// writeFile writes content to dir/allow.txt and returns the path.
func writeFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "allow.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write allowlist file: %v", err)
	}
	return path
}

func newTestStore(t *testing.T, baseline []string, path string, opts ...Option) (*Store, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	store := New(baseline, path, log.New(&buf, "", 0), opts...)
	return store, &buf
}

func TestStoreUnionDedupesAndRecordsSources(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, strings.Join([]string{
		"# comment",
		"",
		roomA,               // also in the baseline: both sources must show up
		roomB,               // file only
		roomB,               // duplicate line
		"  " + roomC + "  ", // surrounding whitespace is tolerated
		"# trailing comment",
	}, "\n"))

	store, logs := newTestStore(t, []string{roomA, roomC}, path)

	want := []string{roomA, roomB, roomC}
	if got := store.Rooms(); !equalStrings(got, want) {
		t.Fatalf("Rooms() = %v, want %v", got, want)
	}
	if !store.Allowed(roomB) {
		t.Fatalf("Allowed(%q) = false, want true", roomB)
	}
	if store.Allowed("!nope:example.com") {
		t.Fatal("Allowed accepted a room that is in neither source")
	}

	entries := store.Entries()
	if len(entries) != 3 {
		t.Fatalf("Entries() = %v, want 3 entries", entries)
	}
	sources := map[string][]string{}
	for _, e := range entries {
		sources[e.RoomID] = e.Sources
	}
	if !equalStrings(sources[roomA], []string{EnvSource, path}) {
		t.Fatalf("sources for %q = %v, want both the env var and the file", roomA, sources[roomA])
	}
	if !equalStrings(sources[roomB], []string{path}) {
		t.Fatalf("sources for %q = %v, want only the file", roomB, sources[roomB])
	}
	if logs.Len() != 0 {
		t.Fatalf("well-formed file logged warnings: %s", logs.String())
	}
}

func TestStoreWarnsOnMalformedLinesWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "not-a-room\n"+roomA+"\n!\n")

	store, logs := newTestStore(t, nil, path)
	if got := store.Rooms(); !equalStrings(got, []string{roomA}) {
		t.Fatalf("Rooms() = %v, want just the valid room", got)
	}
	out := logs.String()
	if !strings.Contains(out, "line 1") || !strings.Contains(out, "line 3") {
		t.Fatalf("warnings = %q, want one per malformed line naming its number", out)
	}
	if strings.Contains(out, "not-a-room") {
		t.Fatalf("warning echoes the malformed line: %q", out)
	}
}

func TestStoreMissingFileIsReadOnly(t *testing.T) {
	store, _ := newTestStore(t, nil, filepath.Join(t.TempDir(), "absent", "allow.txt"))
	if rooms := store.Rooms(); len(rooms) != 0 {
		t.Fatalf("Rooms() = %v, want empty for a missing file", rooms)
	}
	if store.Allowed(roomA) {
		t.Fatal("Allowed accepted a room with a missing file and an empty baseline")
	}
}

func TestStorePicksUpRewrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, roomA+"\n")

	store, _ := newTestStore(t, nil, path, WithMinInterval(0))
	if store.Allowed(roomB) {
		t.Fatal("Allowed saw a room that is not in the file yet")
	}

	if err := os.WriteFile(path, []byte(roomA+"\n"+roomB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !store.Allowed(roomB) {
		t.Fatal("Allowed did not pick up a rewritten file")
	}

	// Dropping a room from the file must revoke it again, and a same-size
	// rewrite must still be noticed.
	if err := os.WriteFile(path, []byte(roomC+"\n"+roomB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.Allowed(roomA) {
		t.Fatal("Allowed kept a room that was removed from the file")
	}
	if !store.Allowed(roomC) {
		t.Fatal("Allowed missed a same-size rewrite of the file")
	}
}

func TestStoreThrottlesFileChecks(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, roomA+"\n")

	store, _ := newTestStore(t, nil, path)
	now := time.Now()
	store.now = func() time.Time { return now }

	if !store.Allowed(roomA) {
		t.Fatal("initial load did not see the room")
	}

	if err := os.WriteFile(path, []byte(roomA+"\n"+roomB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.Allowed(roomB) {
		t.Fatal("the file was re-read inside the minimum interval")
	}

	now = now.Add(defaultMinInterval)
	if !store.Allowed(roomB) {
		t.Fatal("the file was not re-read after the minimum interval elapsed")
	}

	// Reload bypasses the throttle.
	if err := os.WriteFile(path, []byte(roomB+"\n"+roomC+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Reload()
	if !store.Allowed(roomC) {
		t.Fatal("Reload did not bypass the throttle")
	}
}

func TestStoreKeepsLastListWhenFileBecomesUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	dir := t.TempDir()
	path := writeFile(t, dir, roomA+"\n")
	store, logs := newTestStore(t, nil, path, WithMinInterval(0))
	if !store.Allowed(roomA) {
		t.Fatal("initial load did not see the room")
	}

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })

	// Two queries: the problem is logged once, and the previous list survives.
	for range 2 {
		if !store.Allowed(roomA) {
			t.Fatal("an unreadable file dropped the previously loaded room")
		}
	}
	if got := strings.Count(logs.String(), "unreadable"); got != 1 {
		t.Fatalf("logged %d unreadable-file warnings, want exactly 1: %s", got, logs.String())
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, roomA+"\n")
	store, _ := newTestStore(t, []string{roomB}, path, WithMinInterval(0))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = store.Allowed(roomA)
					_ = store.Rooms()
					_ = store.Entries()
				}
			}
		}()
	}
	for i := range 5 {
		room := roomC
		if i%2 == 0 {
			room = roomA
		}
		if _, err := Add(path, room); err != nil {
			t.Errorf("Add: %v", err)
		}
		store.Reload()
	}
	close(stop)
	wg.Wait()
}

func TestAddCreatesDirectoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "allow.txt")

	changed, err := Add(path, roomA)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !changed {
		t.Fatal("Add reported no change for a new file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, roomA) {
		t.Fatalf("file content = %q, want it to contain %q", text, roomA)
	}
	if !strings.HasPrefix(text, "#") {
		t.Fatalf("file content = %q, want a header comment", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != fileMode {
		t.Fatalf("file mode = %o, want %o", perm, fileMode)
	}

	changed, err = Add(path, roomA)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if changed {
		t.Fatal("Add reported a change for a room that was already listed")
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "allow.txt" {
		t.Fatalf("directory holds %v, want only allow.txt (no leftover temp files)", entries)
	}
}

func TestAddAndRemovePreserveComments(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "# keep me\n"+roomA+"\n# and me\n")

	if _, err := Add(path, roomB); err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# keep me", roomA, "# and me", roomB} {
		if !strings.Contains(text, want) {
			t.Fatalf("file content = %q, want it to contain %q", text, want)
		}
	}
	if !strings.HasSuffix(text, roomB+"\n") {
		t.Fatalf("file content = %q, want the new room appended last", text)
	}

	changed, err := Remove(path, roomA)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !changed {
		t.Fatal("Remove reported no change for a listed room")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text = string(data)
	if strings.Contains(text, roomA) {
		t.Fatalf("file content = %q, want %q removed", text, roomA)
	}
	if !strings.Contains(text, "# keep me") || !strings.Contains(text, "# and me") {
		t.Fatalf("file content = %q, want the comments preserved", text)
	}

	changed, err = Remove(path, roomC)
	if err != nil {
		t.Fatalf("Remove of an absent room: %v", err)
	}
	if changed {
		t.Fatal("Remove reported a change for a room that was not listed")
	}
}

func TestRemoveDropsDuplicateLines(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, roomA+"\n"+roomB+"\n"+roomA+"\n")

	if _, err := Remove(path, roomA); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); strings.Contains(text, roomA) || !strings.Contains(text, roomB) {
		t.Fatalf("file content = %q, want every %q line gone and %q kept", text, roomA, roomB)
	}
}

func TestEditRejectsInvalidRoomID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.txt")
	for _, bad := range []string{"", "not-a-room", "!a/b", "!a b"} {
		if _, err := Add(path, bad); err == nil {
			t.Fatalf("Add(%q) = nil error, want an invalid room ID error", bad)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a rejected Add created %s", path)
	}
}

func TestStoreSeesCLIWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.txt")
	store, _ := newTestStore(t, nil, path, WithMinInterval(0))
	if len(store.Rooms()) != 0 {
		t.Fatal("store started with rooms it should not have")
	}

	if _, err := Add(path, roomA); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !store.Allowed(roomA) {
		t.Fatal("store did not see a room written by Add")
	}

	if _, err := Remove(path, roomA); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if store.Allowed(roomA) {
		t.Fatal("store kept a room removed by Remove")
	}
}

func TestEmptyPathIgnoresFile(t *testing.T) {
	store, _ := newTestStore(t, []string{roomA}, "", WithMinInterval(0))
	if got := store.Rooms(); !equalStrings(got, []string{roomA}) {
		t.Fatalf("Rooms() = %v, want the baseline alone", got)
	}
	if store.Path() != "" {
		t.Fatalf("Path() = %q, want empty", store.Path())
	}
}

func TestFromSlice(t *testing.T) {
	list := FromSlice([]string{roomA, roomA})
	if !list.Allowed(roomA) || list.Allowed(roomB) {
		t.Fatal("FromSlice does not report membership correctly")
	}
	if got := list.Rooms(); len(got) != 2 {
		t.Fatalf("Rooms() = %v, want the slice as given", got)
	}

	rooms := list.Rooms()
	rooms[0] = "mutated"
	if got := list.Rooms(); got[0] == "mutated" {
		t.Fatal("Rooms() leaks the internal slice")
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestStoreNilLoggerStillReadsTheFile pins the constructor's contract: a nil
// logger silences the diagnostics and disables nothing else. The file is still
// read and unioned with the baseline.
func TestStoreNilLoggerStillReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, roomA+"\n")

	store := New([]string{roomB}, path, nil, WithMinInterval(0))
	if got := store.Rooms(); !equalStrings(got, []string{roomA, roomB}) {
		t.Fatalf("Rooms() with a nil logger = %v, want the baseline and the file", got)
	}
	if !store.Allowed(roomA) || !store.Allowed(roomB) {
		t.Fatal("a nil logger disabled the file")
	}
	if store.Path() != path {
		t.Fatalf("Path() = %q, want %q", store.Path(), path)
	}
}

// TestStoreNilLoggerDoesNotPanicOnMalformedLine is R3's regression: the parse
// warning loop dereferenced a nil logger, so one hand-edited bad line in the
// file crashed a nil-logger Store instead of being silently dropped.
func TestStoreNilLoggerDoesNotPanicOnMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "not-a-room\n"+roomA+"\n!\n")

	store := New(nil, path, nil, WithMinInterval(0))
	if got := store.Rooms(); !equalStrings(got, []string{roomA}) {
		t.Fatalf("Rooms() = %v, want the one valid room with the malformed lines dropped", got)
	}

	// A rewritten file warns again, still without a logger.
	if err := os.WriteFile(path, []byte(roomB+"\n"+"also-not-a-room\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !store.Allowed(roomB) {
		t.Fatal("a nil-logger Store did not pick up a rewritten file")
	}
}
