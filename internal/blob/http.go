package blob

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// maxRedirects caps how many redirects the client follows.
const maxRedirects = 5

// httpStore is a self-hosted plain HTTP store (nginx/OpenResty): it accepts PUTs
// under put_url and serves reads under get_base.
type httpStore struct {
	putBase     string
	getBase     string
	keyRule     string
	prefix      string
	auth        config.HTTPStoreConfig
	credentials []config.StoreCredentialConfig
	client      *http.Client
	presign     time.Duration

	mu      sync.Mutex
	written map[string]time.Time // object key -> last write, for TTL reaping
}

func newHTTPStore(cfg config.StorageConfig) (Store, error) {
	client, err := newHTTPClient(cfg.HTTP.TLS)
	if err != nil {
		return nil, err
	}
	return &httpStore{
		putBase:     strings.TrimRight(cfg.HTTP.PutURL, "/"),
		getBase:     strings.TrimRight(cfg.HTTP.GetBase, "/"),
		keyRule:     "{sha256}",
		prefix:      cfg.Prefix,
		auth:        cfg.HTTP,
		credentials: cfg.Download.Credential,
		client:      client,
		presign:     cfg.HTTP.PresignTTL,
		written:     map[string]time.Time{},
	}, nil
}

// Target advertises the write base; the sender appends a key and reads it back
// from the read base (get_base, or put_url when unset).
func (s *httpStore) Target() UploadTarget {
	if s.putBase == "" {
		return UploadTarget{}
	}
	return UploadTarget{URL: s.putBase + "/", Reference: s.readBase() + "/", Method: "PUT", KeyRule: s.keyRule}
}

// readBase is the base a reference is read from.
func (s *httpStore) readBase() string {
	if s.getBase != "" {
		return s.getBase
	}
	return s.putBase
}

// Reserve returns a presigned PUT target: from a configured signing service
// (presign_url) when set, else a locally computed nginx secure_link URL.
func (s *httpStore) Reserve(ctx context.Context, name, _ string, _ int64) (UploadTarget, error) {
	rel := path.Join(s.prefix, path.Base(name))
	if s.auth.PresignURL != "" {
		return s.reserveViaService(ctx, rel)
	}
	if s.auth.PresignSecret == "" {
		return UploadTarget{}, ErrUnsupportedURI
	}
	expires := time.Now().Add(s.presign).Unix()
	uri := "/" + rel
	sig := secureLinkMD5(uri, expires, s.auth.PresignSecret)
	u := fmt.Sprintf("%s%s?md5=%s&expires=%d", s.putBase, uri, url.QueryEscape(sig), expires)
	return UploadTarget{URL: u, Method: "PUT", Reference: s.readBase() + uri, Expires: time.Unix(expires, 0)}, nil
}

// reserveViaService asks an external signing service for a PUT target.
func (s *httpStore) reserveViaService(ctx context.Context, rel string) (UploadTarget, error) {
	body, err := json.Marshal(map[string]any{"key": rel, "method": "PUT", "expires": int64(s.presign.Seconds())})
	if err != nil {
		return UploadTarget{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.auth.PresignURL, bytes.NewReader(body))
	if err != nil {
		return UploadTarget{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.setAuth(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return UploadTarget{}, fmt.Errorf("blob: presign: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, "presign"); err != nil {
		return UploadTarget{}, err
	}
	var out struct {
		UploadURL string `json:"uploadUrl"`
		Reference string `json:"reference"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return UploadTarget{}, fmt.Errorf("blob: presign decode: %w", err)
	}
	if out.UploadURL == "" {
		return UploadTarget{}, fmt.Errorf("blob: presign returned no uploadUrl")
	}
	if out.Reference == "" {
		out.Reference = out.UploadURL
	}
	return UploadTarget{URL: out.UploadURL, Method: "PUT", Reference: out.Reference}, nil
}

func (s *httpStore) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	f, n, sum, err := spool(r)
	if err != nil {
		return "", fmt.Errorf("blob: http put: %w", err)
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	rel := contentKeySum(s.prefix, name, sum)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.putBase+"/"+rel, f)
	if err != nil {
		return "", err
	}
	req.ContentLength = n // a file body would otherwise be sent chunked
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	s.setAuth(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("blob: http put: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := checkStatus(resp, "http put"); err != nil {
		return "", err
	}
	s.track(rel)
	return s.readBase() + "/" + rel, nil
}

// track records a written object for TTL reaping.
func (s *httpStore) track(rel string) {
	s.mu.Lock()
	s.written[rel] = time.Now()
	s.mu.Unlock()
}

// Reap deletes tracked objects older than maxAge (the store must accept DELETE,
// for example nginx's WebDAV module). Objects it did not write are not touched.
func (s *httpStore) Reap(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	s.mu.Lock()
	var expired []string
	for rel, t := range s.written {
		if t.Before(cutoff) {
			expired = append(expired, rel)
		}
	}
	s.mu.Unlock()

	removed := 0
	for _, rel := range expired {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.putBase+"/"+rel, nil)
		if err != nil {
			continue
		}
		s.setAuth(req)
		resp, err := s.client.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			continue
		}
		s.mu.Lock()
		delete(s.written, rel)
		s.mu.Unlock()
		removed++
	}
	return removed, nil
}

func (s *httpStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	return s.fetch(ctx, uri, "", 0)
}

// GetRange reads length bytes from offset, using an HTTP Range request.
func (s *httpStore) GetRange(ctx context.Context, uri string, offset, length int64) (io.ReadCloser, error) {
	return s.fetch(ctx, uri, rangeSpec(offset, length), offset)
}

// fetch issues a GET with an optional Range header. A 200 reply to a non-zero
// range offset means the server ignored the range, so it fails rather than
// returning the wrong bytes.
func (s *httpStore) fetch(ctx context.Context, uri, rangeHeader string, offset int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, ErrUnsupportedURI
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: http get: %w", err)
	}
	if err := objectError(resp, offset, "http get"); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Size reports the object's size via a HEAD request.
func (s *httpStore) Size(ctx context.Context, uri string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, uri, nil)
	if err != nil {
		return 0, ErrUnsupportedURI
	}
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("blob: http head: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := objectError(resp, 0, "http head"); err != nil {
		return 0, err
	}
	return resp.ContentLength, nil
}

// Reference maps a relative key to its read URL.
func (s *httpStore) Reference(key string) (string, bool) { return s.readBase() + "/" + key, true }

// setAuth applies Harbor's own credential for reaching the store.
func (s *httpStore) setAuth(req *http.Request) {
	switch s.auth.Auth {
	case "basic":
		req.SetBasicAuth(s.auth.Username, s.auth.Password)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+s.auth.Token)
	}
}

// authorize applies the origin credential for the request's host when one is
// configured, else the store's own credential.
func (s *httpStore) authorize(req *http.Request) {
	host := req.URL.Hostname()
	for _, c := range s.credentials {
		if !strings.EqualFold(c.Host, host) {
			continue
		}
		switch c.Auth {
		case "basic":
			req.SetBasicAuth(c.User, c.Pass)
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		return
	}
	s.setAuth(req)
}

// secureLinkMD5 computes nginx secure_link's base64url(md5(expr)).
func secureLinkMD5(uri string, expires int64, secret string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%d%s %s", expires, uri, secret)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newHTTPClient builds a client honoring the TLS configuration.
func newHTTPClient(cfg config.TLSConfig) (*http.Client, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // opt-in
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("blob: tls ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("blob: tls ca: no certificates in %s", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("blob: tls client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}, nil
}
