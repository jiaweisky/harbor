package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

func TestEncodeStreamJSONInputWithImage(t *testing.T) {
	line := agent.EncodeStreamJSONInput("describe this", []agent.Image{
		{MediaType: "image/png", Data: []byte("PNG")},
	})

	var msg struct {
		Message struct {
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Message.Content) != 2 {
		t.Fatalf("content = %+v, want text + image", msg.Message.Content)
	}
	if msg.Message.Content[0].Type != "text" || msg.Message.Content[0].Text != "describe this" {
		t.Fatalf("first block = %+v, want the text", msg.Message.Content[0])
	}
	img := msg.Message.Content[1]
	if img.Type != "image" || img.Source.Type != "base64" ||
		img.Source.MediaType != "image/png" || img.Source.Data != "UE5H" {
		t.Fatalf("image block = %+v, want base64 PNG", img)
	}
}
