package parser

import (
	"encoding/json"
	"strings"

	"github.com/jiaweisky/harbor/internal/agent"
)

// parseControlRequest handles a CLI control_request. A can_use_tool request is
// surfaced as an input-required interaction — a question for AskUserQuestion, a
// permission otherwise; other subtypes have no Harbor-side handler and are
// ignored.
func (p *StreamJSON) parseControlRequest(rec []byte, out func(agent.Event)) {
	var m struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string         `json:"subtype"`
			ToolName string         `json:"tool_name"`
			Input    map[string]any `json:"input"`
		} `json:"request"`
	}
	if json.Unmarshal(rec, &m) != nil || m.Request.Subtype != "can_use_tool" || m.RequestID == "" {
		return
	}
	if m.Request.ToolName == "AskUserQuestion" {
		out(questionEvent(m.RequestID, m.Request.Input, rec))
		return
	}
	out(permissionEvent(m.RequestID, m.Request.ToolName, m.Request.Input, rec))
}

// permissionEvent builds the input-required event for a tool permission request.
func permissionEvent(id, tool string, input map[string]any, raw []byte) agent.Event {
	return agent.Event{
		Kind:      agent.EventPermissionRequest,
		Text:      permissionText(tool, input),
		Tool:      tool,
		ToolInput: input,
		Interaction: &agent.Interaction{
			Kind:    agent.InteractionPermission,
			ID:      id,
			Action:  tool,
			Target:  permissionTarget(input),
			Options: []string{"allow", "deny"},
			Resume:  true,
		},
		Raw: raw,
	}
}

// questionEvent builds the input-required event for an AskUserQuestion request.
func questionEvent(id string, input map[string]any, raw []byte) agent.Event {
	qs := askUserQuestions(input)
	return agent.Event{
		Kind:      agent.EventPermissionRequest,
		Text:      questionText(qs),
		Tool:      "AskUserQuestion",
		ToolInput: input,
		Interaction: &agent.Interaction{
			Kind:    agent.InteractionQuestion,
			ID:      id,
			Action:  "question",
			Target:  firstQuestionText(qs),
			Options: firstQuestionOptions(qs),
			Resume:  true,
		},
		Raw: raw,
	}
}

// askUserQuestion is one question of an AskUserQuestion tool input
// ({questions:[{question, header, options:[{label, ...}], multiSelect}]}).
type askUserQuestion struct {
	Question string `json:"question"`
	Options  []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// askUserQuestions parses the "questions" array of an AskUserQuestion input.
func askUserQuestions(input map[string]any) []askUserQuestion {
	b, err := json.Marshal(input["questions"])
	if err != nil {
		return nil
	}
	var qs []askUserQuestion
	_ = json.Unmarshal(b, &qs)
	return qs
}

// firstQuestionText returns the first question's text (the answer key).
func firstQuestionText(qs []askUserQuestion) string {
	if len(qs) == 0 {
		return ""
	}
	return qs[0].Question
}

// firstQuestionOptions returns the first question's option labels.
func firstQuestionOptions(qs []askUserQuestion) []string {
	if len(qs) == 0 {
		return nil
	}
	return questionLabels(qs[0])
}

// questionLabels returns a question's non-empty option labels.
func questionLabels(q askUserQuestion) []string {
	labels := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		if o.Label != "" {
			labels = append(labels, o.Label)
		}
	}
	return labels
}

// questionText renders the questions and their options as a prompt.
func questionText(qs []askUserQuestion) string {
	if len(qs) == 0 {
		return "the agent is asking a question"
	}
	var b strings.Builder
	for i, q := range qs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(q.Question)
		if labels := questionLabels(q); len(labels) > 0 {
			b.WriteString(" [" + strings.Join(labels, ", ") + "]")
		}
	}
	return b.String()
}

// permissionText renders a human-readable prompt for a tool permission request.
func permissionText(tool string, input map[string]any) string {
	s := "permission required: " + tool
	if t := permissionTarget(input); t != "" {
		s += " (" + t + ")"
	}
	return s + " — reply allow or deny"
}

// permissionTarget picks the most descriptive string in a tool's input.
func permissionTarget(input map[string]any) string {
	for _, k := range []string{"command", "file_path", "path", "pattern", "url"} {
		if v, ok := input[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
