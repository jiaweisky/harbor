package session

import (
	"database/sql"

	"github.com/jiaweisky/harbor/internal/sqlstore"
)

// SQLiteStore persists contextId -> cliSessionID bindings in a SQLite database,
// so CLI session continuity survives a restart. Binding writes a single row
// instead of rewriting the whole store.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite-backed store at path.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sqlstore.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cli_sessions (
		context_id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

// Get returns the CLI session id bound to a context.
func (s *SQLiteStore) Get(contextID string) (string, bool) {
	var sessionID string
	err := s.db.QueryRow(
		`SELECT session_id FROM cli_sessions WHERE context_id = ?`, contextID,
	).Scan(&sessionID)
	if err != nil {
		return "", false
	}
	return sessionID, true
}

// Bind records the CLI session id for a context.
func (s *SQLiteStore) Bind(contextID, sessionID string) {
	if contextID == "" || sessionID == "" {
		return
	}
	_, _ = s.db.Exec(`INSERT INTO cli_sessions(context_id, session_id) VALUES(?, ?)
		ON CONFLICT(context_id) DO UPDATE SET session_id = excluded.session_id`,
		contextID, sessionID)
}

// Close closes the database.
func (s *SQLiteStore) Close() error { return s.db.Close() }
