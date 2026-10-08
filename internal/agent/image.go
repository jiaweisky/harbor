package agent

// Image is a native image input for a structured (stream-json) CLI turn. It is
// sent as a base64 image content block, so the model receives the pixels rather
// than a file path it cannot view.
type Image struct {
	MediaType string
	Data      []byte
}
