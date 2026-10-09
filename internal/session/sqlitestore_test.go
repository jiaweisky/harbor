package session_test

import (
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/session"
)

// TestSQLiteStorePersists verifies bindings survive reopening the store.
func TestSQLiteStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")

	s, err := session.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	s.Bind("ctx-1", "ses-1")

	reopened, err := session.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, ok := reopened.Get("ctx-1"); !ok || got != "ses-1" {
		t.Fatalf("Get = %q, %v; want ses-1, true", got, ok)
	}
}

// TestSQLiteStoreRebind verifies a later binding for a context wins.
func TestSQLiteStoreRebind(t *testing.T) {
	s, err := session.NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Bind("ctx", "ses-1")
	s.Bind("ctx", "ses-2")
	if got, _ := s.Get("ctx"); got != "ses-2" {
		t.Fatalf("Get = %q, want ses-2", got)
	}
}
