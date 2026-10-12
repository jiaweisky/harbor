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
	"os"
	"path"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// ErrUnsupportedURI reports a URI a store cannot handle.
var ErrUnsupportedURI = errors.New("unsupported uri")

// ErrNotFound reports that the requested object does not exist.
var ErrNotFound = errors.New("object not found")

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
	// Get opens the object at uri for reading. It returns ErrUnsupportedURI when
	// the URI's scheme or host is not this store's, and ErrNotFound when the
	// object does not exist.
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

// RangeReader is implemented by a store that can read a byte range of an object.
// A length of -1 means "to the end". It lets a large object be streamed without
// materializing it whole.
type RangeReader interface {
	GetRange(ctx context.Context, uri string, offset, length int64) (io.ReadCloser, error)
}

// Sizer is implemented by a store that can report an object's size.
type Sizer interface {
	Size(ctx context.Context, uri string) (int64, error)
}

// KeyResolver is implemented by a store that can map a store-relative key back
// to its reference URI, so an HTTP endpoint can serve a stored object.
type KeyResolver interface {
	Reference(key string) (string, bool)
}

// ServeContent serves the object at uri over HTTP, honoring Range requests so a
// large object can be streamed. The store must support ranged reads and sizing.
func ServeContent(w http.ResponseWriter, r *http.Request, store Store, uri, name string) error {
	sizer, ok := store.(Sizer)
	if !ok {
		return ErrUnsupportedURI
	}
	size, err := sizer.Size(r.Context(), uri)
	if err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("blob: unknown size for %s", uri)
	}
	rs, err := openRange(r.Context(), store, uri, size)
	if err != nil {
		return err
	}
	defer rs.Close()
	http.ServeContent(w, r, name, time.Time{}, rs)
	return nil
}

// readSeeker is a seekable reader over one stored object: it fetches the object
// in ranges on demand, so callers such as http.ServeContent can seek within a
// large object without holding it whole.
type readSeeker struct {
	ctx    context.Context
	ranges RangeReader
	uri    string
	size   int64
	off    int64
	rc     io.ReadCloser
}

// openRange returns a seekable reader over uri of the given size.
func openRange(ctx context.Context, store Store, uri string, size int64) (*readSeeker, error) {
	r, ok := store.(RangeReader)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	return &readSeeker{ctx: ctx, ranges: r, uri: uri, size: size}, nil
}

func (s *readSeeker) Read(p []byte) (int, error) {
	if s.off >= s.size {
		return 0, io.EOF
	}
	if s.rc == nil {
		rc, err := s.ranges.GetRange(s.ctx, s.uri, s.off, -1)
		if err != nil {
			return 0, err
		}
		s.rc = rc
	}
	n, err := s.rc.Read(p)
	s.off += int64(n)
	if err == io.EOF {
		s.rc.Close()
		s.rc = nil
	}
	return n, err
}

func (s *readSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.off + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, fmt.Errorf("blob: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("blob: negative seek position")
	}
	if s.rc != nil {
		s.rc.Close()
		s.rc = nil
	}
	s.off = abs
	return abs, nil
}

func (s *readSeeker) Close() error {
	if s.rc != nil {
		err := s.rc.Close()
		s.rc = nil
		return err
	}
	return nil
}

// rangeSpec renders an HTTP Range header value; a negative length means "to the
// end".
func rangeSpec(offset, length int64) string {
	if length < 0 {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
}

// limitedReadCloser caps Reads at a byte count and closes the underlying reader.
type limitedReadCloser struct {
	r  io.Reader
	rc io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.rc.Close() }

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

// CanReap reports whether store has an object-lifecycle policy Harbor must run
// itself. The instrumented wrapper always exposes Reap, so this inspects the
// wrapped store rather than the wrapper.
func CanReap(store Store) bool {
	if s, ok := store.(instrumented); ok {
		_, ok = s.Store.(Reaper)
		return ok
	}
	_, ok := store.(Reaper)
	return ok
}

// instrumented counts a store's operations for the metrics endpoint.
type instrumented struct {
	Store
	backend string
}

func (s instrumented) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	start := time.Now()
	rc, err := s.Store.Get(ctx, uri)
	record("get", s.backend, err, start, "uri", redactURL(uri))
	return rc, err
}

func (s instrumented) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	start := time.Now()
	uri, err := s.Store.Put(ctx, name, mediaType, r, size)
	record("put", s.backend, err, start, "name", name, "size", size)
	return uri, err
}

func (s instrumented) Reserve(ctx context.Context, name, mediaType string, size int64) (UploadTarget, error) {
	start := time.Now()
	t, err := s.Store.Reserve(ctx, name, mediaType, size)
	record("reserve", s.backend, err, start, "name", name, "size", size)
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

// GetRange delegates to the wrapped store when it supports ranged reads.
func (s instrumented) GetRange(ctx context.Context, uri string, offset, length int64) (io.ReadCloser, error) {
	r, ok := s.Store.(RangeReader)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	return r.GetRange(ctx, uri, offset, length)
}

// Size delegates to the wrapped store when it can report object sizes.
func (s instrumented) Size(ctx context.Context, uri string) (int64, error) {
	z, ok := s.Store.(Sizer)
	if !ok {
		return 0, ErrUnsupportedURI
	}
	return z.Size(ctx, uri)
}

// Reference delegates to the wrapped store when it can map keys to URIs.
func (s instrumented) Reference(key string) (string, bool) {
	r, ok := s.Store.(KeyResolver)
	if !ok {
		return "", false
	}
	return r.Reference(key)
}

// Upload writes r to a target a receiver advertised, returning the reference
// the receiver reported in X-Harbor-Reference (empty when it reported none, so
// the caller falls back to the advertised reference). Targets are HTTP today, so
// the sender needs no store credentials of its own. A nil client uses the
// default HTTP client.
func Upload(ctx context.Context, client *http.Client, t UploadTarget, r io.Reader, size int64) (reference string, err error) {
	start := time.Now()
	defer func() { record("upload", schemeOf(t.URL), err, start, "url", redactURL(t.URL), "size", size) }()
	if client == nil {
		client = http.DefaultClient
	}
	method := t.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, r)
	if err != nil {
		return "", err
	}
	if size > 0 {
		// A non-*bytes/*strings reader (an open file) has an unknown length, so
		// the client would otherwise send a chunked body, which object stores
		// reject. The size is known, so declare it.
		req.ContentLength = size
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("blob: upload: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, "upload"); err != nil {
		return "", err
	}
	return resp.Header.Get("X-Harbor-Reference"), nil
}

// NewClient builds an HTTP client honoring the TLS configuration, for the sender
// side to reach an advertised upload target.
func NewClient(tls config.TLSConfig) (*http.Client, error) {
	return newHTTPClient(tls)
}

// contentKeySum returns a content-addressed key under prefix from a SHA-256
// digest, preserving the original extension. Equal content maps to the same key,
// so repeated uploads deduplicate.
func contentKeySum(prefix, name string, sum []byte) string {
	h := hex.EncodeToString(sum)
	return path.Join(prefix, h[:2], h+path.Ext(name))
}

// checkStatus returns an error when resp is not a 2xx, naming the operation op.
func checkStatus(resp *http.Response, op string) error {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("blob: %s: status %d", op, resp.StatusCode)
	}
	return nil
}

// objectError classifies an object GET/HEAD response: a missing object, a
// server that ignored a non-zero Range, or a non-2xx status.
func objectError(resp *http.Response, offset int64, op string) error {
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if offset > 0 && resp.StatusCode == http.StatusOK {
		return fmt.Errorf("blob: %s: server ignored the range", op)
	}
	return checkStatus(resp, op)
}

// spool buffers a reader to a temporary file while hashing it, so a large upload
// is not held in memory. The caller closes and removes the file.
func spool(r io.Reader) (*os.File, int64, []byte, error) {
	f, err := os.CreateTemp("", "harbor-blob-*")
	if err != nil {
		return nil, 0, nil, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, err
	}
	return f, size, h.Sum(nil), nil
}
