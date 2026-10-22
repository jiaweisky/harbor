package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/changes"
)

func TestEmitChangedFileOmitsOversize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &CLIExecutor{}
	e.SetArtifactInlineMaxBytes(5)
	var got *a2a.TaskArtifactUpdateEvent
	ok := e.emitChangedFile(&a2asrv.ExecutorContext{}, dir, "big.bin", "file", "application/octet-stream",
		func(ev a2a.Event) bool {
			got, _ = ev.(*a2a.TaskArtifactUpdateEvent)
			return true
		})
	if !ok || got == nil {
		t.Fatal("expected an omitted-file artifact")
	}
	if got.Artifact.Metadata["omitted"] != true {
		t.Fatalf("metadata = %v, want omitted", got.Artifact.Metadata)
	}
	if text := got.Artifact.Parts[0].Text(); !strings.Contains(text, "not delivered") {
		t.Fatalf("text = %q", text)
	}
}

func TestEmitChangedFileSkipsMissing(t *testing.T) {
	e := &CLIExecutor{}
	e.SetArtifactInlineMaxBytes(5)
	var got a2a.Event
	ok := e.emitChangedFile(&a2asrv.ExecutorContext{}, t.TempDir(), "removed.txt", "file", "text/plain",
		func(ev a2a.Event) bool {
			got = ev
			return true
		})
	if !ok {
		t.Fatal("expected the consumer to remain available")
	}
	if got != nil {
		t.Fatalf("deleted files must not produce an artifact: %+v", got)
	}
}

func TestEmitChangedFileWithinLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &CLIExecutor{}
	e.SetArtifactInlineMaxBytes(10)
	var got *a2a.TaskArtifactUpdateEvent
	e.emitChangedFile(&a2asrv.ExecutorContext{}, dir, "small.txt", "file", "text/plain",
		func(ev a2a.Event) bool {
			got, _ = ev.(*a2a.TaskArtifactUpdateEvent)
			return true
		})
	if got == nil || got.Artifact.Parts[0].Raw() == nil {
		t.Fatal("expected an inlined file artifact")
	}
	if got.Artifact.Metadata["omitted"] != nil {
		t.Fatalf("unexpected omitted metadata: %v", got.Artifact.Metadata)
	}
}

func TestDiffInlineLimit(t *testing.T) {
	e := &CLIExecutor{}
	if got := e.diffInlineLimit(); got != defaultDiffInlineMaxBytes {
		t.Fatalf("default diff limit = %d", got)
	}
	e.SetDiffInlineMaxBytes(123)
	if got := e.diffInlineLimit(); got != 123 {
		t.Fatalf("configured diff limit = %d", got)
	}
}

func TestEmitDiffFormat(t *testing.T) {
	cases := []struct {
		name      string
		unified   bool
		mediaType string
		format    string
	}{
		{"unified", true, "text/x-diff", "unified"},
		{"summary", false, "text/plain", "summary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &CLIExecutor{}
			var got *a2a.TaskArtifactUpdateEvent
			e.emitDiff(&a2asrv.ExecutorContext{}, changes.Result{
				Diff: "diff body", Files: []string{"x"}, Truncated: true, Unified: tc.unified,
			}, func(ev a2a.Event) bool {
				got, _ = ev.(*a2a.TaskArtifactUpdateEvent)
				return true
			})
			if got == nil {
				t.Fatal("expected a diff artifact")
			}
			part := got.Artifact.Parts[0]
			if part.Text() != "diff body" || part.URL() != "" {
				t.Fatalf("diff part = text %q url %q, want text only", part.Text(), part.URL())
			}
			if part.MediaType != tc.mediaType {
				t.Fatalf("media type = %q, want %q", part.MediaType, tc.mediaType)
			}
			if got.Artifact.Metadata["format"] != tc.format {
				t.Fatalf("format = %v, want %q", got.Artifact.Metadata["format"], tc.format)
			}
			if got.Artifact.Metadata["truncated"] != true {
				t.Fatalf("truncated metadata = %v", got.Artifact.Metadata["truncated"])
			}
		})
	}
}
