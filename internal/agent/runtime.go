package agent

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
)

// RawStream writes each task's CLI stdout and stderr under dir for replay and
// debugging. A nil RawStream disables it.
type RawStream struct {
	dir      string
	compress bool
}

// NewRawStream returns nil when dir is empty.
func NewRawStream(dir string, compress bool) *RawStream {
	if dir == "" {
		return nil
	}

	return &RawStream{dir: dir, compress: compress}
}

// Runtime holds the process-wide facilities shared by all runners: orphan
// tracking and raw stream logging. It is created once by the composition root
// and injected into every launcher.
type Runtime struct {
	Procs *ProcRegistry
	Raw   *RawStream
}

// NewRuntime builds the runtime facilities. An empty stateDir disables orphan
// tracking; an empty rawStreamDir disables raw stream logging.
func NewRuntime(stateDir, rawStreamDir string, rawStreamCompress bool) *Runtime {
	return &Runtime{Procs: NewProcRegistry(stateDir), Raw: NewRawStream(rawStreamDir, rawStreamCompress)}
}

// open opens the task's raw stdout/stderr logs; both are nil when disabled.
func (r *RawStream) open(taskID string) (io.WriteCloser, io.WriteCloser) {
	if r == nil {
		return nil, nil
	}

	return openRawFile(filepath.Join(r.dir, taskID+".out"), r.compress),
		openRawFile(filepath.Join(r.dir, taskID+".err"), r.compress)
}

func openRawFile(path string, compress bool) io.WriteCloser {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}

	if compress {
		path += ".gz"
	}

	f, err := os.Create(path)
	if err != nil {
		return nil
	}

	if !compress {
		return f
	}

	return &gzipFile{gz: gzip.NewWriter(f), f: f}
}

// closeRaw closes a raw stream writer if set.
func closeRaw(w io.WriteCloser) {
	if w != nil {
		_ = w.Close()
	}
}

// gzipFile gzips writes and closes the underlying file with the gzip writer.
type gzipFile struct {
	gz *gzip.Writer
	f  *os.File
}

func (g *gzipFile) Write(p []byte) (int, error) { return g.gz.Write(p) }

func (g *gzipFile) Close() error {
	err := g.gz.Close()
	if e := g.f.Close(); err == nil {
		err = e
	}

	return err
}
