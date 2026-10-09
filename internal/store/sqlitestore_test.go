package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/jiaweisky/harbor/internal/store"
)

// TestSQLiteStoreSurvivesReopen verifies a persisted task is reloaded when the
// store is reopened.
func TestSQLiteStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	ctx := context.Background()

	s, err := store.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	reopened, err := store.NewSQLiteStore(path)
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

// TestSQLiteStoreVersions verifies create/update versioning and the rejected
// upgrade when the previous version does not match.
func TestSQLiteStoreVersions(t *testing.T) {
	ctx := context.Background()
	s, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	v, err := s.Create(ctx, task)
	if err != nil || v != taskstore.TaskVersion(1) {
		t.Fatalf("Create = %v, %v; want version 1", v, err)
	}
	if _, err := s.Create(ctx, task); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("duplicate Create err = %v, want ErrTaskAlreadyExists", err)
	}

	task.Status.State = a2a.TaskStateWorking
	if v2, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: v}); err != nil || v2 != 2 {
		t.Fatalf("Update = %v, %v; want version 2", v2, err)
	}

	// A stale expected version must be rejected.
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale Update err = %v, want ErrConcurrentModification", err)
	}

	got, err := s.Get(ctx, "t1")
	if err != nil || got.Version != 2 || got.Task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

// TestSQLiteStoreListRequiresUser verifies List rejects an unauthenticated
// caller (the default authenticator has no identity).
func TestSQLiteStoreListRequiresUser(t *testing.T) {
	s, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(context.Background(), &a2a.ListTasksRequest{}); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Fatalf("List err = %v, want ErrUnauthenticated", err)
	}
}
