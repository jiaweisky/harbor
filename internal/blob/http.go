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
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// httpStore is a self-hosted plain HTTP store (nginx/OpenResty): it accepts PUTs
// under put_url and serves reads under get_base.
type httpStore struct {
	putBase string
	getBase string
	keyRule string
	prefix  string
	auth    config.HTTPStoreConfig
	client  *http.Client
	presign time.Duration
}

func newHTTPStore(cfg config.StorageConfig) (Store, error) {
	client, err := newHTTPClient(cfg.HTTP.TLS)
	if err != nil {
		return nil, err
	}
	return &httpStore{
		putBase: strings.TrimRight(cfg.HTTP.PutURL, "/"),
		getBase: strings.TrimRight(cfg.HTTP.GetBase, "/"),
		keyRule: "{sha256}",
		prefix:  cfg.Prefix,
		auth:    cfg.HTTP,
		client:  client,
		presign: cfg.HTTP.PresignTTL,
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
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return UploadTarget{}, fmt.Errorf("blob: presign: status %d", resp.StatusCode)
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
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("blob: http put: %w", err)
	}
	rel := contentKey(s.prefix, name, data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.putBase+"/"+rel, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	s.setAuth(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("blob: http put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("blob: http put: status %d", resp.StatusCode)
	}
	return s.readBase() + "/" + rel, nil
}

func (s *httpStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, ErrUnsupportedURI
	}
	s.setAuth(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: http get: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("blob: http get: status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// setAuth applies Harbor's own credential for reaching the store.
func (s *httpStore) setAuth(req *http.Request) {
	switch s.auth.Auth {
	case "basic":
		req.SetBasicAuth(s.auth.Username, s.auth.Password)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+s.auth.Token)
	}
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
	}, nil
}
