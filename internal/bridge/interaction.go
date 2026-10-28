package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
)

// interactionFromEvent builds the canonical descriptor for a permission event.
// It uses the adapter's structured interaction when present and fills the gaps
// from the event's tool fields, assigning a fresh id for cross-talk checks.
func interactionFromEvent(ev agent.Event) *agent.Interaction {
	itx := &agent.Interaction{Kind: agent.InteractionPermission, Resume: true}

	if ev.Interaction != nil {
		c := *ev.Interaction
		itx = &c
	}

	if itx.Kind == "" {
		itx.Kind = agent.InteractionPermission
	}

	if itx.ID == "" {
		itx.ID = newInteractionID()
	}

	if itx.Action == "" {
		itx.Action = ev.Tool
	}

	return itx
}

// interactionMessage builds the input-required message: a human-readable text
// part plus a DataPart carrying the structured interaction for rich clients.
func interactionMessage(prompt string, itx *agent.Interaction, sessionID string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(prompt))
	m.Parts = append(m.Parts, a2a.NewDataPart(interactionPayload(itx)))
	m.Metadata = map[string]any{
		metaKeyKind:          string(itx.Kind),
		metaKeyInteractionID: itx.ID,
	}

	// Record the CLI session so a paused task can be resumed after a restart.
	if sessionID != "" {
		m.Metadata[metaKeySessionID] = sessionID
	}

	return m
}

// interactionPayload renders an interaction as JSON-native values (string,
// bool, []any). The DataPart must not hold the Go struct: the task store
// serializes tasks with gob, which requires every type carried in an interface
// to be registered. Marshaling through JSON keeps the wire form and the stored
// form identical and derived from the struct's own tags.
func interactionPayload(itx *agent.Interaction) map[string]any {
	m := map[string]any{}
	if b, err := json.Marshal(itx); err == nil {
		_ = json.Unmarshal(b, &m)
	}

	return m
}

// answerFromMessage maps a client's reply onto an agent.Input. The interaction
// id may arrive in a structured DataPart or in the message metadata; either way
// it is checked against the pending interaction. A reply with no id is accepted
// as a text fallback for clients that do not use the extension.
func (e *CLIExecutor) answerFromMessage(
	ec *a2asrv.ExecutorContext, taskID string, m *a2a.Message,
) (agent.Input, error) {
	id, answer, content := parseAnswer(m)
	kind, action, target := "answer", "", ""

	if cur, ok := e.answerableInteraction(ec, taskID); ok {
		if id != "" && id != cur.ID {
			return agent.Input{}, fmt.Errorf("interaction id mismatch: got %q, want %q", id, cur.ID)
		}

		// Use the interaction's own id, kind, action, and target so the session
		// can build the right response even when the client answered with text.
		id, kind, action, target = cur.ID, string(cur.Kind), cur.Action, cur.Target
	}

	return agent.Input{
		Kind: kind, ID: id, Action: action, Target: target, Answer: answer, Content: content,
	}, nil
}

// parseAnswer extracts the interaction id, answer text, and form content from a
// client reply. It prefers a structured DataPart and falls back to the message
// metadata (key "harbor.interactionId") plus the first text part.
func parseAnswer(m *a2a.Message) (id, answer string, content map[string]any) {
	if m == nil {
		return "", "", nil
	}

	for _, p := range m.Parts {
		if p == nil || p.Data() == nil {
			continue
		}

		b, err := json.Marshal(p.Data())
		if err != nil {
			continue
		}

		var a agent.Answer
		if err := json.Unmarshal(b, &a); err != nil {
			continue
		}

		if a.ID != "" || a.Answer != "" || a.Content != nil {
			return a.ID, a.Answer, a.Content
		}
	}

	return metaString(m.Metadata, metaKeyInteractionID), firstTextPart(m), nil
}

// answerableInteraction returns the input request a paused task is waiting on.
// It falls back to the interaction recorded in the persisted input-required
// status message, so a paused task remains answerable across a restart (when a
// durable task store is configured).
func (e *CLIExecutor) answerableInteraction(ec *a2asrv.ExecutorContext, taskID string) (*agent.Interaction, bool) {
	if st := e.loadState(taskID); st != nil {
		if it := st.pendingInteraction(); it != nil {
			return it, true
		}
	}

	if ec.StoredTask != nil {
		if itx, ok := interactionFromStatusMessage(ec.StoredTask.Status.Message); ok {
			return itx, true
		}
	}

	return nil, false
}

// interactionFromStatusMessage reconstructs the interaction carried by an
// input-required status message.
func interactionFromStatusMessage(m *a2a.Message) (*agent.Interaction, bool) {
	if m == nil {
		return nil, false
	}

	for _, p := range m.Parts {
		if p == nil || p.Data() == nil {
			continue
		}

		b, err := json.Marshal(p.Data())
		if err != nil {
			continue
		}

		var itx agent.Interaction
		if err := json.Unmarshal(b, &itx); err == nil && itx.ID != "" {
			return &itx, true
		}
	}

	return nil, false
}

// newInteractionID returns a short random interaction identifier.
func newInteractionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "itx_unknown"
	}

	return "itx_" + hex.EncodeToString(b[:])
}

func metaString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func metaBool(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func metaInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}

	return 0
}

// metaMap returns a nested JSON object under key, or nil when absent.
func metaMap(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func firstTextPart(m *a2a.Message) string {
	if m == nil {
		return ""
	}

	for _, p := range m.Parts {
		if p != nil {
			if t := p.Text(); t != "" {
				return t
			}
		}
	}

	return ""
}
