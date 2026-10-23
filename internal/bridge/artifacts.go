package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/changes"
)

// emitChanges captures the task's working-tree changes and emits the file,
// image, and diff artifacts they produce. It is a no-op when there is no
// baseline or the workspace cannot be resolved.
func (e *CLIExecutor) emitChanges(ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool) {
	st := e.loadState(taskID)
	if st == nil {
		return
	}
	base, ok := st.takeBaseline()
	if !ok {
		return
	}
	dir, err := e.workspace.Resolve(contextKey(ec))
	if err != nil || dir == "" {
		return
	}
	res := changes.Capture(dir, base, e.diffInlineLimit())
	if !e.emitChangedFiles(ec, dir, res.Files, emit) {
		return
	}
	if res.Diff == "" {
		return
	}
	e.emitDiff(ec, res, emit)
}

// emitDiff delivers the change diff inline as text.
func (e *CLIExecutor) emitDiff(ec *a2asrv.ExecutorContext, res changes.Result, emit func(a2a.Event) bool) {
	data := []byte(res.Diff)
	part := a2a.NewTextPart(res.Diff)
	format := "summary"
	if res.Unified {
		part.MediaType = "text/x-diff"
		format = "unified"
	} else {
		part.MediaType = "text/plain"
	}
	part.Metadata = digestMeta(data)
	emit(artifactEvent(ec, "changes", part, map[string]any{
		"files": toAnySlice(res.Files), "truncated": res.Truncated, "format": format,
	}))
}

// defaultArtifactReadLimit is the default bound on how much of a single changed
// file is read into memory before it is inlined.
const defaultArtifactReadLimit = 256 << 20

// artifactInlineLimit is the effective inline bound for one changed file.
func (e *CLIExecutor) artifactInlineLimit() int64 {
	if e.artifactInlineMaxBytes > 0 {
		return e.artifactInlineMaxBytes
	}
	return defaultArtifactReadLimit
}

// defaultDiffInlineMaxBytes bounds the captured diff before it is truncated.
const defaultDiffInlineMaxBytes = 32 << 20

// diffInlineLimit is the effective diff bound.
func (e *CLIExecutor) diffInlineLimit() int64 {
	if e.diffInlineMaxBytes > 0 {
		return e.diffInlineMaxBytes
	}
	return defaultDiffInlineMaxBytes
}

// imageMediaTypes maps a lower-case file extension to its image MIME type. It
// covers the formats the supported CLIs produce.
var imageMediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// textMediaTypes maps a lower-case file extension to a text media type; unknown
// extensions fall back to application/octet-stream.
var textMediaTypes = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".json": "application/json",
	".yaml": "application/yaml", ".yml": "application/yaml", ".xml": "application/xml",
	".csv": "text/csv", ".diff": "text/x-diff", ".patch": "text/x-diff",
	".go": "text/plain", ".py": "text/plain", ".js": "text/plain", ".ts": "text/plain",
	".sh": "text/plain", ".html": "text/html", ".css": "text/css",
}

// emitChangedFiles emits artifacts for changed images and, when enabled, for
// changed non-image files. It returns false when the consumer has gone away.
func (e *CLIExecutor) emitChangedFiles(
	ec *a2asrv.ExecutorContext, dir string, files []string, emit func(a2a.Event) bool,
) bool {
	for _, rel := range files {
		name, media, ok := artifactForFile(rel, e.nonImageArtifacts)
		if !ok {
			continue
		}
		if !e.emitChangedFile(ec, dir, rel, name, media, emit) {
			return false
		}
	}
	return true
}

// artifactForFile classifies one changed path as an image artifact, an optional
// non-image file artifact, or neither.
func artifactForFile(rel string, includeNonImage bool) (name, media string, ok bool) {
	ext := strings.ToLower(filepath.Ext(rel))
	if media, ok := imageMediaTypes[ext]; ok {
		return "image", media, true
	}
	if !includeNonImage {
		return "", "", false
	}
	media, ok = textMediaTypes[ext]
	if !ok {
		media = "application/octet-stream"
	}
	return "file", media, true
}

// emitChangedFile inlines one changed file as a raw artifact, or emits an
// omitted notice when it cannot be read or exceeds the inline limit. It returns
// false only when the consumer has gone away.
func (e *CLIExecutor) emitChangedFile(
	ec *a2asrv.ExecutorContext, dir, rel, artifactName, media string,
	emit func(a2a.Event) bool,
) bool {
	data, ok := readChangedFile(dir, rel, e.artifactInlineLimit())
	if !ok {
		path, err := safePath(dir, rel)
		if err != nil {
			return true
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return true // deleted or otherwise not a file
		}
		return emit(e.omittedFileArtifact(ec, rel, artifactName, info.Size()))
	}
	return emit(e.fileArtifact(ec, rel, artifactName, media, data))
}

// omittedFileArtifact reports a changed file that could not be inlined, so the
// client learns which file was dropped and why.
func (e *CLIExecutor) omittedFileArtifact(
	ec *a2asrv.ExecutorContext, rel, artifactName string, size int64,
) *a2a.TaskArtifactUpdateEvent {
	reason := "unreadable"
	if size > e.artifactInlineLimit() {
		reason = fmt.Sprintf("size %d exceeds the %d-byte inline limit", size, e.artifactInlineLimit())
	}
	part := a2a.NewTextPart(fmt.Sprintf("[file: %s] not delivered: %s", filepath.Base(rel), reason))
	return artifactEvent(ec, artifactName, part, map[string]any{
		"path": rel, "omitted": true, "size": size,
	})
}

// fileArtifact builds the inline artifact for a changed file.
func (e *CLIExecutor) fileArtifact(
	ec *a2asrv.ExecutorContext, rel, artifactName, media string, data []byte,
) *a2a.TaskArtifactUpdateEvent {
	part := a2a.NewRawPart(data)
	part.MediaType = media
	part.Filename = filepath.Base(rel)
	part.Metadata = digestMeta(data)
	return artifactEvent(ec, artifactName, part, map[string]any{"path": rel})
}

// artifactEvent builds an artifact update event with its name and metadata.
func artifactEvent(
	ec *a2asrv.ExecutorContext, name string, part *a2a.Part, meta map[string]any,
) *a2a.TaskArtifactUpdateEvent {
	evt := a2a.NewArtifactEvent(ec, part)
	evt.Artifact.Name = name
	evt.Artifact.Metadata = meta
	return evt
}

// digestMeta is the integrity metadata carried on a file part.
func digestMeta(data []byte) map[string]any {
	return map[string]any{"sha256": sha256Hex(data), "size": len(data)}
}

// sha256Hex is the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// safePath joins dir and rel, rejecting absolute paths and traversal.
func safePath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	return filepath.Join(dir, rel), nil
}

// readChangedFile reads a changed file inside dir when rel is a safe relative
// path to a regular file within maxBytes.
func readChangedFile(dir, rel string, maxBytes int64) ([]byte, bool) {
	path, err := safePath(dir, rel)
	if err != nil {
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
