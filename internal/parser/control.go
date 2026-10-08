package parser

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/jiaweisky/harbor/internal/agent"
)

// parseControlRequest handles a CLI control_request:
//   - can_use_tool: AskUserQuestion -> a question; any other tool -> a permission.
//   - elicitation_create: a form question carrying its requested JSON Schema.
//
// Other subtypes have no Harbor-side handler and are ignored.
func (p *StreamJSON) parseControlRequest(rec []byte, out func(agent.Event)) {
	var m struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype         string         `json:"subtype"`
			ToolName        string         `json:"tool_name"`
			Input           map[string]any `json:"input"`
			Message         string         `json:"message"`
			RequestedSchema map[string]any `json:"requestedSchema"`
		} `json:"request"`
	}
	if json.Unmarshal(rec, &m) != nil || m.RequestID == "" {
		return
	}
	switch m.Request.Subtype {
	case "can_use_tool":
		switch m.Request.ToolName {
		case "AskUserQuestion":
			out(questionEvent(m.RequestID, m.Request.Input, rec))
		case "ExitPlanMode":
			out(planEvent(m.RequestID, m.Request.Input, rec))
		default:
			out(permissionEvent(m.RequestID, m.Request.ToolName, m.Request.Input, rec))
		}
	case "elicitation_create":
		out(formEvent(m.RequestID, m.Request.Message, m.Request.RequestedSchema, rec))
	}
}

// planEvent builds the input-required event for a plan-mode approval
// (ExitPlanMode). It is answered like a permission (allow/deny).
func planEvent(id string, input map[string]any, raw []byte) agent.Event {
	plan, _ := input["plan"].(string)
	text := "plan approval required - reply allow or deny"
	if plan != "" {
		text = "plan approval required:\n" + plan
	}
	return agent.Event{
		Kind:      agent.EventPermissionRequest,
		Text:      text,
		Tool:      "ExitPlanMode",
		ToolInput: input,
		Interaction: &agent.Interaction{
			Kind:    agent.InteractionPlanApproval,
			ID:      id,
			Action:  "plan",
			Target:  plan,
			Options: []string{"allow", "deny"},
			Resume:  true,
		},
		Raw: raw,
	}
}

// formEvent builds the input-required event for an elicitation_create (form).
func formEvent(id, message string, schema map[string]any, raw []byte) agent.Event {
	fields := formFields(schema)
	itx := &agent.Interaction{
		Kind:   agent.InteractionQuestion,
		ID:     id,
		Action: "form",
		Target: message,
		Schema: schema,
		Resume: true,
	}
	// A single-choice form exposes its options directly.
	if len(fields) == 1 && len(fields[0].enum) > 0 {
		itx.Options = fields[0].enum
	}
	return agent.Event{
		Kind:        agent.EventPermissionRequest,
		Text:        formText(message, fields),
		Tool:        "elicitation",
		ToolInput:   schema,
		Interaction: itx,
		Raw:         raw,
	}
}

// formField is one field of a form schema.
type formField struct {
	name string
	enum []string
}

// formFields extracts the top-level properties of a form schema, sorted by name.
func formFields(schema map[string]any) []formField {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	fields := make([]formField, 0, len(names))
	for _, name := range names {
		p, _ := props[name].(map[string]any)
		f := formField{name: name}
		if arr, ok := p["enum"].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					f.enum = append(f.enum, s)
				}
			}
		}
		fields = append(fields, f)
	}
	return fields
}

// formText renders the form's message and fields as a prompt.
func formText(message string, fields []formField) string {
	if len(fields) == 0 {
		return message
	}
	var b strings.Builder
	b.WriteString(message)
	b.WriteString(" (reply with a content object keyed by field:")
	for _, f := range fields {
		b.WriteString(" " + f.name)
		if len(f.enum) > 0 {
			b.WriteString(" [" + strings.Join(f.enum, "|") + "]")
		}
	}
	b.WriteString(")")
	return b.String()
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
			Kind:      agent.InteractionQuestion,
			ID:        id,
			Action:    "question",
			Target:    firstQuestionText(qs),
			Options:   firstQuestionOptions(qs),
			Questions: interactionQuestions(qs),
			Resume:    true,
		},
		Raw: raw,
	}
}

// askUserQuestion is one question of an AskUserQuestion tool input
// ({questions:[{question, header, options:[{label, ...}], multiSelect}]}).
type askUserQuestion struct {
	Question string `json:"question"`
	Header   string `json:"header"`
	Options  []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// interactionQuestions maps the parsed questions to the canonical interaction
// form so a client can answer every question of a multi-question tool call.
func interactionQuestions(qs []askUserQuestion) []agent.Question {
	if len(qs) == 0 {
		return nil
	}
	out := make([]agent.Question, 0, len(qs))
	for _, q := range qs {
		out = append(out, agent.Question{
			Question: q.Question,
			Header:   q.Header,
			Options:  questionLabels(q),
		})
	}
	return out
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
	return s + " - reply allow or deny"
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
