// Package sqlstore opens SQLite databases with Harbor's standard settings.
package sqlstore

import (
	"database/sql"
	"os"
	"path/filepath"

	// Register the pure-Go SQLite driver under the name "sqlite".
	_ "modernc.org/sqlite"
)

// Open opens (or creates) a SQLite database at path. It creates the parent
// directory, enables WAL journaling, and uses one writer connection (SQLite
// allows a single writer), which keeps writes incremental and avoids
// "database is locked" under concurrent updates.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	// Pre-create the database owner-only; SQLite creates the -wal/-shm side
	// files with the database file's mode, and task rows/session ids may be
	// sensitive.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := f.Close(); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Cap the pool at one connection: SQLite has a single writer, and the
	// PRAGMAs below bind to that connection (a larger pool could leave other
	// connections without them).
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	return db, nil
}
