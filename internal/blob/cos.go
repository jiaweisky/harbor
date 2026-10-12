package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
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
	partSize     int
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
		prefix: cfg.Prefix, presignTTL: cfg.COS.PresignTTL, partSize: cosPartSize, client: client,
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

// cosPartSize is the multipart chunk size. Objects larger than one part use the
// COS multipart upload API, so the single-PUT size cap does not apply.
const cosPartSize = 16 << 20

func (s *cosStore) Put(ctx context.Context, name, mediaType string, r io.Reader, size int64) (string, error) {
	f, n, sum, err := spool(r)
	if err != nil {
		return "", fmt.Errorf("blob: cos put: %w", err)
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
func (s *cosStore) putSingle(ctx context.Context, key, mediaType string, body io.Reader, size int64) error {
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(key), body, mediaType, key, size)
	if err != nil {
		return fmt.Errorf("blob: cos put: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return checkStatus(resp, "cos put")
}

// cosMultipart adapts cosStore to the multipart driver for one object.
type cosMultipart struct {
	store     *cosStore
	key       string
	mediaType string
}

func (m cosMultipart) begin(ctx context.Context) (string, error) {
	return m.store.createMultipart(ctx, m.key, m.mediaType)
}
func (m cosMultipart) putPart(ctx context.Context, id string, n int, data []byte) (string, error) {
	return m.store.uploadPart(ctx, m.key, id, n, data)
}
func (m cosMultipart) complete(ctx context.Context, id string, parts []part) error {
	return m.store.completeMultipart(ctx, m.key, id, parts)
}
func (m cosMultipart) abort(ctx context.Context, id string) { m.store.abortMultipart(ctx, m.key, id) }

// putMultipart streams size bytes from f as parts under one COS multipart upload.
func (s *cosStore) putMultipart(ctx context.Context, key, mediaType string, f io.Reader, size int64) error {
	return uploadMultipart(ctx, cosMultipart{store: s, key: key, mediaType: mediaType}, s.partSize, size, f)
}

func (s *cosStore) createMultipart(ctx context.Context, key, mediaType string) (string, error) {
	resp, err := s.do(ctx, http.MethodPost, s.objectURL(key)+"?uploads", nil, mediaType, key, 0)
	if err != nil {
		return "", fmt.Errorf("blob: cos create multipart: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp, "cos create multipart"); err != nil {
		return "", err
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil || out.UploadID == "" {
		return "", fmt.Errorf("blob: cos create multipart: bad response")
	}
	return out.UploadID, nil
}

func (s *cosStore) uploadPart(ctx context.Context, key, uploadID string, n int, data []byte) (string, error) {
	uri := fmt.Sprintf("%s?partNumber=%d&uploadId=%s", s.objectURL(key), n, url.QueryEscape(uploadID))
	resp, err := s.do(ctx, http.MethodPut, uri, bytes.NewReader(data), "", key, 0)
	if err != nil {
		return "", fmt.Errorf("blob: cos upload part: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := checkStatus(resp, "cos upload part"); err != nil {
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

func (s *cosStore) completeMultipart(ctx context.Context, key, uploadID string, parts []part) error {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%q</ETag></Part>", p.number, p.etag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	uri := s.objectURL(key) + "?uploadId=" + url.QueryEscape(uploadID)
	resp, err := s.do(ctx, http.MethodPost, uri, strings.NewReader(b.String()), "application/xml", key, 0)
	if err != nil {
		return fmt.Errorf("blob: cos complete multipart: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return checkStatus(resp, "cos complete multipart")
}

// abortMultipart discards an in-progress upload; its failure is not fatal.
func (s *cosStore) abortMultipart(ctx context.Context, key, uploadID string) {
	uri := s.objectURL(key) + "?uploadId=" + url.QueryEscape(uploadID)
	if resp, err := s.do(ctx, http.MethodDelete, uri, nil, "", key, 0); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// do signs and sends a COS request against a URI, deriving the signature's
// query parameters from the URI itself. A positive size declares the body
// length, so a file body is not sent chunked.
func (s *cosStore) do(ctx context.Context, method, uri string, body io.Reader, mediaType, key string, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, uri, body)
	if err != nil {
		return nil, err
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	s.authorize(req, key, method)
	if size > 0 {
		req.ContentLength = size
	}
	return s.client.Do(req)
}

func (s *cosStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	return s.fetch(ctx, uri, "", 0)
}

// GetRange reads length bytes from offset, using an HTTP Range request.
func (s *cosStore) GetRange(ctx context.Context, uri string, offset, length int64) (io.ReadCloser, error) {
	return s.fetch(ctx, uri, rangeSpec(offset, length), offset)
}

// fetch issues a signed GET with an optional Range header. A 200 reply to a
// non-zero range offset means the range was ignored.
func (s *cosStore) fetch(ctx context.Context, uri, rangeHeader string, offset int64) (io.ReadCloser, error) {
	key, ok := s.keyFromURI(uri)
	if !ok {
		return nil, ErrUnsupportedURI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	s.authorize(req, key, "get")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: cos get: %w", err)
	}
	if err := objectError(resp, offset, "cos get"); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Size reports the object's size via a HEAD request.
func (s *cosStore) Size(ctx context.Context, uri string) (int64, error) {
	key, ok := s.keyFromURI(uri)
	if !ok {
		return 0, ErrUnsupportedURI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.objectURL(key), nil)
	if err != nil {
		return 0, err
	}
	s.authorize(req, key, "head")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("blob: cos head: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := objectError(resp, 0, "cos head"); err != nil {
		return 0, err
	}
	return resp.ContentLength, nil
}

// Reference maps a key to its cos:// reference URI.
func (s *cosStore) Reference(key string) (string, bool) { return s.uri(key), true }

// authorize adds the native COS Authorization header, signing host and any
// request query parameters.
func (s *cosStore) authorize(req *http.Request, key, method string) {
	keyTime, signature, paramList := s.signature(method, key, req.URL.Query(), time.Now().Add(time.Hour))
	auth := fmt.Sprintf(
		"q-sign-algorithm=sha1&q-ak=%s&q-sign-time=%s&q-key-time=%s&q-header-list=host&q-url-param-list=%s&q-signature=%s",
		s.secretID, keyTime, keyTime, paramList, signature)
	req.Header.Set("Authorization", auth)
	if s.sessionToken != "" {
		req.Header.Set("x-cos-security-token", s.sessionToken)
	}
}

// presign renders a query-signed URL (the q-sign-* components as parameters).
func (s *cosStore) presign(key string, expires time.Time) string {
	keyTime, signature, _ := s.signature("put", key, nil, expires)
	q := url.Values{}
	q.Set("q-sign-algorithm", "sha1")
	q.Set("q-ak", s.secretID)
	q.Set("q-sign-time", keyTime)
	q.Set("q-key-time", keyTime)
	q.Set("q-header-list", "host")
	q.Set("q-url-param-list", "")
	q.Set("q-signature", signature)
	if s.sessionToken != "" {
		// Temporary credentials carry the session token as a query parameter; it
		// is not part of the signed parameters.
		q.Set("x-cos-security-token", s.sessionToken)
	}
	return s.objectURL(key) + "?" + q.Encode()
}

// signature computes the COS sign key, string to sign, and signature over the
// signed headers (host) and the given request query parameters. It returns the
// key time, the signature, and the signed URL parameter list.
func (s *cosStore) signature(method, key string, params url.Values, end time.Time) (keyTime, signature, paramList string) {
	paramString, paramList := cosParameters(params)
	keyTime = fmt.Sprintf("%d;%d", time.Now().Unix(), end.Unix())
	signKey := hmacSHA1Hex([]byte(s.secretKey), keyTime)
	httpString := method + "\n/" + key + "\n" + paramString + "\n" + "host=" + s.endpoint + "\n"
	stringToSign := "sha1\n" + keyTime + "\n" + sha1Hex(httpString) + "\n"
	return keyTime, hmacSHA1Hex([]byte(signKey), stringToSign), paramList
}

// cosParameters renders a query as COS's HttpParameters string (lowercase keys,
// sorted, joined by &) and the matching q-url-param-list (semicolon-joined).
// The q-sign-* components and the session token are not part of the signature.
func cosParameters(q url.Values) (params, list string) {
	lower := make(map[string][]string, len(q))
	for k, vs := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "q-") || lk == "x-cos-security-token" {
			continue
		}
		lower[lk] = append(lower[lk], vs...)
	}
	keys := make([]string, 0, len(lower))
	for k := range lower {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range lower[k] {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "&"), strings.Join(keys, ";")
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
