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
