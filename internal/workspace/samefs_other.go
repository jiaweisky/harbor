//go:build !linux && !darwin

package workspace

// sameFilesystem accepts the pair on platforms where copy-on-write is not
// implemented; CheckIsolation rejects cow before this check runs.
func sameFilesystem(left, right string) (bool, error) { return true, nil }
