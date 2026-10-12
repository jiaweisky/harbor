package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// cosStore is a Tencent COS store over its native REST API (q-sign-algorithm=sha1),
// needing no S3 SDK.
type cosStore struct {
	scheme       string
	endpoint     string // host
	bucket       string
	region       string
	secretID     string
	secretKey    string
	sessionToken string
	prefix       string
	presignTTL   time.Duration
	client       *http.Client
}

func newCOSStore(cfg config.StorageConfig) (Store, error) {
	client, err := newHTTPClient(cfg.COS.TLS)
	if err != nil {
		return nil, err
	}
	scheme, endpoint := "https", cfg.COS.Endpoint
	if endpoint == "" {
		endpoint = cfg.COS.Bucket + ".cos." + cfg.COS.Region + ".myqcloud.com"
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		scheme, endpoint = u.Scheme, u.Host
	}
	return &cosStore{
		scheme: scheme, endpoint: endpoint, bucket: cfg.COS.Bucket, region: cfg.COS.Region,
		secretID: cfg.COS.SecretID, secretKey: cfg.COS.SecretKey, sessionToken: cfg.COS.SessionToken,
		prefix: cfg.Prefix, presignTTL: cfg.COS.PresignTTL, client: client,
	}, nil
}

// Target returns no static target: COS peers are reached through broker
// presigning, which carries its own credential.
func (s *cosStore) Target() UploadTarget { return UploadTarget{} }

// Reserve returns a presigned PUT and the cos:// reference for it.
func (s *cosStore) Reserve(_ context.Context, name, mediaType string, _ int64) (UploadTarget, error) {
	key := s.key(name)
	expires := time.Now().Add(s.presignTTL)
	u := s.presign(key, expires)
	t := UploadTarget{URL: u, Method: "PUT", Reference: s.uri(key), Expires: expires}
	if mediaType != "" {
		t.Headers = map[string]string{"Content-Type": mediaType}
	}
	return t, nil
}

func (s *cosStore) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("blob: cos put: %w", err)
	}
	key := contentKey(s.prefix, name, data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	s.authorize(req, key, "put")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("blob: cos put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("blob: cos put: status %d", resp.StatusCode)
	}
	return s.uri(key), nil
}

func (s *cosStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	key, ok := s.keyFromURI(uri)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	s.authorize(req, key, "get")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: cos get: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("blob: cos get: status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// authorize adds the native COS Authorization header, signing host alone.
func (s *cosStore) authorize(req *http.Request, key, method string) {
	keyTime, signature := s.signature(method, key, time.Now().Add(time.Hour))
	auth := fmt.Sprintf(
		"q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=host&q-url-param-list=&q-signature=%s",
		s.secretID, keyTime, keyTime, signature)
	req.Header.Set("Authorization", auth)
	if s.sessionToken != "" {
		req.Header.Set("x-cos-security-token", s.sessionToken)
	}
}

// presign renders a query-signed URL (the q-sign-* components as parameters).
func (s *cosStore) presign(key string, expires time.Time) string {
	keyTime, signature := s.signature("put", key, expires)
	q := url.Values{}
	q.Set("q-sign-algorithm", "sha1")
	q.Set("q-ak", s.secretID)
	q.Set("q-sign-time", keyTime)
	q.Set("q-key-time", keyTime)
	q.Set("q-header-list", "host")
	q.Set("q-url-param-list", "")
	q.Set("q-signature", signature)
	return s.objectURL(key) + "?" + q.Encode()
}

// signature computes the COS sign key, string to sign, and signature.
func (s *cosStore) signature(method, key string, end time.Time) (keyTime, signature string) {
	keyTime = fmt.Sprintf("%d;%d", time.Now().Unix(), end.Unix())
	signKey := hmacSHA1Hex([]byte(s.secretKey), keyTime)
	httpString := method + "\n/" + key + "\n" + "" + "\n" + "host=" + s.endpoint + "\n"
	stringToSign := "sha1\n" + keyTime + "\n" + sha1Hex(httpString) + "\n"
	return keyTime, hmacSHA1Hex([]byte(signKey), stringToSign)
}

func (s *cosStore) objectURL(key string) string {
	return s.scheme + "://" + s.endpoint + "/" + awsEscapePath(key)
}

func (s *cosStore) uri(key string) string  { return "cos://" + s.bucket + "/" + s.region + "/" + key }
func (s *cosStore) key(name string) string { return path.Join(s.prefix, path.Base(name)) }

func (s *cosStore) keyFromURI(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, "cos://"+s.bucket+"/"+s.region+"/")
	return rest, ok
}

func hmacSHA1Hex(key []byte, msg string) string {
	h := hmac.New(sha1.New, key)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
