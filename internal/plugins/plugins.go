//go:build cgo && (linux || darwin || freebsd)

// Package plugins loads Go plugins that register additional CLI adapters at
// runtime (see integration.RegisterAdapter). It is an optional, experimental extension
// point: a plugin must export a function named "Register" with type func(),
// which registers its adapters.
package plugins

import (
	"fmt"
	"path/filepath"
	"plugin"
)

// Load opens each Go plugin (.so) and calls its exported "Register" function.
// It stops at the first error; plugins loaded before the failure stay registered.
func Load(paths []string) error {
	for _, p := range paths {
		pl, err := plugin.Open(p)
		if err != nil {
			return fmt.Errorf("open plugin %s: %w", filepath.Base(p), err)
		}
		sym, err := pl.Lookup("Register")
		if err != nil {
			return fmt.Errorf("plugin %s: missing Register symbol", filepath.Base(p))
		}
		register, ok := sym.(func())
		if !ok {
			return fmt.Errorf("plugin %s: Register has type %T, want func()", filepath.Base(p), sym)
		}
		register()
	}
	return nil
}
