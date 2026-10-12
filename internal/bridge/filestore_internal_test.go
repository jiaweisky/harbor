package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/blob"
)

// fakeStore serves fixed bytes for known reference URIs.
type fakeStore struct {
	data   map[string]string
	target blob.UploadTarget
}

func (s fakeStore) Get(_ context.Context, uri string) (io.ReadCloser, error) {
	v, ok := s.data[uri]
	if !ok {
		return nil, blob.ErrUnsupportedURI
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

func (fakeStore) Target() blob.UploadTarget { return blob.UploadTarget{} }
func (s fakeStore) Reserve(context.Context, string, string, int64) (blob.UploadTarget, error) {
	if s.target.URL == "" {
		return blob.UploadTarget{}, blob.ErrUnsupportedURI
	}
	return s.target, nil
}
func (fakeStore) Put(context.Context, string, string, io.Reader, int64) (string, error) {
	return "", nil
}

// stubLauncher exposes capabilities only.
type stubLauncher struct{ caps agent.Capabilities }

func (stubLauncher) Name() string                       { return "stub" }
func (l stubLauncher) Capabilities() agent.Capabilities { return l.caps }
func (stubLauncher) Probe(context.Context) agent.Info   { return agent.Info{} }
func (stubLauncher) Auth() agent.Authenticator          { return nil }
func (stubLauncher) AuthReady(context.Context) bool     { return true }
func (stubLauncher) Start(context.Context, *agent.Request) (agent.Session, error) {
	return nil, nil
}

func TestPromptReadsReference(t *testing.T) {
	dir := t.TempDir()
	e := &CLIExecutor{
		launcher: stubLauncher{caps: agent.Capabilities{InputText: true, InputFile: true, AttachFiles: true}},
		store:    fakeStore{data: map[string]string{"s3://b/x.txt": "the codeword is ORANGE"}},
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("see attached"),
		a2a.NewFileURLPart(a2a.URL("s3://b/x.txt"), "text/plain"))

	in, err := e.promptFromParts(context.Background(), msg, dir)
	if err != nil {
		t.Fatalf("promptFromParts: %v", err)
	}
	if len(in.Files) != 1 {
		t.Fatalf("files = %v, want one materialized path", in.Files)
	}
	got, err := os.ReadFile(in.Files[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "the codeword is ORANGE" {
		t.Fatalf("materialized content = %q", got)
	}
}

func TestPromptUnreadableReference(t *testing.T) {
	e := &CLIExecutor{
		launcher: stubLauncher{caps: agent.Capabilities{InputText: true, InputFile: true}},
		store:    fakeStore{},
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewFileURLPart(a2a.URL("s3://b/missing"), "text/plain"))
	if _, err := e.promptFromParts(context.Background(), msg, t.TempDir()); err == nil {
		t.Fatal("expected an error for an unreadable reference")
	}
}

func TestUploadArtifact(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
	}))
	defer srv.Close()

	e := &CLIExecutor{inlineMax: 1}
	ec := &a2asrv.ExecutorContext{Message: &a2a.Message{Metadata: map[string]any{
		"harbor.ingest": map[string]any{"base": srv.URL, "method": "PUT"},
	}}}
	ref := e.uploadArtifact(ec, "out.txt", "text/plain", []byte("large payload"))
	if ref == "" {
		t.Fatal("expected an upload")
	}
	if !strings.HasPrefix(ref, srv.URL) {
		t.Fatalf("reference = %q, want a URL under %s", ref, srv.URL)
	}
	if received != "large payload" {
		t.Fatalf("server received %q", received)
	}
}

func TestUploadArtifactInlinesWithoutTarget(t *testing.T) {
	e := &CLIExecutor{inlineMax: 1}
	ec := &a2asrv.ExecutorContext{Message: &a2a.Message{}}
	if ref := e.uploadArtifact(ec, "out.txt", "text/plain", []byte("data")); ref != "" {
		t.Fatal("expected no upload without a consumer target")
	}
}

func TestFileArtifactMetadata(t *testing.T) {
	e := &CLIExecutor{}
	evt := e.fileArtifact(&a2asrv.ExecutorContext{}, "x.txt", "file", "text/plain", []byte("hello"))
	meta := evt.Artifact.Parts[0].Metadata
	if meta["size"] != 5 {
		t.Fatalf("size = %v", meta["size"])
	}
	if sum, _ := meta["sha256"].(string); len(sum) != 64 {
		t.Fatalf("sha256 = %v", meta["sha256"])
	}
}

func TestMaterializeReferenceIntegrity(t *testing.T) {
	e := &CLIExecutor{store: fakeStore{data: map[string]string{"s3://b/x": "hello"}}}
	dir := t.TempDir()
	if _, err := e.materializeReference(context.Background(), "s3://b/x", "deadbeef", dir, "x.txt"); err == nil {
		t.Fatal("expected a sha256 mismatch error")
	}
	path, err := e.materializeReference(context.Background(), "s3://b/x", "", dir, "x.txt")
	if err != nil {
		t.Fatalf("materializeReference: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("content = %q", got)
	}
}

// recordingStore records the last Put.
type recordingStore struct {
	fakeStore
	name, body, ref string
}

func (s *recordingStore) Put(_ context.Context, name, _ string, r io.Reader, _ int64) (string, error) {
	b, _ := io.ReadAll(r)
	s.name, s.body = name, string(b)
	return s.ref, nil
}

func TestIngestHandler(t *testing.T) {
	st := &recordingStore{ref: "s3://b/x.txt"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/agents/a/storage/in/x.txt", strings.NewReader("payload"))
	IngestHandler(st).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("X-Harbor-Reference") != "s3://b/x.txt" {
		t.Fatalf("reference header = %q", rec.Header().Get("X-Harbor-Reference"))
	}
	if st.name != "x.txt" || st.body != "payload" {
		t.Fatalf("stored name=%q body=%q", st.name, st.body)
	}
}

func TestInboundMediaSpoolVsInline(t *testing.T) {
	img := a2a.NewRawPart([]byte("PNG"))
	img.MediaType = "image/png"
	msg := a2a.NewMessage(a2a.MessageRoleUser, img)

	inline := &CLIExecutor{
		launcher:     stubLauncher{caps: agent.Capabilities{InputText: true, InputImage: []string{"image/png"}, InlineImages: true}},
		inboundMedia: "inline",
	}
	in, err := inline.promptFromParts(context.Background(), msg, t.TempDir())
	if err != nil {
		t.Fatalf("inline: %v", err)
	}
	if len(in.Images) != 1 {
		t.Fatalf("inline should project a native image; images = %v", in.Images)
	}

	spool := &CLIExecutor{
		launcher: stubLauncher{caps: agent.Capabilities{
			InputText: true, InputImage: []string{"image/png"}, InputFile: true, AttachImages: true,
		}},
		inboundMedia: "spool",
	}
	in, err = spool.promptFromParts(context.Background(), msg, t.TempDir())
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	if len(in.Images) != 0 {
		t.Fatalf("spool should not inline: %v", in.Images)
	}
	if len(in.ImageFiles) != 1 {
		t.Fatalf("spool should attach the image by path; imageFiles = %v", in.ImageFiles)
	}
}

func TestBrokerReserve(t *testing.T) {
	e := &CLIExecutor{
		store:      fakeStore{target: blob.UploadTarget{URL: "https://store/presigned", Method: "PUT", Reference: "s3://b/k"}},
		maxBytes:   100,
		mediaTypes: []string{"text/*"},
	}
	ec := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"kind": kindUploadSlotRequest, "name": "x.txt", "mediaType": "text/plain", "size": float64(10),
	}))}

	var slot map[string]any
	handled := e.handleStorageRequest(context.Background(), ec, func(ev a2a.Event, _ error) bool {
		if u, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			if d, ok := u.Artifact.Parts[0].Data().(map[string]any); ok {
				slot = d
			}
		}
		return true
	})
	if !handled {
		t.Fatal("expected the storage request to be handled")
	}
	if slot == nil || slot["reference"] != "s3://b/k" || slot["uploadUrl"] != "https://store/presigned" {
		t.Fatalf("slot = %v", slot)
	}
}

func TestBrokerReserveRejectsOversize(t *testing.T) {
	e := &CLIExecutor{
		store:      fakeStore{target: blob.UploadTarget{URL: "https://store/x"}},
		maxBytes:   5,
		mediaTypes: []string{"text/*"},
	}
	ec := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"kind": kindUploadSlotRequest, "name": "x.txt", "mediaType": "text/plain", "size": float64(10),
	}))}
	var failed bool
	e.handleStorageRequest(context.Background(), ec, func(ev a2a.Event, _ error) bool {
		if u, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && u.Status.State == a2a.TaskStateFailed {
			failed = true
		}
		return true
	})
	if !failed {
		t.Fatal("expected an oversize request to fail")
	}
}
