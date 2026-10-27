package agent

import (
	"encoding/json"
	"strings"
)

// EncodePermissionResponse encodes a stream-json control_response answering a
// can_use_tool request. answer is the client's chosen option ("allow"/"deny") or
// free text. The inner response carries both the codebuddy "allowed" field and
// the claude "behavior" field so the same frame works for either CLI.
func EncodePermissionResponse(requestID, answer string) string {
	allow := permissionAllowed(answer)
	behavior := "deny"
	if allow {
		behavior = "allow"
	}

	inner := map[string]any{"allowed": allow, "behavior": behavior}
	if reason := strings.TrimSpace(answer); !allow && reason != "" {
		inner["reason"] = reason
		inner["message"] = reason
	}

	return controlResponse(requestID, inner)
}

// EncodeQuestionResponse encodes a stream-json control_response answering an
// AskUserQuestion tool request (delivered as can_use_tool). The chosen answers
// are injected through updatedInput.answers, keyed by each question's text.
func EncodeQuestionResponse(requestID string, answers map[string]string) string {
	return controlResponse(requestID, map[string]any{
		"allowed":  true,
		"behavior": "allow",
		"updatedInput": map[string]any{
			"answers": answers,
		},
	})
}

// EncodeElicitationResponse encodes a stream-json control_response answering an
// elicitation_create (form) request. action is "accept" (with content), or
// "decline"/"cancel" (no content).
func EncodeElicitationResponse(requestID, action string, content map[string]any) string {
	inner := map[string]any{"action": action}
	if len(content) > 0 {
		inner["content"] = content
	}

	return controlResponse(requestID, inner)
}

// EncodeControlInitialize encodes the SDK capability handshake sent to a
// stream-json CLI before its first prompt. Declaring askUserQuestion lets the
// CLI route the AskUserQuestion tool through can_use_tool, which Harbor answers
// as an InteractionQuestion.
func EncodeControlInitialize() string {
	return encodeFrame(map[string]any{
		"type":       "control_request",
		"request_id": "harbor-init",
		"request": map[string]any{
			"subtype": "initialize",
			"capabilities": map[string]any{
				"askUserQuestion": true,
				"elicitation":     map[string]any{"form": true},
			},
			"sdkMcpServers": []any{},
			"hooks":         map[string]any{},
		},
	})
}

// controlResponse wraps a command's inner response in the stream-json
// control_response envelope.
func controlResponse(requestID string, inner map[string]any) string {
	return encodeFrame(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   inner,
		},
	})
}

// encodeFrame marshals one stream-json record and appends the newline that
// delimits records.
func encodeFrame(frame map[string]any) string {
	b, _ := json.Marshal(frame)
	return string(b) + "\n"
}

// permissionAllowed reports whether a client answer approves a tool call. Only
// an explicit affirmative counts; anything else (including an empty answer)
// denies.
func permissionAllowed(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "allow", "yes", "approve", "accept", "true", "1", "ok":
		return true
	}
	return false
}
