package session_test

import (
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/session"
)

// TestFileStorePersists verifies bindings survive reopening the store.
func TestFileStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	s, err := session.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	s.Bind("ctx-1", "ses-1")

	reopened, err := session.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, ok := reopened.Get("ctx-1"); !ok || got != "ses-1" {
		t.Fatalf("Get = %q, %v; want ses-1, true", got, ok)
	}
}

// TestFileStoreIgnoresEmpty verifies empty ids are not bound.
func TestFileStoreIgnoresEmpty(t *testing.T) {
	s, err := session.NewFileStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Bind("", "ses")
	s.Bind("ctx", "")
	if _, ok := s.Get(""); ok {
		t.Fatal("empty context should not be bound")
	}
}
