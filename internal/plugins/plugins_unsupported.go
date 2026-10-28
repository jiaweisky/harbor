//go:build !cgo || (!linux && !darwin && !freebsd)

// Package plugins provides a no-op loader on platforms/builds where Go plugins
// (the standard "plugin" package) are unavailable.
package plugins

import "errors"

// Load reports an error when plugins are configured on an unsupported platform.
func Load(paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	return errors.New("Go plugins require cgo on linux, darwin, or freebsd")
}
