//go:build windows

package agent

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func terminateGroup(p *Process) {
	if p.cmd != nil && p.cmd.Process != nil {
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
