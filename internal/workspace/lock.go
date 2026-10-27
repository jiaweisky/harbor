package workspace

import "sync"

// lockSet provides reference-counted per-key mutexes.
type lockSet struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// directoryLocks serializes tasks across resolvers that share a directory.
var directoryLocks = newLockSet()

func newLockSet() *lockSet { return &lockSet{entries: map[string]*lockEntry{}} }

func (l *lockSet) lock(key string) func() {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		e = &lockEntry{}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}
