package workspace_test

import (
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/workspace"
)

func TestLockSerializesAcrossResolvers(t *testing.T) {
	dir := t.TempDir()
	r1 := workspace.NewResolver(dir)
	r2 := workspace.NewResolver(dir)

	unlock := r1.Lock(dir)
	acquired := make(chan struct{})
	go func() {
		u := r2.Lock(dir)
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first is held")
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("lock was not released")
	}
}

func TestLockIsolationRoot(t *testing.T) {
	root := t.TempDir()
	first, err := workspace.LockIsolationRoot(workspace.IsolationCopy, root)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Skip("isolation locking is not available on this platform")
	}
	defer first.Close()
	if second, err := workspace.LockIsolationRoot(workspace.IsolationCopy, root); err == nil {
		_ = second.Close()
		t.Fatal("second isolation_root lock was acquired")
	}
}
