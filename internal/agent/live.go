package agent

import "sync"

// LiveSessions maps taskID -> Session so resume and cancel can find a live process.
type LiveSessions struct {
	m sync.Map
}

// NewLiveSessions creates an empty live-session table.
func NewLiveSessions() *LiveSessions { return &LiveSessions{} }

func (l *LiveSessions) Get(taskID string) (Session, bool) {
	v, ok := l.m.Load(taskID)
	if !ok {
		return nil, false
	}

	return v.(Session), true
}

func (l *LiveSessions) Put(taskID string, s Session) { l.m.Store(taskID, s) }

func (l *LiveSessions) Del(taskID string) { l.m.Delete(taskID) }
