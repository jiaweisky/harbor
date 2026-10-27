package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func ensureWorktree(repo, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // already provisioned for this context
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := gitWorktree(repo, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return nil
}

func gitWorktree(repo, dst string) error {
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "--detach", dst, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
