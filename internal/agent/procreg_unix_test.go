//go:build !windows

package agent_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
)

// startGrouped starts a long-running process in its own group and records it as
// Harbor would, returning the command and its group id.
func startGrouped(t *testing.T, dir, binary string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	pgid := cmd.Process.Pid
	rec, err := json.Marshal(map[int]string{pgid: binary})
	if err != nil {
		t.Fatal(err)
	}
	// A record left by a crashed instance: its owning PID is no longer alive.
	if err := os.WriteFile(filepath.Join(dir, "procs.4000000"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	return cmd, pgid
}

func TestSweepOrphansKillsMatchingGroup(t *testing.T) {
	dir := t.TempDir()
	cmd, _ := startGrouped(t, dir, "sleep") // binary base matches the process

	agent.SweepOrphans(dir)

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("orphaned process group was not killed")
	}
}

func TestSweepOrphansIgnoresUnknownBinary(t *testing.T) {
	dir := t.TempDir()
	cmd, pgid := startGrouped(t, dir, "/usr/bin/some-other-tool")

	agent.SweepOrphans(dir)

	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatal("a process whose command does not match the record must not be killed")
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Wait()
}

func TestSweepOrphansSkipsLiveOwner(t *testing.T) {
	dir := t.TempDir()
	cmd, pgid := startGrouped(t, dir, "sleep")
	// Rewrite the record under the still-running test process's PID.
	rec, _ := json.Marshal(map[int]string{pgid: "sleep"})
	name := "procs." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, name), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "procs.4000000"))

	agent.SweepOrphans(dir)

	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatal("must not kill a child of a still-running instance")
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Wait()
}
