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

// promptFromParts assembles the normalized input from a message's parts: an
// ordered list of text blocks, native image inputs, and attachable file paths.
// Text parts become blocks, structured data parts become standalone JSON blocks,
// raw image parts the CLI can accept become native image blocks, other raw file
// parts are materialized into dir and attached via their path or named in a
// block, and URL file parts are passed through as text and metadata. Raw parts
// the agent cannot accept are rejected.
func (e *CLIExecutor) promptFromParts(m *a2a.Message, dir string) (promptInput, error) {
	var in promptInput
	if m == nil {
		return in, errNoText
	}

	caps := e.launcher.Capabilities()
	n := 0
	for _, p := range m.Parts {
		if p == nil {
			continue
		}

		switch {
		case p.Text() != "":
			in.Blocks = appendBlock(in.Blocks, p.Text())
		case p.Data() != nil:
			in.Blocks = appendDataBlock(in.Blocks, p.Data())
		case p.Raw() != nil || p.URL() != "":
			n++
			if err := e.addFilePart(&in, p, dir, n, caps); err != nil {
				return promptInput{}, err
			}
		}
	}

	return in, nil
}

// addFilePart classifies one raw/URL part. URL parts are passed through as
// text and metadata; raw parts are handled by addRawPart.
func (e *CLIExecutor) addFilePart(
	in *promptInput, p *a2a.Part, dir string, n int, caps agent.Capabilities,
) error {
	if u := p.URL(); u != "" {
		addURLPart(in, p, string(u), n)
		return nil
	}

	return e.addRawPart(in, p, dir, n, caps)
}

// addURLPart passes a URL part through as a prompt block without fetching or
// validating it.
func addURLPart(in *promptInput, p *a2a.Part, u string, n int) {
	in.Blocks = appendBlock(in.Blocks, fileURLBlock(p, u, n))
}

// addRawPart classifies one raw part. A raw image the CLI accepts inline becomes
// a native content block; other raw files are materialized and attached by path
// or named in a block.
func (e *CLIExecutor) addRawPart(
	in *promptInput, p *a2a.Part, dir string, n int, caps agent.Capabilities,
) error {
	img, err := imageInput(p, caps)
	if err != nil {
		return err
	}

	if img != nil {
		in.Images = append(in.Images, *img)
		return nil
	}

	ref, path, err := e.materializeFile(p, dir, n, caps)
	if err != nil {
		return err
	}

	switch {
	case path != "" && isImage(p) && caps.AttachImages:
		in.ImageFiles = append(in.ImageFiles, path)
	case caps.AttachFiles && path != "":
		in.Files = append(in.Files, path)
	default:
		in.Blocks = appendBlock(in.Blocks, ref)
	}

	return nil
}

// promptInput is the normalized input assembled from a message's parts.
type promptInput struct {
	Blocks     []string
	Images     []agent.Image
	Files      []string
	ImageFiles []string
}

// isImage reports whether a part's media type is an image.
func isImage(p *a2a.Part) bool {
	return strings.HasPrefix(strings.ToLower(p.MediaType), "image/")
}

// imageInput returns a native image input for a raw image part the CLI accepts
// inline, nil for non-images and images the CLI takes as arguments (handled as
// file references), or an error when an image MIME is unsupported.
func imageInput(p *a2a.Part, caps agent.Capabilities) (*agent.Image, error) {
	if p.Raw() == nil || !isImage(p) {
		return nil, nil
	}

	if err := checkRawMedia(caps, p); err != nil {
		return nil, err
	}

	if !caps.InlineImages {
		return nil, nil // materialized and attached via image_args
	}

	return &agent.Image{MediaType: p.MediaType, Data: p.Raw()}, nil
}

// appendBlock appends a non-empty text block to blocks.
func appendBlock(blocks []string, block string) []string {
	if block == "" {
		return blocks
	}
	return append(blocks, block)
}

// appendDataBlock appends a structured part as a standalone JSON block.
func appendDataBlock(blocks []string, v any) []string {
	buf, err := json.Marshal(v)
	if err != nil {
		return blocks
	}

	return appendBlock(blocks, string(buf))
}

// materializeFile writes a raw file part under dir and returns a prompt line
// naming it plus its path. Images are only accepted when the agent advertises
// their MIME type; other files require file-input support.
func (e *CLIExecutor) materializeFile(
	p *a2a.Part, dir string, n int, caps agent.Capabilities,
) (ref, path string, err error) {
	if err := checkRawMedia(caps, p); err != nil {
		return "", "", err
	}

	name := safeFilename(p.Filename, n)
	if ext := imageExt[strings.ToLower(p.MediaType)]; ext != "" && filepath.Ext(name) == "" {
		name += ext
	}

	text := fileLabel(name)

	pth, err := writeUpload(dir, fmt.Sprintf("%d-%s", n, name), p.Raw())
	if err != nil {
		return "", "", fmt.Errorf("materialize %s: %w", name, err)
	}

	return text + " saved to " + pth, pth, nil
}

// fileURLBlock renders a URL file part as a prompt block, preserving its name,
// media type, URL, and metadata without fetching it.
func fileURLBlock(p *a2a.Part, u string, n int) string {
	text := fileLabel(safeFilename(p.Filename, n))
	if p.MediaType != "" {
		text += " (" + p.MediaType + ")"
	}

	text += " " + u
	if len(p.Metadata) > 0 {
		if b, err := json.Marshal(p.Metadata); err == nil {
			text += " metadata=" + string(b)
		}
	}

	return text
}

// fileLabel renders the prompt label for one file.
func fileLabel(name string) string { return fmt.Sprintf("[file: %s]", name) }

// checkRawMedia rejects a raw part the agent cannot accept: an image whose MIME
// type it does not advertise, or a non-image file without file-input support.
func checkRawMedia(caps agent.Capabilities, p *a2a.Part) error {
	if strings.HasPrefix(strings.ToLower(p.MediaType), "image/") {
		if !supportsMIME(caps.InputImage, p.MediaType) {
			return fmt.Errorf("%w: image %s", errUnsupportedContent, p.MediaType)
		}
	} else if !caps.InputFile {
		return fmt.Errorf("%w: file %s", errUnsupportedContent, p.MediaType)
	}

	return nil
}

// imageExt maps an image MIME type to a file extension, appended when a
// materialized image's name has none (CLIs that take images by path may infer
// the format from the extension).
var imageExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
}

// writeUpload writes data under the attachments directory dir, falling back to
// the system temp directory when the task has no working directory.
func writeUpload(dir, name string, data []byte) (string, error) {
	path, err := uploadPath(dir, name)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}

	return path, nil
}

// uploadPath returns the materialization path under the attachments directory
// dir, creating it.
func uploadPath(dir, name string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	return filepath.Join(dir, name), nil
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
