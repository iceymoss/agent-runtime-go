package icoder

import (
	"context"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

func TestConservativeTokenCounterAccountsForMultibyteText(t *testing.T) {
	counter := byteCounter{}
	ascii, err := counter.CountTokens(context.Background(), []agent.Message{agent.NewUserMessage("1234")})
	if err != nil {
		t.Fatal(err)
	}
	multibyte, err := counter.CountTokens(context.Background(), []agent.Message{agent.NewUserMessage("中文文本")})
	if err != nil {
		t.Fatal(err)
	}
	if multibyte <= ascii {
		t.Fatalf("ascii tokens = %d, multibyte tokens = %d", ascii, multibyte)
	}
}

func TestEstimateToolSchemaTokensUsesActiveDefinitions(t *testing.T) {
	registry := agent.NewRegistry()
	tool := &fixedTool{}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	tokens, err := estimateToolSchemaTokens(registry, []string{"write_file"})
	if err != nil || tokens <= 8 {
		t.Fatalf("estimateToolSchemaTokens() = %d, %v", tokens, err)
	}
	if _, err := estimateToolSchemaTokens(registry, []string{"missing"}); err == nil {
		t.Fatal("estimateToolSchemaTokens() accepted an unregistered tool")
	}
}
