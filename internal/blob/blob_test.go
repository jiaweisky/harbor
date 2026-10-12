package blob

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

func TestLocalStoreRoundTrip(t *testing.T) {
	st, err := New(config.StorageConfig{Backend: "local", Local: config.LocalStoreConfig{Root: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri, err := st.Put(context.Background(), "report.txt", "text/plain", bytes.NewReader([]byte("hello")), 5)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := st.Get(context.Background(), uri)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello" {
		t.Fatalf("Get = %q, want hello", got)
	}
	if !strings.HasPrefix(uri, "file://") {
		t.Fatalf("uri = %q, want file://", uri)
	}
}

func TestContentAddressedDedup(t *testing.T) {
	st, err := New(config.StorageConfig{Backend: "local", Local: config.LocalStoreConfig{Root: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u1, err := st.Put(context.Background(), "a.txt", "text/plain", strings.NewReader("same"), 4)
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	u2, err := st.Put(context.Background(), "b.txt", "text/plain", strings.NewReader("same"), 4)
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if u1 != u2 {
		t.Fatalf("content-addressed keys differ: %q vs %q", u1, u2)
	}
}

func TestLocalStoreConfinement(t *testing.T) {
	root := t.TempDir()
	st := &localStore{root: root}
	if _, err := st.Get(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("expected a confinement error for a path outside root")
	}
}

func TestHTTPStorePutGet(t *testing.T) {
	var putPath, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			_, _ = io.ReadAll(r.Body)
		case http.MethodGet:
			gotPath = r.URL.Path
			_, _ = w.Write([]byte("body"))
		}
	}))
	defer srv.Close()

	st, err := New(config.StorageConfig{
		Backend: "http",
		Prefix:  "harbor/",
		HTTP:    config.HTTPStoreConfig{PutURL: srv.URL + "/in", GetBase: srv.URL + "/pub"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref, err := st.Put(context.Background(), "x.txt", "text/plain", strings.NewReader("hi"), 2)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(putPath, "/in/harbor/") || !strings.HasSuffix(putPath, ".txt") {
		t.Fatalf("PUT path = %q, want a content-addressed key under /in/harbor/", putPath)
	}
	if want := srv.URL + strings.Replace(putPath, "/in", "/pub", 1); ref != want {
		t.Fatalf("reference = %q, want %q", ref, want)
	}
	rc, err := st.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	if gotPath != strings.Replace(putPath, "/in", "/pub", 1) {
		t.Fatalf("GET path = %q, want the read base", gotPath)
	}
}

func TestStorageDisabled(t *testing.T) {
	st, err := New(config.StorageConfig{})
	if err != nil || st != nil {
		t.Fatalf("New(disabled) = %v, %v; want nil, nil", st, err)
	}
}

func TestLocalStoreReap(t *testing.T) {
	st, err := New(config.StorageConfig{Backend: "local", Local: config.LocalStoreConfig{Root: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri, err := st.Put(context.Background(), "x.txt", "text/plain", strings.NewReader("old"), 3)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	p := strings.TrimPrefix(uri, "file://")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	reaper, ok := st.(Reaper)
	if !ok {
		t.Fatal("the local store should support reaping")
	}
	n, err := reaper.Reap(context.Background(), time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("Reap = %d, %v; want 1, nil", n, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("object not removed: %v", err)
	}
}

func TestHTTPStoreReap(t *testing.T) {
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
		}
	}))
	defer srv.Close()
	st, err := New(config.StorageConfig{Backend: "http", Prefix: "harbor/", HTTP: config.HTTPStoreConfig{PutURL: srv.URL + "/in", GetBase: srv.URL + "/pub"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.Put(context.Background(), "x.txt", "text/plain", strings.NewReader("hi"), 2); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reaper, ok := st.(Reaper)
	if !ok {
		t.Fatal("the http store should support reaping")
	}
	if n, err := reaper.Reap(context.Background(), 0); err != nil || n != 1 {
		t.Fatalf("Reap = %d, %v; want 1, nil", n, err)
	}
	if !strings.HasPrefix(deleted, "/in/harbor/") {
		t.Fatalf("deleted path = %q", deleted)
	}
}

func TestHTTPGetRangeAndSize(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes=2-3/5")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "cd")
	}))
	defer srv.Close()

	st, err := New(config.StorageConfig{
		Backend: "http",
		HTTP:    config.HTTPStoreConfig{PutURL: srv.URL + "/in", GetBase: srv.URL + "/pub"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	uri := srv.URL + "/pub/x.txt"
	if size, err := st.(Sizer).Size(context.Background(), uri); err != nil || size != 5 {
		t.Fatalf("Size = %d, %v; want 5", size, err)
	}
	rc, err := st.(RangeReader).GetRange(context.Background(), uri, 2, 2)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if gotRange != "bytes=2-3" || string(b) != "cd" {
		t.Fatalf("range = %q, body = %q", gotRange, string(b))
	}
}

func TestS3SinglePutContentLength(t *testing.T) {
	gotLen := int64(-1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			gotLen = r.ContentLength
		}
	}))
	defer srv.Close()

	st, err := newS3Store(config.StorageConfig{
		Backend: "s3", Prefix: "harbor/",
		S3: config.S3StoreConfig{
			Endpoint: srv.URL, Bucket: "b", Region: "us-east-1",
			AccessKey: "ak", SecretKey: "sk", PathStyle: true,
		},
	})
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}
	if _, err := st.Put(context.Background(), "a.txt", "text/plain", strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotLen != 5 {
		t.Fatalf("ContentLength = %d, want 5 (a chunked body would be -1)", gotLen)
	}
}

func TestHTTPPutContentLength(t *testing.T) {
	gotLen := int64(-1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
	}))
	defer srv.Close()

	st, err := New(config.StorageConfig{
		Backend: "http",
		HTTP:    config.HTTPStoreConfig{PutURL: srv.URL + "/in", GetBase: srv.URL + "/pub"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.Put(context.Background(), "a.txt", "text/plain", strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotLen != 5 {
		t.Fatalf("ContentLength = %d, want 5 (a chunked body would be -1)", gotLen)
	}
}

func TestHTTPStoreReserveSecureLink(t *testing.T) {
	st, err := New(config.StorageConfig{
		Backend: "http", Prefix: "harbor/",
		HTTP: config.HTTPStoreConfig{
			PutURL: "https://store/in", GetBase: "https://store/pub",
			PresignSecret: "sekret", PresignTTL: 15 * time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tgt, err := st.Reserve(context.Background(), "x.txt", "text/plain", 10)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	u, err := url.Parse(tgt.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Path != "/in/harbor/x.txt" {
		t.Fatalf("path = %q", u.Path)
	}
	expires := u.Query().Get("expires")
	if want := secureLinkMD5("/harbor/x.txt", mustInt(t, expires), "sekret"); u.Query().Get("md5") != want {
		t.Fatalf("md5 = %q, want %q", u.Query().Get("md5"), want)
	}
	if tgt.Reference != "https://store/pub/harbor/x.txt" {
		t.Fatalf("reference = %q", tgt.Reference)
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse int %q: %v", s, err)
	}
	return n
}

func TestMetricsCounters(t *testing.T) {
	st, err := New(config.StorageConfig{Backend: "local", Local: config.LocalStoreConfig{Root: t.TempDir()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.Get(context.Background(), "file:///nope"); err == nil {
		t.Fatal("expected a get error")
	}
	for _, c := range Snapshot() {
		if c.Op == "get" && c.Label == "local" && c.Result == "error" && c.Value >= 1 {
			return
		}
	}
	t.Fatal("expected a get/error counter")
}

func TestAWSEscape(t *testing.T) {
	cases := map[string]string{
		"a b/c": "a%20b/c",
		"a!b":   "a%21b",
		"~-._":  "~-._",
		"a/b":   "a/b",
	}
	for in, want := range cases {
		if got := awsEscapePath(in); got != want {
			t.Errorf("awsEscapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestS3PresignShape(t *testing.T) {
	s := &s3Store{
		scheme: "https", endpoint: "minio.example", bucket: "b", region: "us-east-1",
		accessKey: "AK", secretKey: "SK", presignTTL: 0,
	}
	u, err := s.presign("PUT", "harbor/x.txt", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	for _, want := range []string{"X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Credential=AK", "X-Amz-Signature=", "X-Amz-Expires="} {
		if !strings.Contains(u, want) {
			t.Errorf("presigned URL missing %q: %s", want, u)
		}
	}
}

func TestS3MultipartUpload(t *testing.T) {
	var parts int
	var initiated, completed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			initiated = true
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>abc</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			parts++
			w.Header().Set("ETag", `"etag-`+q.Get("partNumber")+`"`)
		case r.Method == http.MethodPost && q.Has("uploadId"):
			completed = true
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()

	st, err := newS3Store(config.StorageConfig{
		Backend: "s3", Prefix: "harbor/",
		S3: config.S3StoreConfig{
			Endpoint: srv.URL, Bucket: "b", Region: "us-east-1",
			AccessKey: "ak", SecretKey: "sk", PathStyle: true,
		},
	})
	if err != nil {
		t.Fatalf("newS3Store: %v", err)
	}
	st.(*s3Store).partSize = 8
	data := strings.Repeat("x", 20)
	if _, err := st.Put(context.Background(), "big.bin", "application/octet-stream", strings.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !initiated || !completed {
		t.Fatalf("initiated=%v completed=%v", initiated, completed)
	}
	if parts != 3 {
		t.Fatalf("parts = %d, want 3", parts)
	}
}

func TestCOSMultipartUpload(t *testing.T) {
	var parts int
	var initiated, completed bool
	var lastParamList string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			initiated = true
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>abc</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			parts++
			auth := r.Header.Get("Authorization")
			if i := strings.Index(auth, "q-url-param-list="); i >= 0 {
				lastParamList = auth[i+len("q-url-param-list="):]
				if j := strings.Index(lastParamList, "&"); j >= 0 {
					lastParamList = lastParamList[:j]
				}
			}
			w.Header().Set("ETag", `"etag-`+q.Get("partNumber")+`"`)
		case r.Method == http.MethodPost && q.Has("uploadId"):
			completed = true
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()

	st, err := newCOSStore(config.StorageConfig{
		Backend: "cos", Prefix: "harbor/",
		COS: config.COSStoreConfig{
			Endpoint: srv.URL, Bucket: "b", Region: "ap-shanghai",
			SecretID: "id", SecretKey: "key",
		},
	})
	if err != nil {
		t.Fatalf("newCOSStore: %v", err)
	}
	st.(*cosStore).partSize = 8
	data := strings.Repeat("x", 20)
	if _, err := st.Put(context.Background(), "big.bin", "application/octet-stream", strings.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !initiated || !completed {
		t.Fatalf("initiated=%v completed=%v", initiated, completed)
	}
	if parts != 3 {
		t.Fatalf("parts = %d, want 3", parts)
	}
	if lastParamList != "partnumber;uploadid" {
		t.Fatalf("last q-url-param-list = %q, want partnumber;uploadid", lastParamList)
	}
}

func TestSigV4SigningKey(t *testing.T) {
	// AWS SigV4 reference vector: iam, us-east-1, 2015-08-30.
	tm := time.Date(2015, 8, 30, 0, 0, 0, 0, time.UTC)
	got := hex.EncodeToString(sigV4SigningKey(
		"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "iam", tm))
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Fatalf("signing key = %s, want %s", got, want)
	}
}

func TestCOSAuthorizationShape(t *testing.T) {
	s := &cosStore{
		scheme: "https", endpoint: "b.cos.r.myqcloud.com", bucket: "b", region: "r",
		secretID: "AK", secretKey: "SK", presignTTL: time.Minute,
	}
	req, err := http.NewRequest(http.MethodGet, s.objectURL("harbor/x.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.authorize(req, "harbor/x.txt", "get")
	auth := req.Header.Get("Authorization")
	for _, want := range []string{"q-sign-algorithm=sha1", "q-ak=AK", "q-header-list=host", "q-signature="} {
		if !strings.Contains(auth, want) {
			t.Errorf("authorization missing %q: %s", want, auth)
		}
	}
	u := s.presign("harbor/x.txt", time.Now().Add(time.Minute))
	if !strings.Contains(u, "q-sign-algorithm=sha1") || !strings.Contains(u, "q-signature=") {
		t.Errorf("presigned URL missing q-sign params: %s", u)
	}
}
