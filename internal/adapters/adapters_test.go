package adapters_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/adapters"
)

func TestKnownAdapters(t *testing.T) {
	cases := []struct {
		binary       string
		env          string
		loginArg     string
		unverifiable bool // LoggedIn cannot report absence (Keychain only)
	}{
		{"claude", "ANTHROPIC_API_KEY", "auth", false},
		{"codebuddy", "CODEBUDDY_API_KEY", "", true},
		{"opencode", "OPENCODE_API_KEY", "auth", false},
		{"codex", "CODEX_API_KEY", "login", false},
	}
	for _, c := range cases {
		auth, ok := adapters.AuthFor("/usr/local/bin/"+c.binary, "1.0")
		if !ok {
			t.Fatalf("AuthFor(%q) not found", c.binary)
		}
		home := t.TempDir()
		if !auth.LoggedIn(home, map[string]string{c.env: "x"}) {
			t.Errorf("%s should be logged in when %s is set", c.binary, c.env)
		}
		got := auth.LoggedIn(home, map[string]string{})
		if c.unverifiable && !got {
			t.Errorf("%s is unverifiable and should not report absence", c.binary)
		}
		if !c.unverifiable && got {
			t.Errorf("%s should not be logged in with no env and no file", c.binary)
		}
		args := auth.LoginArgs()
		if c.loginArg == "" {
			if args != nil {
				t.Errorf("%s LoginArgs = %v, want nil", c.binary, args)
			}
		} else if len(args) == 0 || args[0] != c.loginArg {
			t.Errorf("%s LoginArgs = %v, want first %q", c.binary, args, c.loginArg)
		}
	}
}

func TestLoggedInResolvesConfigLocations(t *testing.T) {
	home := t.TempDir()

	// Default location under the home.
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"))
	claude, _ := adapters.AuthFor("claude", "1.0")
	if !claude.LoggedIn(home, map[string]string{}) {
		t.Error("claude should find credentials under the home")
	}

	// A dedicated override wins over the home.
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, ".credentials.json"))
	other := t.TempDir()
	if !claude.LoggedIn(other, map[string]string{"CLAUDE_CONFIG_DIR": cfgDir}) {
		t.Error("claude should honor CLAUDE_CONFIG_DIR")
	}
	if claude.LoggedIn(other, map[string]string{}) {
		t.Error("claude should not find credentials in an unrelated home")
	}

	codexHome := t.TempDir()
	writeFile(t, filepath.Join(codexHome, "auth.json"))
	codex, _ := adapters.AuthFor("codex", "1.0")
	if !codex.LoggedIn(home, map[string]string{"CODEX_HOME": codexHome}) {
		t.Error("codex should honor CODEX_HOME")
	}

	data := t.TempDir()
	writeFile(t, filepath.Join(data, "opencode", "auth.json"))
	oc, _ := adapters.AuthFor("opencode", "1.0")
	if !oc.LoggedIn(home, map[string]string{"XDG_DATA_HOME": data}) {
		t.Error("opencode should honor XDG_DATA_HOME")
	}
	if oc.LoggedIn(home, map[string]string{}) {
		t.Error("opencode should not report a ticket without a file or env")
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStatusParsing(t *testing.T) {
	claude, _ := adapters.AuthFor("claude", "1.0")
	if claude.StatusArgs() == nil {
		t.Fatal("claude should have a status command")
	}
	if !claude.LoggedInStatus(`{"loggedIn":true}`, nil) {
		t.Error("claude should parse loggedIn:true")
	}
	if claude.LoggedInStatus(`{"loggedIn":false}`, nil) {
		t.Error("claude should parse loggedIn:false")
	}
	if !claude.LoggedInStatus("note: config loaded\n{\"loggedIn\":true}\n", nil) {
		t.Error("claude should parse JSON surrounded by other output")
	}

	codex, _ := adapters.AuthFor("codex", "1.0")
	if !codex.LoggedInStatus("Logged in using ChatGPT", nil) {
		t.Error("codex should report logged in on success")
	}
	if codex.LoggedInStatus("Not logged in", nil) {
		t.Error("codex should not report logged in on 'Not logged in'")
	}
	if codex.LoggedInStatus("", errFake{}) {
		t.Error("codex should not report logged in on error")
	}
}

type errFake struct{}

func (errFake) Error() string { return "failed" }

func TestUnknownAdapterFallsBack(t *testing.T) {
	if _, ok := adapters.AuthFor("my-coder", "1.0"); ok {
		t.Fatal("unknown binary should have no registered adapter")
	}
	auth := adapters.Unverified()
	if !auth.LoggedIn(t.TempDir(), map[string]string{}) {
		t.Error("an unverifiable authenticator should not block")
	}
}
