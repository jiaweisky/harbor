//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGroup(p *Process) {
	if p.pgid > 0 {
		_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	}
}

func killGroup(p *Process) {
	if p.pgid > 0 {
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	}
}
