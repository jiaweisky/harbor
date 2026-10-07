package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// authProbeTimeout bounds a login-status probe so startup is not blocked.
const authProbeTimeout = 10 * time.Second

// loginTimeout bounds a startup login attempt, which may require the operator
// to complete a browser or device-code step.
const loginTimeout = 5 * time.Minute

// ErrAuthRequired indicates the CLI has no usable credential, so the task must
// not start. It maps to the A2A auth_required error code.
var ErrAuthRequired = errors.New("cli authentication required")

// ErrNoLogin indicates the CLI cannot perform a headless login.
var ErrNoLogin = errors.New("cli has no headless login")

// Authenticator describes a coding-agent CLI's authentication surface. Each
// supported CLI provides its own implementation, so Harbor stays CLI-agnostic.
// Harbor holds a single credential per CLI, obtained under the OS user it runs
// as.
type Authenticator interface {
	// LoggedIn reports whether a usable credential is present. home is the base
	// home directory (the configured CLI home, or the real user home) and env is
	// the effective environment, so a CLI can resolve its own config locations.
	// An implementation that cannot verify returns true.
	LoggedIn(home string, env map[string]string) bool
	// LoginArgs returns the args of the CLI's login command, or nil when the
	// CLI has no headless login.
	LoginArgs() []string
	// StatusArgs returns the args of the CLI's login-status command, or nil when
	// the CLI has no such command.
	StatusArgs() []string
	// LoggedInStatus interprets the login-status command's output. It is called
	// only when StatusArgs is non-nil.
	LoggedInStatus(output string, runErr error) bool
}

// CheckStatus runs the CLI's login-status command and reports whether it is
// logged in. It returns true when the CLI has no status command, so an
// unverifiable CLI never reports unready.
func CheckStatus(ctx context.Context, binary string, env []string, auth Authenticator) bool {
	if auth == nil {
		return true
	}
	args := auth.StatusArgs()
	if len(args) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, authProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return auth.LoggedInStatus(string(out), err)
}

// Authenticated reports whether the CLI already holds a usable ticket, using a
// live status command when the CLI offers one, otherwise a static check of the
// environment and the CLI's home.
func Authenticated(ctx context.Context, binary, home string, env []string, auth Authenticator) bool {
	if auth == nil {
		return true
	}
	if len(auth.StatusArgs()) > 0 {
		return CheckStatus(ctx, binary, env, auth)
	}
	return auth.LoggedIn(home, EnvMap(env))
}

// EnsureLogin verifies a CLI holds a ticket and, when it does not, attempts its
// login command once and re-checks. A failure returns ErrAuthRequired. A nil
// authenticator is treated as already authenticated.
func EnsureLogin(ctx context.Context, name, binary, home string, env []string, auth Authenticator) error {
	if auth == nil {
		return nil
	}
	if Authenticated(ctx, binary, home, env, auth) {
		return nil
	}
	args := auth.LoginArgs()
	if len(args) == 0 {
		return ErrAuthRequired
	}
	slog.Warn("cli has no ticket; attempting login", "agent", name, "timeout", loginTimeout)
	if err := RunLogin(ctx, name, binary, args, env); err != nil {
		return fmt.Errorf("%w: %v", ErrAuthRequired, err)
	}
	if !Authenticated(ctx, binary, home, env, auth) {
		return ErrAuthRequired
	}
	return nil
}

// RunLogin runs the CLI's login command, streaming its output to the log (so an
// operator can see a device-code URL) until it exits or the timeout elapses.
func RunLogin(ctx context.Context, name, binary string, args, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			slog.Info("cli login", "agent", name, "line", sc.Text())
		}
	}()
	err := cmd.Wait()
	pw.Close()
	pr.Close()
	if err != nil {
		return fmt.Errorf("login command failed: %w", err)
	}
	return nil
}

// EnvMap returns the environment as a name->value map (later entries win), so
// adapters can resolve config locations from the values a CLI will actually see.
func EnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			m[k] = v
		}
	}
	return m
}
