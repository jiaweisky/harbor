package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// localStore is a shared-directory store: it is meaningful only when the peers
// share a filesystem (same host, or a shared NFS/SMB mount).
type localStore struct {
	root      string
	serveBase string
	prefix    string
}

func newLocalStore(cfg config.StorageConfig) (Store, error) {
	root, err := filepath.Abs(cfg.Local.Root)
	if err != nil {
		return nil, fmt.Errorf("blob: local root: %w", err)
	}
	return &localStore{root: root, serveBase: strings.TrimRight(cfg.Local.ServeBaseURL, "/"), prefix: cfg.Prefix}, nil
}

// Target advertises the shared directory. Without serve_base_url the base is a
// file:// path, usable only by a peer on the same host or mount.
func (s *localStore) Target() UploadTarget {
	base := s.serveBase + "/"
	if s.serveBase == "" {
		base = "file://" + s.root + "/"
	}
	return UploadTarget{URL: base, Reference: base, Method: "PUT", KeyRule: "{sha256}"}
}

// Reserve is unsupported: the local store has no signing service.
func (s *localStore) Reserve(context.Context, string, string, int64) (UploadTarget, error) {
	return UploadTarget{}, ErrUnsupportedURI
}

func (s *localStore) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("blob: local put: %w", err)
	}
	rel := contentKey(s.prefix, name, data)
	dst, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("blob: local put: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", fmt.Errorf("blob: local put: %w", err)
	}
	return s.ref(rel), nil
}

func (s *localStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	rel, ok := s.rel(uri)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	p, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// ref renders the reference URI for a relative key.
func (s *localStore) ref(rel string) string {
	if s.serveBase != "" {
		return s.serveBase + "/" + rel
	}
	return "file://" + filepath.Join(s.root, rel)
}

// rel maps a reference URI to a relative key within root.
func (s *localStore) rel(uri string) (string, bool) {
	switch {
	case s.serveBase != "" && strings.HasPrefix(uri, s.serveBase+"/"):
		return strings.TrimPrefix(uri, s.serveBase+"/"), true
	case strings.HasPrefix(uri, "file://"):
		abs := strings.TrimPrefix(uri, "file://")
		r, err := filepath.Rel(s.root, abs)
		if err != nil {
			return "", false
		}
		return r, true
	}
	return "", false
}

// Reap removes stored objects last modified more than maxAge ago.
func (s *localStore) Reap(_ context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

// resolve turns a relative key into an absolute path confined to root.
func (s *localStore) resolve(rel string) (string, error) {
	p := filepath.Join(s.root, filepath.Clean("/"+rel))
	if p != s.root && !strings.HasPrefix(p, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("blob: path %q escapes root", rel)
	}
	return p, nil
}
