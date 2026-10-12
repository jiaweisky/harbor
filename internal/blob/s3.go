package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// s3Store is an S3-compatible store (AWS S3, OSS, MinIO, COS via SigV4). It
// signs with AWS Signature Version 4 and needs no SDK.
type s3Store struct {
	scheme       string
	endpoint     string
	bucket       string
	region       string
	accessKey    string
	secretKey    string
	sessionToken string
	pathStyle    bool
	prefix       string
	presignTTL   time.Duration
	client       *http.Client
}

func newS3Store(cfg config.StorageConfig) (Store, error) {
	client, err := newHTTPClient(cfg.S3.TLS)
	if err != nil {
		return nil, err
	}
	scheme, endpoint := "https", cfg.S3.Endpoint
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		scheme, endpoint = u.Scheme, u.Host
	}
	return &s3Store{
		scheme: scheme, endpoint: strings.TrimRight(endpoint, "/"), bucket: cfg.S3.Bucket,
		region: cfg.S3.Region, accessKey: cfg.S3.AccessKey, secretKey: cfg.S3.SecretKey,
		sessionToken: cfg.S3.SessionToken, pathStyle: cfg.S3.PathStyle,
		prefix: cfg.Prefix, presignTTL: cfg.S3.PresignTTL, client: client,
	}, nil
}

// Target returns no static target: an S3 peer is reached through broker
// presigning, which carries its own credential.
func (s *s3Store) Target() UploadTarget { return UploadTarget{} }

// Reserve returns a presigned PUT and the s3:// reference for it.
func (s *s3Store) Reserve(_ context.Context, name, mediaType string, _ int64) (UploadTarget, error) {
	key := s.key(name)
	expires := time.Now().Add(s.presignTTL)
	u, err := s.presign(http.MethodPut, key, expires)
	if err != nil {
		return UploadTarget{}, err
	}
	t := UploadTarget{URL: u, Method: "PUT", Reference: s.uri(key), Expires: expires}
	if mediaType != "" {
		t.Headers = map[string]string{"Content-Type": mediaType}
	}
	return t, nil
}

func (s *s3Store) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("blob: s3 put: %w", err)
	}
	key := contentKey(s.prefix, name, data)
	req, err := s.request(ctx, http.MethodPut, key, bytes.NewReader(data), mediaType)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("blob: s3 put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("blob: s3 put: status %d", resp.StatusCode)
	}
	return s.uri(key), nil
}

func (s *s3Store) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	key, ok := s.keyFromURI(uri)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	req, err := s.request(ctx, http.MethodGet, key, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 get: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("blob: s3 get: status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// request builds a SigV4-signed request, using UNSIGNED-PAYLOAD so the body can
// stream.
func (s *s3Store) request(ctx context.Context, method, key string, body io.Reader, mediaType string) (*http.Request, error) {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	s.sign(req, time.Now().UTC())
	return req, nil
}

// presign builds a query-signed URL for method/key.
func (s *s3Store) presign(method, key string, expires time.Time) (string, error) {
	u, err := url.Parse(s.objectURL(key))
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	scope := now.Format("20060102") + "/" + s.region + "/s3/aws4_request"
	q := u.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", s.accessKey+"/"+scope)
	q.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int64(expires.Sub(now).Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	if s.sessionToken != "" {
		q.Set("X-Amz-Security-Token", s.sessionToken)
	}
	u.RawQuery = q.Encode()

	canonical := method + "\n" + u.EscapedPath() + "\n" + canonicalQuery(q) + "\n" +
		"host:" + u.Host + "\n\nhost\nUNSIGNED-PAYLOAD"
	sts := "AWS4-HMAC-SHA256\n" + now.Format("20060102T150405Z") + "\n" + scope + "\n" + hexSHA256(canonical)
	q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(s.signingKey(now), sts)))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// sign adds the SigV4 Authorization header for a header-signed request.
func (s *s3Store) sign(req *http.Request, now time.Time) {
	const payload = "UNSIGNED-PAYLOAD"
	req.Header.Set("x-amz-content-sha256", payload)
	req.Header.Set("x-amz-date", now.Format("20060102T150405Z"))
	if s.sessionToken != "" {
		req.Header.Set("x-amz-security-token", s.sessionToken)
	}
	signed := "host;x-amz-content-sha256;x-amz-date"
	if s.sessionToken != "" {
		signed = "host;x-amz-content-sha256;x-amz-date;x-amz-security-token"
	}
	var headers strings.Builder
	headers.WriteString("host:" + req.URL.Host + "\n")
	headers.WriteString("x-amz-content-sha256:" + payload + "\n")
	headers.WriteString("x-amz-date:" + req.Header.Get("x-amz-date") + "\n")
	if s.sessionToken != "" {
		headers.WriteString("x-amz-security-token:" + s.sessionToken + "\n")
	}
	scope := now.Format("20060102") + "/" + s.region + "/s3/aws4_request"
	canonical := req.Method + "\n" + req.URL.EscapedPath() + "\n" + canonicalQuery(req.URL.Query()) + "\n" +
		headers.String() + "\n" + signed + "\n" + payload
	sts := "AWS4-HMAC-SHA256\n" + now.Format("20060102T150405Z") + "\n" + scope + "\n" + hexSHA256(canonical)
	sig := hex.EncodeToString(hmacSHA256(s.signingKey(now), sts))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", s.accessKey, scope, signed, sig))
}

func (s *s3Store) signingKey(t time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+s.secretKey), t.Format("20060102"))
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

// objectURL renders the object's HTTP URL (virtual-hosted unless path_style).
func (s *s3Store) objectURL(key string) string {
	if s.pathStyle {
		return s.scheme + "://" + s.endpoint + "/" + s.bucket + "/" + awsEscapePath(key)
	}
	return s.scheme + "://" + s.bucket + "." + s.endpoint + "/" + awsEscapePath(key)
}

func (s *s3Store) uri(key string) string  { return "s3://" + s.bucket + "/" + key }
func (s *s3Store) key(name string) string { return path.Join(s.prefix, path.Base(name)) }

func (s *s3Store) keyFromURI(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, "s3://"+s.bucket+"/")
	return rest, ok
}

// canonicalQuery renders the canonical query string (sorted, encoded).
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscapePath encodes each path segment, leaving "/" intact.
func awsEscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

// awsEscape applies AWS UriEncode's rules for one component.
func awsEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func hexSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
