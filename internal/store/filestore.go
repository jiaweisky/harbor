// Package store provides a durable task store.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// FileStore is a taskstore.Store that persists tasks to a JSON file. It wraps
// the SDK in-memory store for task semantics and keeps its own task copy so it
// can snapshot without relying on List (which requires an authenticator).
type FileStore struct {
	mem   *taskstore.InMemory
	path  string
	mu    sync.Mutex
	tasks map[a2a.TaskID]*a2a.Task
}

// NewFileStore opens (or creates) a file-backed store at path, loading any
// previously persisted tasks.
func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{
		mem:   taskstore.NewInMemory(nil),
		path:  path,
		tasks: map[a2a.TaskID]*a2a.Task{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Create implements taskstore.Store.
func (s *FileStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	v, err := s.mem.Create(ctx, task)
	if err == nil {
		s.store(task)
	}
	return v, err
}

// Update implements taskstore.Store.
func (s *FileStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	v, err := s.mem.Update(ctx, req)
	if err == nil {
		s.store(req.Task)
	}
	return v, err
}

// Get implements taskstore.Store.
func (s *FileStore) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	return s.mem.Get(ctx, id)
}

// List implements taskstore.Store.
func (s *FileStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return s.mem.List(ctx, req)
}

// store records the latest task state and persists the whole set. Persistence
// failures are ignored: a snapshot error must not fail the in-flight request.
func (s *FileStore) store(task *a2a.Task) {
	if task == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task

	data, err := json.Marshal(s.taskList())
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

func (s *FileStore) taskList() []*a2a.Task {
	out := make([]*a2a.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out
}

func (s *FileStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var tasks []*a2a.Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return fmt.Errorf("task store %s: %w", s.path, err)
	}
	for _, t := range tasks {
		if _, err := s.mem.Create(context.Background(), t); err != nil {
			return fmt.Errorf("task store %s: %w", s.path, err)
		}
		s.tasks[t.ID] = t
	}
	return nil
}
