package adapters

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// claude authenticates via an API key or OAuth token in the environment, or a
// credentials file under CLAUDE_CONFIG_DIR. On macOS the login state otherwise
// lives in the Keychain, which cannot be redirected by environment variables,
// so env credentials are the reliable isolated path.
type claude struct{}

func (claude) LoggedIn(home string, env map[string]string) bool {
	if anySet(env, "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN") {
		return true
	}
	dir := configDir(home, env["CLAUDE_CONFIG_DIR"], ".claude")
	return fileExists(filepath.Join(dir, ".credentials.json"))
}

// `claude auth status` prints a JSON object with a loggedIn field.
func (claude) StatusArgs() []string { return []string{"auth", "status"} }

func (claude) LoggedInStatus(output string, runErr error) bool {
	// Parse the JSON object even if the CLI surrounds it with other text.
	if i, j := strings.IndexByte(output, '{'), strings.LastIndexByte(output, '}'); i >= 0 && j > i {
		var s struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if json.Unmarshal([]byte(output[i:j+1]), &s) == nil {
			return s.LoggedIn
		}
	}
	return runErr == nil
}
