package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// errStorage indicates the data plane could not read or verify a reference.
var errStorage = errors.New("storage error")

// promptFromParts assembles the normalized input from a message's parts: an
// ordered list of text blocks, native image inputs, and attachable file paths.
// Text parts become blocks, structured data parts become standalone JSON blocks,
// raw image parts the CLI can accept become native image blocks, and other file
// parts are materialized into dir, attached via their path (caps.AttachFiles)
// or named in a block. Parts the agent cannot accept are rejected.
func (e *CLIExecutor) promptFromParts(ctx context.Context, m *a2a.Message, dir string) (promptInput, error) {
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
			if err := e.addFilePart(ctx, &in, p, dir, n, caps); err != nil {
				return promptInput{}, err
			}
		}
	}
	return in, nil
}

// addFilePart classifies one raw/URL part: a native image, an image attached via
// args, a file attached via args, or a named reference in the prompt. A URL part
// is read through the store when one is configured.
func (e *CLIExecutor) addFilePart(
	ctx context.Context, in *promptInput, p *a2a.Part, dir string, n int, caps agent.Capabilities,
) error {
	// spool keeps every attachment as a file path; any other mode (or no store)
	// lets a CLI-accepted image become a native content block.
	if e.inboundMedia != "spool" {
		img, err := imageInput(p, caps)
		if err != nil {
			return err
		}
		if img != nil {
			in.Images = append(in.Images, *img)
			return nil
		}
	}
	ref, path, err := e.materializeFile(ctx, p, dir, n, caps)
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
// inline, nil for non-images, URL parts, and images the CLI takes as arguments
// (handled as file references), or an error when an image MIME is unsupported.
func imageInput(p *a2a.Part, caps agent.Capabilities) (*agent.Image, error) {
	if p.Raw() == nil || !isImage(p) {
		return nil, nil
	}
	if !supportsMIME(caps.InputImage, p.MediaType) {
		return nil, fmt.Errorf("%w: image %s", errUnsupportedContent, p.MediaType)
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

// materializeFile writes a file part under dir and returns a prompt line naming
// it plus its path (empty for an unreadable URL part). Images are only accepted
// when the agent advertises their MIME type; other files require file-input
// support. A URL part is read through the store when one is configured.
func (e *CLIExecutor) materializeFile(
	ctx context.Context, p *a2a.Part, dir string, n int, caps agent.Capabilities,
) (ref, path string, err error) {
	if err := checkMedia(caps, p, e.mediaTypes); err != nil {
		return "", "", err
	}

	name := safeFilename(p.Filename, n)
	if ext := imageExt[strings.ToLower(p.MediaType)]; ext != "" && filepath.Ext(name) == "" {
		name += ext
	}
	text := fmt.Sprintf("[file: %s]", name)
	if data := p.Raw(); data != nil {
		pth, err := writeUpload(dir, fmt.Sprintf("%d-%s", n, name), data)
		if err != nil {
			return "", "", fmt.Errorf("materialize %s: %w", name, err)
		}
		return text + " saved to " + pth, pth, nil
	}
	if u := p.URL(); u != "" && e.store != nil {
		pth, err := e.materializeReference(ctx, string(u), metaString(p.Metadata, "sha256"),
			dir, fmt.Sprintf("%d-%s", n, name))
		if err != nil {
			return "", "", fmt.Errorf("%w: %s", errStorage, u)
		}
		return text + " saved to " + pth, pth, nil
	}
	return text + " " + string(p.URL()), "", nil
}

// checkMedia rejects a part the agent cannot accept: an image whose MIME type it
// does not advertise, any non-image file without file-input support, or a media
// type outside the media_types policy.
func checkMedia(caps agent.Capabilities, p *a2a.Part, allowed []string) error {
	if strings.HasPrefix(strings.ToLower(p.MediaType), "image/") {
		if !supportsMIME(caps.InputImage, p.MediaType) {
			return fmt.Errorf("%w: image %s", errUnsupportedContent, p.MediaType)
		}
	} else if !caps.InputFile {
		return fmt.Errorf("%w: file %s", errUnsupportedContent, p.MediaType)
	}
	if len(allowed) > 0 {
		if mt := effectiveMediaType(p); mt != "" && !mediaAllowed(allowed, mt) {
			return fmt.Errorf("%w: %s", errUnsupportedContent, mt)
		}
	}
	return nil
}

// effectiveMediaType is the part's declared media type, or one inferred from its
// filename extension.
func effectiveMediaType(p *a2a.Part) string {
	return mediaTypeFor(p.Filename, p.MediaType)
}

// mediaTypeFor returns the declared media type, or one inferred from the file
// name's extension.
func mediaTypeFor(name, declared string) string {
	if declared != "" {
		return declared
	}
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := imageMediaTypes[ext]; ok {
		return m
	}
	return textMediaTypes[ext]
}

// materializeReference streams a store reference into dir, enforcing the size
// bound and verifying the expected sha256 (when provided), so a large file
// never has to be buffered whole.
func (e *CLIExecutor) materializeReference(
	ctx context.Context, uri, wantSHA, dir, name string,
) (string, error) {
	if e.downloadTO > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.downloadTO)
		defer cancel()
	}
	rc, err := e.store.Get(ctx, uri)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	path, err := uploadPath(dir, name)
	if err != nil {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	src := io.Reader(rc)
	if e.maxBytes > 0 {
		src = io.LimitReader(rc, e.maxBytes+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if e.maxBytes > 0 && n > e.maxBytes {
		_ = os.Remove(path)
		return "", fmt.Errorf("reference %s exceeds the size limit", uri)
	}
	if wantSHA != "" && hex.EncodeToString(h.Sum(nil)) != wantSHA {
		_ = os.Remove(path)
		return "", fmt.Errorf("reference %s failed its integrity check", uri)
	}
	return path, nil
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
