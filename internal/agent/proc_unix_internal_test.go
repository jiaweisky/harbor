//go:build !windows

package agent

import (
	"os"
	"testing"
)

// TestProcessMatchesExactProgram verifies matching is by exact program name, so
// a reused PID running a different program whose name merely contains the
// recorded one is not killed.
func TestProcessMatchesExactProgram(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve executable: %v", err)
	}
	if !processMatches(os.Getpid(), exe) {
		t.Fatalf("current process should match its own executable %q", exe)
	}
	if processMatches(os.Getpid(), "/no/such/unrelated-binary-xyz") {
		t.Fatal("a different program must not match")
	}
}
