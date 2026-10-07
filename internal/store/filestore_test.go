package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/store"
)

func TestFileStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	ctx := context.Background()

	s, err := store.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	reopened, err := store.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Task.ID != "t1" || got.Task.ContextID != "c1" {
		t.Fatalf("got %+v", got.Task)
	}
}
