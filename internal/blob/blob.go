// Package blob is Harbor's file data plane. It stores and retrieves the files
// exchanged with peers, so A2A messages carry only references.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// ErrUnsupportedURI reports a URI a store cannot handle.
var ErrUnsupportedURI = errors.New("unsupported uri")

// UploadTarget is a place to upload a file and the reference to advertise for
// it. A reference-mode store returns a base in URL/Reference that the sender
// completes with a key; a broker returns a concrete presigned URL and reference.
type UploadTarget struct {
	URL       string            // where the sender PUTs
	Method    string            // HTTP method, usually "PUT"
	Headers   map[string]string // extra request headers
	Reference string            // the URI to put in the A2A FilePart; empty means URL
	KeyRule   string            // reference mode: how the sender forms the key (e.g. "{sha256}")
	Expires   time.Time         // broker targets only
}

// Reference returns the URI to advertise, defaulting to URL.
func (t UploadTarget) Ref() string {
	if t.Reference != "" {
		return t.Reference
	}
	return t.URL
}

// Store is this process's own file backend: it serves the process's ingest.
type Store interface {
	// Get reads uri back into bytes. It returns ErrUnsupportedURI when the URI's
	// scheme or host is not this store's.
	Get(ctx context.Context, uri string) (io.ReadCloser, error)
	// Target returns the static upload target advertised on the card (reference
	// mode); the zero value when the store has none.
	Target() UploadTarget
	// Reserve issues a per-file, presigned upload target (broker mode).
	Reserve(ctx context.Context, name, mediaType string, size int64) (UploadTarget, error)
	// Put writes a file into the store itself and returns its reference. It backs
	// the process's own ingest endpoint.
	Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (uri string, err error)
}

// Reaper is implemented by a store whose objects Harbor must clean up itself
// (a store with no lifecycle policy). It removes objects older than maxAge.
type Reaper interface {
	Reap(ctx context.Context, maxAge time.Duration) (int, error)
}

// New builds the configured store, or returns nil when storage is disabled.
func New(cfg config.StorageConfig) (Store, error) {
	var st Store
	var err error
	switch cfg.Backend {
	case "":
		return nil, nil
	case "local":
		st, err = newLocalStore(cfg)
	case "http":
		st, err = newHTTPStore(cfg)
	case "s3":
		st, err = newS3Store(cfg)
	case "cos":
		st, err = newCOSStore(cfg)
	default:
		return nil, fmt.Errorf("blob: unknown backend %q", cfg.Backend)
	}
	if err != nil {
		return nil, err
	}
	return instrumented{Store: st, backend: cfg.Backend}, nil
}

// instrumented counts a store's operations for the metrics endpoint.
type instrumented struct {
	Store
	backend string
}

func (s instrumented) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	rc, err := s.Store.Get(ctx, uri)
	count("get", s.backend, resultLabel(err))
	return rc, err
}

func (s instrumented) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	uri, err := s.Store.Put(ctx, name, mediaType, r, size)
	count("put", s.backend, resultLabel(err))
	return uri, err
}

func (s instrumented) Reserve(ctx context.Context, name, mediaType string, size int64) (UploadTarget, error) {
	t, err := s.Store.Reserve(ctx, name, mediaType, size)
	count("reserve", s.backend, resultLabel(err))
	return t, err
}

// Reap delegates to the wrapped store when it has a lifecycle policy.
func (s instrumented) Reap(ctx context.Context, maxAge time.Duration) (int, error) {
	r, ok := s.Store.(Reaper)
	if !ok {
		return 0, ErrUnsupportedURI
	}
	return r.Reap(ctx, maxAge)
}

// Upload writes r to a target a receiver advertised. Targets are HTTP today, so
// the sender needs no store credentials of its own. A nil client uses the
// default HTTP client.
func Upload(ctx context.Context, client *http.Client, t UploadTarget, r io.Reader, size int64) (err error) {
	defer func() { count("upload", schemeOf(t.URL), resultLabel(err)) }()
	if client == nil {
		client = http.DefaultClient
	}
	method := t.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, r)
	if err != nil {
		return err
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("blob: upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("blob: upload: status %d", resp.StatusCode)
	}
	return nil
}

// NewClient builds an HTTP client honoring the TLS configuration, for the sender
// side to reach an advertised upload target.
func NewClient(tls config.TLSConfig) (*http.Client, error) {
	return newHTTPClient(tls)
}

// contentKey hashes data and returns a content-addressed key under prefix,
// preserving the original extension. Equal content maps to the same key, so
// repeated uploads deduplicate.
func contentKey(prefix, name string, data []byte) string {
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	return path.Join(prefix, h[:2], h+path.Ext(name))
}
