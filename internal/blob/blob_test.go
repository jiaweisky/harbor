package blob

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
