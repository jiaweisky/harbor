package adapters

import (
	"os"
	"path/filepath"
)

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// anySet reports whether any of the keys has a non-empty value.
func anySet(env map[string]string, keys ...string) bool {
	for _, k := range keys {
		if env[k] != "" {
			return true
		}
	}
	return false
}

// configDir resolves a CLI config directory: an explicit override from the
// effective environment wins, otherwise the conventional subdirectory under the
// home is used.
func configDir(home, override, sub string) string {
	if override != "" {
		return override
	}
	return filepath.Join(home, sub)
}
