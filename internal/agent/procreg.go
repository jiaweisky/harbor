package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// procFilePrefix names the per-instance record of live CLI process groups. The
// suffix is the Harbor PID, so a startup can tell its own live siblings (which
// must be left alone) from records left by crashed instances.
const procFilePrefix = "procs."

// ProcRegistry records the process groups of running CLIs so that a crash does
// not leave orphans behind: a later startup reads the record and kills any
// group still alive.
type ProcRegistry struct {
	path string
	mu   sync.Mutex
	live map[int]string // pgid -> resolved binary path
}

// newProcRegistry starts an empty registry at path. Any stale record under the
// same name is overwritten: records of crashed instances are handled by
// SweepOrphans, and this registry tracks only this process's children.
func newProcRegistry(path string) *ProcRegistry {
	r := &ProcRegistry{path: path, live: map[int]string{}}
	r.mu.Lock()
	r.persistLocked()
	r.mu.Unlock()
	return r
}

// Add records a process group and its binary.
func (r *ProcRegistry) Add(pgid int, binary string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[pgid] = binary
	r.persistLocked()
}

// Remove forgets a process group.
func (r *ProcRegistry) Remove(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, pgid)
	r.persistLocked()
}

// persistLocked writes the record atomically. It is called with the lock held.
func (r *ProcRegistry) persistLocked() {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(r.live)
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}

// NewProcRegistry starts an empty registry for this process under dir. Records
// of live siblings are left alone; records from crashed instances are handled by
// SweepOrphans.
func NewProcRegistry(dir string) *ProcRegistry {
	return newProcRegistry(filepath.Join(dir, fmt.Sprintf("%s%d", procFilePrefix, os.Getpid())))
}

// SweepOrphans kills CLI process groups recorded by previous Harbor runs that
// are still alive (for example after a crash). Records whose owning Harbor
// process is still running are left alone, so concurrent instances do not kill
// each other's children. A group is killed only when its command line still
// matches the recorded binary, to guard against PID reuse.
func SweepOrphans(dir string) {
	paths, _ := filepath.Glob(filepath.Join(dir, procFilePrefix+"*"))
	for _, path := range paths {
		if strings.HasSuffix(path, ".tmp") || ownerAlive(path) {
			continue
		}
		sweepFile(path)
	}
}

// ownerAlive reports whether the Harbor process that wrote a record is still
// running.
func ownerAlive(path string) bool {
	pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), procFilePrefix))
	if err != nil {
		return false
	}
	return processAlive(pid)
}

// sweepFile kills the still-matching groups in one record and removes it.
func sweepFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var live map[int]string
	if json.Unmarshal(b, &live) != nil {
		_ = os.Remove(path)
		return
	}
	for pgid, binary := range live {
		if pgid > 0 && processMatches(pgid, binary) {
			killProcGroup(pgid)
			slog.Warn("killed orphaned CLI process group", "pgid", pgid, "binary", binary)
		}
	}
	_ = os.Remove(path)
}
