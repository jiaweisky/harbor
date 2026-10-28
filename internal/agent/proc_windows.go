//go:build windows

package agent

import (
	"os/exec"
	"strconv"
)

func setProcessGroup(cmd *exec.Cmd) {}

// terminateGroup kills the CLI and its descendants. CLI launchers on Windows are
// often .cmd shims that run the real program as a child; killing only the shim
// leaves the child holding the inherited stdout pipe, so the read loop never
// sees EOF. taskkill /T walks and terminates the whole tree.
func terminateGroup(p *Process) {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	pid := strconv.Itoa(p.cmd.Process.Pid)
	if exec.Command("taskkill", "/T", "/F", "/PID", pid).Run() != nil {
		_ = p.cmd.Process.Kill()
	}
}

func killGroup(p *Process) {
	terminateGroup(p)
}

// killProcGroup is a no-op on Windows: a stale process group cannot be verified
// and killing an unverified PID risks a reused handle.
func killProcGroup(int) {}

// processMatches always reports false on Windows, so orphan sweep never kills
// an unverified process.
func processMatches(int, string) bool { return false }

// processAlive always reports false on Windows, so a record is always swept
// (but processMatches prevents any kill).
func processAlive(int) bool { return false }
