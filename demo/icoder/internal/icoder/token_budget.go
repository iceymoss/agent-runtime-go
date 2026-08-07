package icoder

import (
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

func estimateToolSchemaTokens(registry *agent.Registry, names []string) (int, error) {
	tokens := 0
	for _, name := range names {
		tool, ok := registry.Get(name)
		if !ok {
			return 0, fmt.Errorf("active tool %q is not registered", name)
		}
		payload, err := marshalString(tool.Definition())
		if err != nil {
			return 0, err
		}
		tokens += 8 + conservativeTextTokens(payload)
	}
	return tokens, nil
}
