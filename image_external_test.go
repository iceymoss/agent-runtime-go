package agent_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

func TestPortableImagePartPublicContract(t *testing.T) {
	message := agent.Message{Role: agent.RoleUser, Parts: []agent.ContentPart{
		{Type: agent.PartText, Text: "before"},
		{Type: agent.PartImage, Image: &agent.ImageContent{MediaType: "image/jpeg", Data: []byte{1}}},
		{Type: agent.PartText, Text: "after"},
	}}
	if err := agent.ValidateMessage(message); err != nil {
		t.Fatalf("ValidateMessage() error = %v", err)
	}
	if err := agent.ValidateRunRequestCapabilities(agent.RunRequest{Messages: []agent.Message{message}}, agent.Capabilities{ImageInput: true}); err != nil {
		t.Fatalf("ValidateRunRequestCapabilities() error = %v", err)
	}
}
