package cli

import (
	"path/filepath"
	"strings"
)

// codex stores its login state in CODEX_HOME/auth.json and also accepts API key
// and access token environment variables. CODEX_HOME redirects the login state
// cleanly, so tenant isolation is straightforward.
type codex struct{}

func (codex) LoggedIn(home string, env map[string]string) bool {
	if anySet(env, "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN") {
		return true
	}

	dir := configDir(home, env["CODEX_HOME"], ".codex")

	return fileExists(filepath.Join(dir, "auth.json"))
}

// `codex login status` exits non-zero (and prints "not logged in") when there
// is no active login.
func (codex) StatusArgs() []string { return []string{"login", "status"} }

func (codex) LoggedInStatus(output string, runErr error) bool {
	return runErr == nil && !strings.Contains(strings.ToLower(output), "not logged in")
}
