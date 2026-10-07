package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrCLINotFound indicates the CLI binary is missing or not executable.
var ErrCLINotFound = errors.New("cli not found")

// Process tracks one running CLI process group.
type Process struct {
	cmd      *exec.Cmd
	pgid     int
	stdin    io.WriteCloser
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	canceled atomic.Bool
}

// stop marks the process stopped and closes stopped so blocked event sends
// abort instead of leaking. It is idempotent.
func (p *Process) stop() {
	p.canceled.Store(true)
	p.stopOnce.Do(func() { close(p.stopped) })
}

// Runner spawns and supervises CLI subprocesses.
type Runner struct {
	sem        chan struct{}
	inheritEnv bool
	cliHome    string
	procs      sync.Map // taskID -> *Process
}

// NewRunner creates a Runner limiting concurrency to maxConcurrent (<=0 =
// unbounded). When inheritEnv is false the child receives only an allowlisted
// base environment plus the adapter-provided variables. When cliHome is set the
// child's HOME (and XDG dirs) are redirected under it.
func NewRunner(maxConcurrent int, inheritEnv bool, cliHome string) *Runner {
	var sem chan struct{}
	if maxConcurrent > 0 {
		sem = make(chan struct{}, maxConcurrent)
	}
	return &Runner{sem: sem, inheritEnv: inheritEnv, cliHome: cliHome}
}

// Start runs the CLI and streams normalized events parsed from its stdout.
func (r *Runner) Start(ctx context.Context, taskID string, inv *Invocation, p Parser) (<-chan Event, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}

	// exec.Command (not CommandContext): cancellation is handled here so the
	// whole process group is terminated, not just the direct child.
	cmd := exec.Command(inv.Bin, inv.Args...)
	cmd.Dir = inv.Dir
	cmd.Env = buildEnv(r.inheritEnv, r.cliHome, inv.Env)
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.release()
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.release()
		return nil, err
	}
	stderr := newLimitedBuffer(64 << 10)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		r.release()
		return nil, classifyStartErr(err)
	}
	r.feedStdin(stdin, inv)

	out := make(chan Event, 64)
	proc := &Process{
		cmd:     cmd,
		pgid:    cmd.Process.Pid,
		stdin:   stdin,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	r.procs.Store(taskID, proc)
	go r.watchCancellation(ctx, proc)
	go r.readLoop(stdout, p, out, stderr, proc, taskID)

	return out, nil
}

// feedStdin copies an optional initial payload and, unless the invocation wants
// a duplex pipe, closes stdin at EOF.
func (r *Runner) feedStdin(stdin io.WriteCloser, inv *Invocation) {
	if inv.Stdin == nil {
		return
	}
	go func() {
		_, _ = io.Copy(stdin, inv.Stdin)
		if !inv.KeepStdinOpen {
			_ = stdin.Close()
		}
	}()
}

// readLoop parses stdout line by line, then waits for the process. It owns the
// event channel's lifetime and removes the process entry when it finishes.
//
// Event sends block when the buffer is full (backpressure); they abort as soon
// as the process is stopped, so a consumer that goes away never leaks this
// goroutine or its concurrency slot.
func (r *Runner) readLoop(
	stdout io.Reader, p Parser, out chan<- Event,
	stderr fmt.Stringer, proc *Process, taskID string,
) {
	defer close(out)
	defer close(proc.done)
	defer r.release()
	defer r.procs.CompareAndDelete(taskID, proc)

	alive := true
	emit := func(e Event) {
		if !alive {
			return
		}
		select {
		case out <- e:
		case <-proc.stopped:
			alive = false
		}
	}

	rd := bufio.NewReaderSize(stdout, 64*1024)
	for alive {
		line, readErr := rd.ReadBytes('\n')
		if rec := trimLine(line); len(rec) > 0 {
			if perr := p.Parse(rec, emit); perr != nil {
				emit(Event{Kind: EventError, Err: perr})
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				emit(Event{Kind: EventError, Err: readErr})
			}
			break
		}
	}
	_ = p.Flush(emit)

	// Wait always runs so the process is reaped even after an early stop.
	if werr := proc.cmd.Wait(); werr != nil && !proc.canceled.Load() {
		emit(Event{Kind: EventError, Err: fmt.Errorf("cli exit: %w; stderr=%s", werr, truncate(stderr.String(), 2000))})
	}
}

// watchCancellation terminates the process group if ctx is canceled before exit.
func (r *Runner) watchCancellation(ctx context.Context, proc *Process) {
	select {
	case <-ctx.Done():
		proc.stop()
		terminateGroup(proc)
		select {
		case <-proc.done:
		case <-time.After(5 * time.Second):
			killGroup(proc)
		}
	case <-proc.done:
	}
}

// Kill terminates the process group for taskID (SIGTERM, then SIGKILL).
func (r *Runner) Kill(taskID string) {
	v, ok := r.procs.LoadAndDelete(taskID)
	if !ok {
		return
	}
	p := v.(*Process)
	p.stop()
	terminateGroup(p)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		killGroup(p)
	}
}

// Stdin returns the process stdin for duplex sessions.
func (r *Runner) Stdin(taskID string) io.WriteCloser {
	if v, ok := r.procs.Load(taskID); ok {
		return v.(*Process).stdin
	}
	return nil
}

func (r *Runner) acquire(ctx context.Context) error {
	if r.sem == nil {
		return nil
	}
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) release() {
	if r.sem != nil {
		<-r.sem
	}
}

// baseEnvKeys are the parent variables kept when environment inheritance is
// disabled (sandbox.inherit_env: false); every other parent variable is dropped.
// Adapter-provided variables are always added on top of this set. The Windows
// roots are included so a CLI there can still find its profile.
var baseEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"LANG": true, "LC_ALL": true, "TERM": true, "TMPDIR": true,
}

// buildEnv returns the child environment. When inherit is false only an
// allowlisted base plus the adapter extras is passed (no credential leakage).
// When cliHome is set, HOME and the CLI config roots are redirected under it;
// those redirects win over any adapter-provided values so isolation cannot be
// overridden.
func buildEnv(inherit bool, cliHome string, extra []string) []string {
	base := os.Environ()
	if !inherit {
		base = allowlistedEnv()
	}
	return append(base, append(extra, cliHomeEnv(cliHome)...)...)
}

// Env returns the full environment a child invocation would receive, so callers
// can inspect it (for example to check credentials) before starting.
func (r *Runner) Env(inv *Invocation) []string {
	return buildEnv(r.inheritEnv, r.cliHome, inv.Env)
}

// AuthBase returns the base home directory for credential checks: the
// configured CLI home, otherwise the process user's real home.
func (r *Runner) AuthBase() string {
	if r.cliHome != "" {
		return r.cliHome
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func allowlistedEnv() []string {
	env := make([]string, 0, len(baseEnvKeys))
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); baseEnvKeys[k] {
			env = append(env, kv)
		}
	}
	return env
}

// cliHomeEnv redirects a CLI's config/home to the configured directory. It sets
// the roots each supported CLI resolves its config under, so the CLI derives the
// platform-correct subpaths itself.
func cliHomeEnv(cliHome string) []string {
	if cliHome == "" {
		return nil
	}
	home := cliHome
	return []string{
		// Unix roots.
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		// Windows roots: a CLI on Windows resolves its config from these
		// instead of HOME, so setting only HOME would leak the real profile.
		"USERPROFILE=" + home,
		"APPDATA=" + filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA=" + filepath.Join(home, "AppData", "Local"),
		// CLI-specific config roots.
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEBUDDY_CONFIG_DIR=" + filepath.Join(home, ".codebuddy"),
		"OPENCODE_CONFIG_DIR=" + filepath.Join(home, ".config", "opencode"),
	}
}

func classifyStartErr(err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrCLINotFound, err)
	}
	return err
}

func trimLine(line []byte) []byte {
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return nil
	}
	return append([]byte(nil), line...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// limitedBuffer keeps at most max bytes; extra writes are dropped.
type limitedBuffer struct {
	buf []byte
	max int
}

func newLimitedBuffer(max int) *limitedBuffer { return &limitedBuffer{max: max} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.buf) }
