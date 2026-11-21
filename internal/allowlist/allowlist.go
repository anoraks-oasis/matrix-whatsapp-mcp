// Package allowlist owns the send allowlist: the ALLOWED_SEND_ROOMS baseline
// from the environment, unioned with the chat IDs in a plain-text file that the
// running server re-reads when the file changes. That file is what
// `matrix-whatsapp-mcp chats allow` writes, so enrolling a chat needs no
// restart and no .env edit.
//
// The file is read at most once per second, and only when it is consulted, so
// there is no watcher goroutine to leak. A re-read compares the content it gets
// with the content it already has, so a rewrite is noticed regardless of the
// filesystem's timestamp granularity; an unchanged file costs one small read
// and nothing else. Writes go through a temporary file and a rename, so a
// reader never observes a half-written allowlist.
package allowlist

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

// EnvSource is the origin label of the ALLOWED_SEND_ROOMS baseline.
const EnvSource = "ALLOWED_SEND_ROOMS"

const (
	// defaultMinInterval bounds how often the file on disk is stat'ed. A
	// second is fast enough that a `chats allow` takes effect before the
	// person can send anything, and slow enough that a busy send loop never
	// turns into a stat storm.
	defaultMinInterval = time.Second

	fileMode = 0o600
	dirMode  = 0o700

	fileHeader = "# matrix-whatsapp-mcp send allowlist: one chat ID per line.\n" +
		"# Lines starting with '#' are comments.\n" +
		"# Managed by `matrix-whatsapp-mcp chats allow` and `chats deny`.\n"
)

// ErrInvalidRoomID is returned when a caller asks to write a string that is not
// a well-formed Matrix room ID.
var ErrInvalidRoomID = errors.New("allowlist: invalid room ID")

// List is the read-only view of the effective allowlist. Both the Matrix client
// and the MCP send gate hold one, so they can never disagree about which rooms
// are sendable.
type List interface {
	// Rooms returns the effective room IDs, sorted and deduplicated.
	Rooms() []string
	// Allowed reports whether roomID is in the effective set.
	Allowed(roomID string) bool
}

// Entry is one effectively allow-listed room and where it came from: the
// environment variable name, the allowlist file path, or both.
type Entry struct {
	RoomID  string
	Sources []string
}

// Store is a List backed by the environment baseline and the allowlist file.
// It is safe for concurrent use.
type Store struct {
	baseline    []string
	path        string
	log         *log.Logger
	minInterval time.Duration
	now         func() time.Time

	mu          sync.Mutex
	entries     map[string]*Entry
	lastCheck   time.Time
	lastContent []byte
	lastExists  bool
	loaded      bool
	lastWarn    string
}

// Option configures a Store.
type Option func(*Store)

// WithMinInterval overrides how often the file is re-read. Tests use it to
// disable the throttle; production keeps the one-second default.
func WithMinInterval(d time.Duration) Option {
	return func(s *Store) { s.minInterval = d }
}

// New builds a Store over the given baseline (ALLOWED_SEND_ROOMS) and file. An
// empty path disables the file, leaving the baseline alone; a logger of nil
// only silences the diagnostics, so a nil-logger Store still reads the file,
// still warns nobody, and never panics on a malformed line. The file is not
// read until the first query, so construction cannot fail.
func New(baseline []string, path string, logger *log.Logger, opts ...Option) *Store {
	s := &Store{
		baseline:    append([]string(nil), baseline...),
		path:        path,
		log:         logger,
		minInterval: defaultMinInterval,
		now:         time.Now,
		entries:     make(map[string]*Entry, len(baseline)),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.rebuildLocked(nil, "")
	return s
}

// Path returns the allowlist file the Store watches, or "" when it watches
// none.
func (s *Store) Path() string { return s.path }

// Rooms returns the effective allow-list, sorted and deduplicated.
func (s *Store) Rooms() []string {
	s.refresh(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	rooms := make([]string, 0, len(s.entries))
	for roomID := range s.entries {
		rooms = append(rooms, roomID)
	}
	sort.Strings(rooms)
	return rooms
}

// Allowed reports whether roomID is in the effective allow-list.
func (s *Store) Allowed(roomID string) bool {
	s.refresh(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[roomID]
	return ok
}

// Entries returns every effective room with its origins, sorted by room ID.
func (s *Store) Entries() []Entry {
	s.refresh(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, Entry{RoomID: e.RoomID, Sources: append([]string(nil), e.Sources...)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RoomID < entries[j].RoomID })
	return entries
}

// Reload re-reads the file unconditionally, ignoring the throttle. Nothing in
// cmd/ or on the server path calls it: every query refreshes itself, and the
// running server sees a rewrite within the throttle anyway. It stays for tests
// that have to force a re-read without moving the clock.
func (s *Store) Reload() { s.refresh(true) }

func (s *Store) refresh(force bool) {
	if s.path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if !force && !s.lastCheck.IsZero() && now.Sub(s.lastCheck) < s.minInterval {
		return
	}
	s.lastCheck = now

	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if s.loaded && !s.lastExists {
			return
		}
		s.lastExists = false
		s.lastContent = nil
		s.rebuildLocked(nil, "")
		s.lastWarn = ""
		return
	case err != nil:
		s.warnLocked(fmt.Sprintf("allowlist file %s is unreadable, keeping the previous list: %v", s.path, err))
		return
	}
	if s.loaded && s.lastExists && bytes.Equal(data, s.lastContent) {
		return
	}
	rooms, warnings := parse(data)
	if s.log != nil {
		// A nil logger means no diagnostics at all, not a disabled file: the
		// parse warnings are the only thing it silences.
		for _, warning := range warnings {
			s.log.Printf("allowlist file %s: %s", s.path, warning)
		}
	}
	s.lastExists = true
	s.lastContent = data
	s.rebuildLocked(rooms, s.path)
	s.lastWarn = ""
}

// warnLocked logs a file problem once per distinct message, so a permission
// problem does not fill stderr with one line per send.
func (s *Store) warnLocked(msg string) {
	if s.log == nil || msg == s.lastWarn {
		return
	}
	s.lastWarn = msg
	s.log.Print(msg)
}

// rebuildLocked replaces the effective set. The caller holds s.mu.
func (s *Store) rebuildLocked(fileRooms []string, source string) {
	entries := make(map[string]*Entry, len(s.baseline)+len(fileRooms))
	add := func(roomID, from string) {
		if from == "" {
			return
		}
		if e, ok := entries[roomID]; ok {
			for _, have := range e.Sources {
				if have == from {
					return
				}
			}
			e.Sources = append(e.Sources, from)
			return
		}
		entries[roomID] = &Entry{RoomID: roomID, Sources: []string{from}}
	}
	for _, roomID := range s.baseline {
		add(roomID, EnvSource)
	}
	for _, roomID := range fileRooms {
		add(roomID, source)
	}
	s.entries = entries
	s.loaded = true
}

// parse extracts room IDs from allowlist file content, skipping blank lines and
// '#' comments. A malformed line becomes a warning naming its line number; the
// line's content is never echoed.
func parse(data []byte) (rooms, warnings []string) {
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !config.ValidRoomID(line) {
			warnings = append(warnings, fmt.Sprintf("line %d is not a room ID and was ignored", i+1))
			continue
		}
		rooms = append(rooms, line)
	}
	return rooms, warnings
}

// Add appends roomID to the allowlist file at path, creating the file and its
// directory when they do not exist. Existing comments and ordering are
// preserved, and a room that is already present is left alone. It reports
// whether the file changed.
func Add(path, roomID string) (bool, error) {
	return edit(path, roomID, true)
}

// Remove drops every line equal to roomID from the allowlist file, preserving
// comments. It reports whether the file changed.
func Remove(path, roomID string) (bool, error) {
	return edit(path, roomID, false)
}

func edit(path, roomID string, add bool) (bool, error) {
	if !config.ValidRoomID(roomID) {
		return false, fmt.Errorf("%w: %q", ErrInvalidRoomID, roomID)
	}
	lines, exists, err := readLines(path)
	if err != nil {
		return false, err
	}

	present := false
	kept := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if strings.TrimSpace(line) == roomID {
			present = true
			if !add {
				continue
			}
		}
		kept = append(kept, line)
	}
	if add && present {
		return false, nil
	}
	if !add && !present {
		return false, nil
	}
	if add {
		kept = append(kept, roomID)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	var b strings.Builder
	if !exists {
		b.WriteString(fileHeader)
	}
	for _, line := range kept {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := writeAtomic(path, b.String()); err != nil {
		return false, err
	}
	return true, nil
}

// readLines returns the file's lines without their trailing newline, and
// whether the file exists. A missing file is not an error: the first `rooms
// allow` creates it.
func readLines(path string) (lines []string, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, true, nil
	}
	return strings.Split(text, "\n"), true, nil
}

// writeAtomic writes content to a temporary file in the destination directory
// and renames it over path, so a concurrent reader sees either the old file or
// the new one and never a partial write.
func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("allowlist: create directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".allow-*.tmp")
	if err != nil {
		return fmt.Errorf("allowlist: create temporary file: %w", err)
	}
	tmp := f.Name()
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("allowlist: write temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("allowlist: sync temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("allowlist: close temporary file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("allowlist: replace: %w", err)
	}
	return nil
}

// FromSlice adapts a fixed list to List.
func FromSlice(rooms []string) List { return sliceList(append([]string(nil), rooms...)) }

type sliceList []string

func (l sliceList) Rooms() []string { return append([]string(nil), l...) }

func (l sliceList) Allowed(roomID string) bool {
	for _, r := range l {
		if r == roomID {
			return true
		}
	}
	return false
}
