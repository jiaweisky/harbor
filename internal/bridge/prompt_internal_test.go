package bridge

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
)

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

func TestPromptPassesURLAsText(t *testing.T) {
	e := &CLIExecutor{launcher: stubLauncher{caps: agent.Capabilities{InputText: true}}}
	link := a2a.NewFileURLPart(a2a.URL("s3://bucket/spec.bin"), "application/octet-stream")
	link.Filename = "spec.bin"
	link.Metadata = map[string]any{"sha256": "deadbeef"}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("see"), link)

	in, err := e.promptFromParts(msg, t.TempDir())
	if err != nil {
		t.Fatalf("promptFromParts: %v", err)
	}
	if len(in.Files) != 0 || len(in.Images) != 0 || len(in.ImageFiles) != 0 {
		t.Fatalf("URL part must not be materialized: %+v", in)
	}
	got := strings.Join(in.Blocks, "\n")
	for _, want := range []string{"spec.bin", "application/octet-stream", "s3://bucket/spec.bin", "deadbeef"} {
		if !strings.Contains(got, want) {
			t.Fatalf("block missing %q: %q", want, got)
		}
	}
}

func TestPromptMaterializesRawFile(t *testing.T) {
	e := &CLIExecutor{launcher: stubLauncher{caps: agent.Capabilities{
		InputText: true, InputFile: true, AttachFiles: true,
	}}}
	file := a2a.NewRawPart([]byte("file body"))
	file.Filename = "notes.txt"
	file.MediaType = "text/plain"
	msg := a2a.NewMessage(a2a.MessageRoleUser, file)

	in, err := e.promptFromParts(msg, t.TempDir())
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
	if string(got) != "file body" {
		t.Fatalf("content = %q", got)
	}
}

func TestPromptRawImageInlineOrAttach(t *testing.T) {
	img := a2a.NewRawPart([]byte("PNG"))
	img.MediaType = "image/png"
	msg := a2a.NewMessage(a2a.MessageRoleUser, img)

	inline := &CLIExecutor{launcher: stubLauncher{caps: agent.Capabilities{
		InputText: true, InputImage: []string{"image/png"}, InlineImages: true,
	}}}
	in, err := inline.promptFromParts(msg, t.TempDir())
	if err != nil {
		t.Fatalf("inline: %v", err)
	}
	if len(in.Images) != 1 {
		t.Fatalf("inline should project a native image; images = %v", in.Images)
	}

	attach := &CLIExecutor{launcher: stubLauncher{caps: agent.Capabilities{
		InputText: true, InputImage: []string{"image/png"}, InputFile: true, AttachImages: true,
	}}}
	in, err = attach.promptFromParts(msg, t.TempDir())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(in.Images) != 0 || len(in.ImageFiles) != 1 {
		t.Fatalf("attach should use a file path; images=%v imageFiles=%v", in.Images, in.ImageFiles)
	}
}

func TestPromptUnsupportedRawImageRejected(t *testing.T) {
	e := &CLIExecutor{launcher: stubLauncher{caps: agent.Capabilities{InputText: true}}}
	img := a2a.NewRawPart([]byte("PNG"))
	img.MediaType = "image/png"
	if _, err := e.promptFromParts(a2a.NewMessage(a2a.MessageRoleUser, img), t.TempDir()); err == nil {
		t.Fatal("expected an unsupported image error")
	}
}

func TestPromptURLBypassesCapabilityChecks(t *testing.T) {
	e := &CLIExecutor{launcher: stubLauncher{caps: agent.Capabilities{InputText: true}}}
	link := a2a.NewFileURLPart(a2a.URL("https://example.test/x.pdf"), "application/pdf")
	in, err := e.promptFromParts(a2a.NewMessage(a2a.MessageRoleUser, link), t.TempDir())
	if err != nil {
		t.Fatalf("URL parts must not be validated: %v", err)
	}
	if !strings.Contains(strings.Join(in.Blocks, "\n"), "https://example.test/x.pdf") {
		t.Fatalf("URL missing from blocks: %v", in.Blocks)
	}
}

