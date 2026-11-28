// Package pastebin holds the single shared scratch pad behind the panel's
// Pastebin page: one text, persisted atomically under <DataDir>/pastebin/paste.txt,
// with an in-process publish/subscribe hub so every open panel sees edits live.
//
// It is deliberately self-contained and independent of the fleet event bus.
package pastebin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

// FileName is the persisted pad inside <DataDir>/pastebin.
const FileName = "paste.txt"

// MaxBytes is the largest pad the store accepts (5 MB, as the original).
const MaxBytes = 5 << 20

// ErrTooLarge is returned by Set when the text exceeds MaxBytes.
var ErrTooLarge = errors.New("pastebin: text is larger than the 5 MB limit")

// subscriberBuffer is how many undelivered frames a subscriber may lag by
// before it is dropped (it reconnects and reloads the current pad).
const subscriberBuffer = 16

// Frame is one pad state pushed to subscribers. Client is the id the editing
// tab sent with its write, so that tab can ignore its own echo; it is empty
// for the initial snapshot.
type Frame struct {
	Text    string `json:"text"`
	Version uint64 `json:"version"`
	Client  string `json:"client"`
}

// Store is the shared pad. All methods are safe for concurrent use.
type Store struct {
	dir    string
	rename func(oldpath, newpath string) error

	mu      sync.Mutex
	text    string
	version uint64
	subs    map[chan Frame]struct{}
}

// Open opens the pad under dataDir, loading <dataDir>/pastebin/paste.txt when
// it exists. A missing file starts an empty pad.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("pastebin: data directory is required")
	}
	s := &Store{
		dir:    filepath.Join(dataDir, config.PastebinDirName),
		rename: os.Rename,
		subs:   map[chan Frame]struct{}{},
	}
	data, err := os.ReadFile(s.Path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("pastebin: read %s: %w", s.Path(), err)
	default:
		s.text = string(data)
	}
	return s, nil
}

// Dir returns the storage directory (<dataDir>/pastebin).
func (s *Store) Dir() string { return s.dir }

// Path returns the persisted pad file.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

// Get returns the current text and its version. The version starts at 0 on
// every process start and increases with each Set.
func (s *Store) Get() (string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text, s.version
}

// Set replaces the pad, persists it, bumps the version and broadcasts the new
// state (tagged with client) to every subscriber. When persisting fails the
// pad is left unchanged and nothing is broadcast.
func (s *Store) Set(text, client string) (uint64, error) {
	if len(text) > MaxBytes {
		return 0, ErrTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveLocked(text); err != nil {
		return 0, err
	}
	s.text = text
	s.version++
	s.broadcastLocked(Frame{Text: text, Version: s.version, Client: client})
	return s.version, nil
}

// Subscribe registers a subscriber. The returned channel first delivers the
// current state (Client empty), then every later Set. The channel is closed
// when the subscriber falls too far behind, or after cancel is called; the
// holder should then reconnect and reload. cancel is idempotent.
func (s *Store) Subscribe() (<-chan Frame, func()) {
	ch := make(chan Frame, subscriberBuffer)
	s.mu.Lock()
	ch <- Frame{Text: s.text, Version: s.version}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() { s.unsubscribe(ch) }
}

func (s *Store) unsubscribe(ch chan Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
}

func (s *Store) broadcastLocked(f Frame) {
	for ch := range s.subs {
		select {
		case ch <- f:
		default:
			// Too slow: drop it so it reconnects and reloads the pad.
			delete(s.subs, ch)
			close(ch)
		}
	}
}

func (s *Store) saveLocked(text string) error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("pastebin: create %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("pastebin: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pastebin: chmod temp file: %w", err)
	}
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pastebin: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pastebin: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("pastebin: close temp file: %w", err)
	}
	if err := s.rename(tmpName, s.Path()); err != nil {
		cleanup()
		return fmt.Errorf("pastebin: replace %s: %w", s.Path(), err)
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
