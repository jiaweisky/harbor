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
	Resume  bool            `json:"resume"`
}

// Answer is a client's structured reply to an Interaction. It travels in a
// DataPart of the client's message; the ID must match the pending interaction.
// A plain-text reply remains valid for clients that do not use the extension.
type Answer struct {
	Kind   InteractionKind `json:"kind,omitempty"`
	ID     string          `json:"id,omitempty"`
	Answer string          `json:"answer"`
}
