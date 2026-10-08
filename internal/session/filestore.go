package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStore persists contextId -> cliSessionID bindings to a JSON file, so CLI
// session continuity (and resuming a paused task's session) survives a restart.
type FileStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

// NewFileStore opens (or creates) a file-backed store at path.
func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, m: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.m); err != nil {
			return nil, fmt.Errorf("parse session store %s: %w", path, err)
		}
	}
	return s, nil
}

// Get returns the CLI session id bound to a context.
func (s *FileStore) Get(contextID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[contextID]
	return v, ok
}

// Bind records the CLI session id for a context and persists the store.
func (s *FileStore) Bind(contextID, sessionID string) {
	if contextID == "" || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[contextID] = sessionID
	_ = s.flushLocked()
}

// flushLocked writes the store atomically; it must be called with the lock held.
func (s *FileStore) flushLocked() error {
	b, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
