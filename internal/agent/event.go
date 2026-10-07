package agent

// EventKind enumerates normalized events produced by adapters/parsers.
type EventKind int

const (
	EventSession EventKind = iota
	EventTextDelta
	EventTextFinal
	EventThinking
	EventToolCall
	EventToolResult
	EventPermissionRequest
	EventUsage
	EventError
	EventDone
)

// Usage carries token/cost information when the CLI reports it.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// Event is a transport-agnostic event normalized from a coding-agent CLI.
type Event struct {
	Kind      EventKind
	Text      string
	Tool      string
	ToolInput map[string]any
	SessionID string
	Usage     *Usage
	Err       error
	Raw       []byte
}
