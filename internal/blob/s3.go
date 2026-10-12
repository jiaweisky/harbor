package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	partSize     int
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
		prefix: cfg.Prefix, presignTTL: cfg.S3.PresignTTL, partSize: s3PartSize, client: client,
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

// s3PartSize is the multipart chunk size. Objects larger than one part use the
// S3 multipart upload API, so the single-PUT size cap does not apply.
const s3PartSize = 16 << 20

func (s *s3Store) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	f, n, sum, err := spool(r)
	if err != nil {
		return "", fmt.Errorf("blob: s3 put: %w", err)
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	key := contentKeySum(s.prefix, name, sum)
	if n <= int64(s.partSize) {
		if err := s.putSingle(ctx, key, mediaType, f, n); err != nil {
			return "", err
		}
		return s.uri(key), nil
	}
	if err := s.putMultipart(ctx, key, mediaType, f, n); err != nil {
		return "", err
	}
	return s.uri(key), nil
}

// putSingle sends one PUT for the whole object.
func (s *s3Store) putSingle(ctx context.Context, key, mediaType string, body io.Reader, size int64) error {
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(key), body, mediaType, size)
	if err != nil {
		return fmt.Errorf("blob: s3 put: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return checkStatus(resp, "s3 put")
}

// s3Multipart adapts s3Store to the multipart driver for one object.
type s3Multipart struct {
	store     *s3Store
	key       string
	mediaType string
}

func (m s3Multipart) begin(ctx context.Context) (string, error) {
	return m.store.createMultipart(ctx, m.key, m.mediaType)
}
func (m s3Multipart) putPart(ctx context.Context, id string, n int, data []byte) (string, error) {
	return m.store.uploadPart(ctx, m.key, id, n, data)
}
func (m s3Multipart) complete(ctx context.Context, id string, parts []part) error {
	return m.store.completeMultipart(ctx, m.key, id, parts)
}
func (m s3Multipart) abort(ctx context.Context, id string) { m.store.abortMultipart(ctx, m.key, id) }

// putMultipart streams size bytes from f as parts under one S3 multipart upload.
func (s *s3Store) putMultipart(ctx context.Context, key, mediaType string, f io.Reader, size int64) error {
	return uploadMultipart(ctx, s3Multipart{store: s, key: key, mediaType: mediaType}, s.partSize, size, f)
}

func (s *s3Store) createMultipart(ctx context.Context, key, mediaType string) (string, error) {
	resp, err := s.do(ctx, http.MethodPost, s.objectURL(key)+"?uploads", nil, mediaType, 0)
	if err != nil {
		return "", fmt.Errorf("blob: s3 create multipart: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, "s3 create multipart"); err != nil {
		return "", err
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil || out.UploadID == "" {
		return "", fmt.Errorf("blob: s3 create multipart: bad response")
	}
	return out.UploadID, nil
}

func (s *s3Store) uploadPart(ctx context.Context, key, uploadID string, n int, data []byte) (string, error) {
	uri := fmt.Sprintf("%s?partNumber=%d&uploadId=%s", s.objectURL(key), n, url.QueryEscape(uploadID))
	resp, err := s.do(ctx, http.MethodPut, uri, bytes.NewReader(data), "", 0)
	if err != nil {
		return "", fmt.Errorf("blob: s3 upload part: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := checkStatus(resp, "s3 upload part"); err != nil {
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

func (s *s3Store) completeMultipart(ctx context.Context, key, uploadID string, parts []part) error {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%q</ETag></Part>", p.number, p.etag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	uri := s.objectURL(key) + "?uploadId=" + url.QueryEscape(uploadID)
	resp, err := s.do(ctx, http.MethodPost, uri, strings.NewReader(b.String()), "application/xml", 0)
	if err != nil {
		return fmt.Errorf("blob: s3 complete multipart: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return checkStatus(resp, "s3 complete multipart")
}

// abortMultipart discards an in-progress upload; its failure is not fatal.
func (s *s3Store) abortMultipart(ctx context.Context, key, uploadID string) {
	uri := s.objectURL(key) + "?uploadId=" + url.QueryEscape(uploadID)
	if resp, err := s.do(ctx, http.MethodDelete, uri, nil, "", 0); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (s *s3Store) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	return s.fetch(ctx, uri, "", 0)
}

// GetRange reads length bytes from offset, using an HTTP Range request.
func (s *s3Store) GetRange(ctx context.Context, uri string, offset, length int64) (io.ReadCloser, error) {
	return s.fetch(ctx, uri, rangeSpec(offset, length), offset)
}

// fetch issues a signed GET with an optional Range header. A 200 reply to a
// non-zero range offset means the range was ignored.
func (s *s3Store) fetch(ctx context.Context, uri, rangeHeader string, offset int64) (io.ReadCloser, error) {
	key, ok := s.keyFromURI(uri)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	req, err := s.request(ctx, http.MethodGet, key, nil, "")
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 get: %w", err)
	}
	if err := objectError(resp, offset, "s3 get"); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Size reports the object's size via a HEAD request.
func (s *s3Store) Size(ctx context.Context, uri string) (int64, error) {
	key, ok := s.keyFromURI(uri)
	if !ok {
		return 0, ErrUnsupportedURI
	}
	req, err := s.request(ctx, http.MethodHead, key, nil, "")
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("blob: s3 head: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := objectError(resp, 0, "s3 head"); err != nil {
		return 0, err
	}
	return resp.ContentLength, nil
}

// Reference maps a key to its s3:// reference URI.
func (s *s3Store) Reference(key string) (string, bool) { return s.uri(key), true }

// request builds a SigV4-signed request, using UNSIGNED-PAYLOAD so the body can
// stream.
func (s *s3Store) request(ctx context.Context, method, key string, body io.Reader, mediaType string) (*http.Request, error) {
	return s.build(ctx, method, s.objectURL(key), body, mediaType)
}

// build creates and signs a request against an arbitrary object URL, which may
// carry query parameters (as the multipart upload calls do).
func (s *s3Store) build(ctx context.Context, method, uri string, body io.Reader, mediaType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, uri, body)
	if err != nil {
		return nil, err
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	s.sign(req, time.Now().UTC())
	return req, nil
}

// do signs and sends a request against an arbitrary object URL. A positive size
// declares the body length, so the body is not sent chunked (which object stores
// reject for a file body).
func (s *s3Store) do(ctx context.Context, method, uri string, body io.Reader, mediaType string, size int64) (*http.Response, error) {
	req, err := s.build(ctx, method, uri, body, mediaType)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		req.ContentLength = size
	}
	return s.client.Do(req)
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
	return sigV4SigningKey(s.secretKey, s.region, "s3", t)
}

// sigV4SigningKey derives the SigV4 signing key for a secret, region, service,
// and date.
func sigV4SigningKey(secret, region, service string, t time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), t.Format("20060102"))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
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
