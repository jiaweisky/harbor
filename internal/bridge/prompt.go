package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
)

// errUnsupportedContent indicates a message part the agent's capabilities
// cannot accept. It is raised before the CLI starts so an unsupported request
// never spawns a process.
var errUnsupportedContent = errors.New("unsupported content type")

// promptFromParts assembles the normalized prompt from a message's parts. Text
// parts are concatenated, structured data parts are serialized as JSON, and file
// parts are materialized into dir (or referenced by URL) with a line naming the
// target so the agent can open it. Parts the agent cannot accept are rejected.
func (e *CLIExecutor) promptFromParts(m *a2a.Message, dir string) (string, error) {
	if m == nil {
		return "", errNoText
	}
	caps := e.launcher.Capabilities()
	var b strings.Builder
	files := 0
	for _, p := range m.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text() != "":
			appendBlock(&b, p.Text())
		case p.Data() != nil:
			appendDataBlock(&b, p.Data())
		case p.Raw() != nil || p.URL() != "":
			files++
			ref, err := materializeFile(p, dir, files, caps)
			if err != nil {
				return "", err
			}
			appendBlock(&b, ref)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// appendBlock appends a block to the prompt, separated from any earlier block.
func appendBlock(b *strings.Builder, block string) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(block)
}

// appendDataBlock serializes a structured part as JSON for a CLI that has no
// native structured-input channel.
func appendDataBlock(b *strings.Builder, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	appendBlock(b, string(buf))
}

// materializeFile writes a raw file part under dir and returns a prompt line
// naming it, or names a URL part. Images are only accepted when the agent
// advertises their MIME type; other files require file-input support.
func materializeFile(p *a2a.Part, dir string, n int, caps agent.Capabilities) (string, error) {
	if strings.HasPrefix(strings.ToLower(p.MediaType), "image/") {
		if !supportsMIME(caps.InputImage, p.MediaType) {
			return "", fmt.Errorf("%w: image %s", errUnsupportedContent, p.MediaType)
		}
	} else if !caps.InputFile {
		return "", fmt.Errorf("%w: file %s", errUnsupportedContent, p.MediaType)
	}

	name := safeFilename(p.Filename, n)
	text := fmt.Sprintf("[file: %s]", name)
	if data := p.Raw(); data != nil {
		path, err := writeUpload(dir, fmt.Sprintf("%d-%s", n, name), data)
		if err != nil {
			return "", fmt.Errorf("materialize %s: %w", name, err)
		}
		return text + " saved to " + path, nil
	}
	return text + " " + string(p.URL()), nil
}

// writeUpload writes data under dir/.harbor/uploads, falling back to the system
// temp directory when the task has no working directory.
func writeUpload(dir, name string, data []byte) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	uploads := filepath.Join(dir, ".harbor", "uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(uploads, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// safeFilename reduces a client-supplied name to a single path element, falling
// back to a generated name when it is empty or unusable.
func safeFilename(name string, n int) string {
	base := filepath.Base(filepath.Clean(name))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return fmt.Sprintf("upload-%d", n)
	}
	return base
}

// supportsMIME reports whether mime is in the supported list (case-insensitive).
func supportsMIME(supported []string, mime string) bool {
	for _, m := range supported {
		if strings.EqualFold(m, mime) {
			return true
		}
	}
	return false
}
