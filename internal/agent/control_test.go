package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

func TestEncodeQuestionResponse(t *testing.T) {
	var frame struct {
		Type     string `json:"type"`
		Response struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Allowed      bool `json:"allowed"`
				UpdatedInput struct {
					Answers map[string]string `json:"answers"`
				} `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	line := agent.EncodeQuestionResponse("perm_q1", map[string]string{
		"Favorite color?": "Blue", "Favorite animal?": "Cat",
	})
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if frame.Type != "control_response" || frame.Response.RequestID != "perm_q1" || !frame.Response.Response.Allowed {
		t.Fatalf("frame = %+v", frame)
	}
	answers := frame.Response.Response.UpdatedInput.Answers
	if answers["Favorite color?"] != "Blue" || answers["Favorite animal?"] != "Cat" {
		t.Fatalf("answers = %v, want both questions keyed", answers)
	}
}

func TestEncodeElicitationResponse(t *testing.T) {
	var frame struct {
		Response struct {
			RequestID string         `json:"request_id"`
			Response  map[string]any `json:"response"`
		} `json:"response"`
	}
	line := agent.EncodeElicitationResponse("el_1", "accept", map[string]any{"color": "blue"})
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if frame.Response.RequestID != "el_1" || frame.Response.Response["action"] != "accept" {
		t.Fatalf("frame = %+v", frame)
	}
	content, _ := frame.Response.Response["content"].(map[string]any)
	if content["color"] != "blue" {
		t.Fatalf("content = %v, want color blue", content)
	}
}

func TestEncodePermissionResponse(t *testing.T) {
	cases := []struct {
		answer string
		allow  bool
	}{
		{"allow", true},
		{"YES", true},
		{"deny", false},
		{"", false},
		{"free text", false},
	}
	for _, c := range cases {
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string         `json:"subtype"`
				RequestID string         `json:"request_id"`
				Response  map[string]any `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(agent.EncodePermissionResponse("perm_1", c.answer)), &frame); err != nil {
			t.Fatalf("answer %q: unmarshal: %v", c.answer, err)
		}
		if frame.Type != "control_response" || frame.Response.Subtype != "success" || frame.Response.RequestID != "perm_1" {
			t.Fatalf("answer %q: frame = %+v", c.answer, frame)
		}
		if got := frame.Response.Response["allowed"]; got != c.allow {
			t.Errorf("answer %q: allowed = %v, want %v", c.answer, got, c.allow)
		}
		want := "deny"
		if c.allow {
			want = "allow"
		}
		if got := frame.Response.Response["behavior"]; got != want {
			t.Errorf("answer %q: behavior = %v, want %v", c.answer, got, want)
		}
	}
}
