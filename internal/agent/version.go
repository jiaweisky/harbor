package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// DetectVersion runs the binary with the given version arguments and returns
// the first output line. It is best-effort: any error yields an empty string.
func DetectVersion(ctx context.Context, path string, args []string) string {
	if len(args) == 0 {
		return ""
	}
	vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, path, args...).Output()
	if err != nil {
		return ""
	}
	return firstLine(string(out))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
