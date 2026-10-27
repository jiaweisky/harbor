//go:build !linux && !darwin

package workspace

import (
	"fmt"
	"runtime"
)

func cloneFile(src, dst string) error {
	return fmt.Errorf("copy-on-write clone is not supported on %s", runtime.GOOS)
}
