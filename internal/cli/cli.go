// Package cli holds Harbor's knowledge about well-known coding-agent CLIs:
// their capabilities, defaults, and minimum supported versions. It keeps
// CLI-specific data outside the YAML schema.
package cli

import (
	"path/filepath"
	"strings"

	"github.com/jiaweisky/harbor/internal/core"
)

// Profile describes a known coding-agent CLI: which integrations it supports,
// the default integration, the minimum supported version, and how to read the
// version. The CLI's authentication surface lives with the CLI's adapter.
type Profile struct {
	Binary      string             // matched against the binary's base name
	Integration core.Integration   // default integration
	Parser      core.ParserKind    // default exec parser
	Supported   []core.Integration // accepted integrations
	MinVersion  string             // minimum supported version; below it Harbor refuses to start
	VersionArgs []string           // version flag(s); nil disables probing
	ACPArgs     []string           // args selecting ACP mode, when unset in the agent config
	ForkFlag    string             // default session-fork flag (e.g. --fork-session)
	// OutputSchemaArgs is the default structured-output args for this CLI,
	// applied when the agent config leaves output_schema_args unset.
	OutputSchemaArgs []string
	// FileArgs is the default file-attachment args for this CLI, applied when
	// the agent config leaves file_args unset.
	FileArgs []string
	// ImageArgs is the default image-attachment args for this CLI, applied when
	// the agent config leaves image_args unset.
	ImageArgs []string
	// InputImage lists image MIME types the CLI accepts; nil means images are
	// not accepted. They are sent as native content blocks when the CLI's
	// integration supports structured input, otherwise attached via ImageArgs.
	InputImage []string
	// OutputImage lists image MIME types the CLI can produce (for example a
	// text-to-image model writing files into the workdir); nil means Harbor does
	// not advertise image output for this CLI.
	OutputImage []string
}

// Supports reports whether the profile accepts the integration.
func (p Profile) Supports(i core.Integration) bool {
	for _, s := range p.Supported {
		if s == i {
			return true
		}
	}

	return false
}

// knownProfiles maps well-known CLIs to their capabilities. MinVersion is the
// version Harbor has been validated against; older versions are rejected so the
// user upgrades rather than hitting undefined behavior. Unknown binaries fall
// through to explicit configuration.
var knownProfiles = []Profile{
	{Binary: "codebuddy", Integration: core.IntegrationStreamJSON, MinVersion: "2.161.4",
		VersionArgs: []string{"--version"},
		Supported: []core.Integration{
			core.IntegrationStreamJSON, core.IntegrationExec,
			core.IntegrationACP, core.IntegrationServer,
		},
		InputImage:  []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		OutputImage: []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		ForkFlag:    "--fork-session"},
	{Binary: "claude", Integration: core.IntegrationStreamJSON, MinVersion: "2.1.292",
		VersionArgs: []string{"--version"},
		Supported:   []core.Integration{core.IntegrationStreamJSON}, ForkFlag: "--fork-session"},
	{Binary: "opencode", Integration: core.IntegrationExec, MinVersion: "1.18.35",
		VersionArgs: []string{"--version"},
		Supported:   []core.Integration{core.IntegrationExec, core.IntegrationACP, core.IntegrationServer},
		ACPArgs:     []string{"acp"},
		FileArgs:    []string{"-f", "{{.File}}"}, ForkFlag: "--fork"},
	{Binary: "codex", Integration: core.IntegrationExec, Parser: core.ParserCodex,
		MinVersion:       "0.160.1",
		VersionArgs:      []string{"--version"},
		Supported:        []core.Integration{core.IntegrationExec, core.IntegrationServer},
		OutputSchemaArgs: []string{"--output-schema", "{{.SchemaFile}}"},
		InputImage:       []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		ImageArgs:        []string{"-i", "{{.Image}}"}},
}

// Lookup returns the profile for a binary path or name.
// baseName normalizes a binary path or name for registry lookups: the base
// name without the executable extension Windows appends (.exe, .cmd, .bat,
// .ps1). Without this, a registry keyed by "codebuddy" never matches the
// codebuddy.cmd that Windows PATH resolution hands to Harbor, silently
// disabling profiles, auth, and dedicated adapters on that platform.
func baseName(binary string) string {
	name := filepath.Base(binary)
	if ext := filepath.Ext(name); ext != "" {
		switch strings.ToLower(ext) {
		case ".exe", ".cmd", ".bat", ".ps1":
			return strings.TrimSuffix(name, ext)
		}
	}

	return name
}

func Lookup(binary string) (Profile, bool) {
	name := baseName(binary)

	for _, p := range knownProfiles {
		if p.Binary == name {
			return p, true
		}
	}

	return Profile{}, false
}
