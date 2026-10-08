//go:build !windows

package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// killProcGroup sends SIGKILL to a process group.
func killProcGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// processAlive reports whether a process with pid exists (including a zombie).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// processMatches reports whether pid's command line references binary. It is
// conservative: when the command line cannot be read it returns false, so a
// reused PID is never killed by mistake.
func processMatches(pid int, binary string) bool {
	name := filepath.Base(binary)
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return len(data) > 0 && bytes.Contains(data, []byte(name))
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), name)
}
