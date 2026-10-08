package agent

// InteractionKind classifies why a paused task needs client input. The kinds
// carry different semantics: a permission gate is not the same as the agent
// asking a clarifying question, and neither is a plan approval.
type InteractionKind string

const (
	// InteractionPermission is a safety gate: the CLI wants approval to run an
	// action (shell command, file write, ...).
	InteractionPermission InteractionKind = "permission"
	// InteractionQuestion is the agent proactively asking the user something.
	InteractionQuestion InteractionKind = "question"
	// InteractionPlanApproval is a plan-mode confirmation.
	InteractionPlanApproval InteractionKind = "plan_approval"
)

// Interaction is the canonical description of a task paused on input-required.
// It is delivered to the client in a DataPart next to a human-readable TextPart,
// so both structured and text-only clients can answer.
type Interaction struct {
	Kind    InteractionKind `json:"kind"`
	ID      string          `json:"id"`
	Action  string          `json:"action,omitempty"`
	Target  string          `json:"target,omitempty"`
	Risk    string          `json:"risk,omitempty"`
	Options []string        `json:"options,omitempty"`
	// Questions lists every question of a multi-question interaction (kind
	// "question", action "question"), so a client can answer each one. A single
	// question is also reflected in Target/Options for text-only clients.
	Questions []Question `json:"questions,omitempty"`
	// Schema is a form's requested JSON Schema (interaction kind "question",
	// action "form"), so a client can render fields and answer with content.
	Schema map[string]any `json:"schema,omitempty"`
	Resume bool           `json:"resume"`
}

// Question is one item of a multi-question interaction: the question text and
// its allowed option labels (empty for free text).
type Question struct {
	Question string   `json:"question"`
	Header   string   `json:"header,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// Answer is a client's structured reply to an Interaction. It travels in a
// DataPart of the client's message; the ID must match the pending interaction.
// A plain-text reply remains valid for clients that do not use the extension.
type Answer struct {
	Kind    InteractionKind `json:"kind,omitempty"`
	ID      string          `json:"id,omitempty"`
	Answer  string          `json:"answer"`
	Content map[string]any  `json:"content,omitempty"` // form answers, keyed by field
}
