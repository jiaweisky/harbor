// Package session stores A2A contextId -> CLI native sessionId bindings.
package session

import "sync"

// Store is an in-memory contextId -> cliSessionID map.
type Store struct {
	mu sync.RWMutex
	m  map[string]string
}

// New creates an empty Store.
func New() *Store { return &Store{m: map[string]string{}} }

// Get returns the CLI session id bound to a context.
func (s *Store) Get(contextID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[contextID]
	return v, ok
}

// Bind records the CLI session id for a context.
func (s *Store) Bind(contextID, sessionID string) {
	if contextID == "" || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[contextID] = sessionID
}
