// Package version parses and compares CLI version strings.
package version

import (
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

var semverPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// Parse extracts the first semantic version from arbitrary text and returns it
// in canonical form (e.g. "v2.1.292"). ok is false when none is found.
func Parse(text string) (string, bool) { return canonical(text) }

// Less reports whether a is older than b. ok is false when either string does
// not contain a recognizable version.
func Less(a, b string) (bool, bool) {
	ca, oka := canonical(a)
	cb, okb := canonical(b)
	if !oka || !okb {
		return false, false
	}

	return semver.Compare(ca, cb) < 0, true
}

// canonical normalizes a version string to "vX.Y.Z", accepting bare versions,
// already-canonical versions, and text that embeds a version.
func canonical(s string) (string, bool) {
	if m := semverPattern.FindString(s); m != "" {
		v := semver.Canonical("v" + m)
		return v, v != ""
	}

	v := semver.Canonical(strings.TrimSpace(s))

	return v, v != ""
}
