package agent

import (
	"encoding/json"
	"strings"
)

// EncodePermissionResponse encodes a stream-json control_response answering a
// can_use_tool request. answer is the client's chosen option ("allow"/"deny") or
// free text. The frame carries both the codebuddy "allowed" field and the claude
// "behavior" field so the same response works for either CLI.
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
	frame := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   inner,
		},
	}
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
