package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
)

type ICoderRunner struct{ Config icoder.Config }

func (r ICoderRunner) Run(ctx context.Context, workspace, prompt string) (metrics RunMetrics, resultErr error) {
	config := r.Config
	config.Workspace = workspace
	config.Database = filepath.Join(workspace, ".icoder-eval.db")
	config.SessionID = "eval"
	config.AllowWrites = true
	app, err := icoder.NewApp(ctx, config)
	if err != nil {
		return RunMetrics{}, err
	}
	defer func() {
		resultErr = errorsJoin(resultErr, app.Close(context.Background()))
		_ = os.Remove(config.Database)
		_ = os.Remove(config.Database + "-shm")
		_ = os.Remove(config.Database + "-wal")
	}()
	result, err := app.Run(ctx, prompt, nil)
	if result != nil {
		metrics.PromptTokens = result.Usage.PromptTokens
		metrics.CompletionTokens = result.Usage.CompletionTokens
		metrics.TotalTokens = result.Usage.TotalTokens
		for _, message := range result.Messages {
			metrics.ToolCalls += len(message.ToolResults())
		}
	}
	return metrics, err
}

func errorsJoin(left, right error) error {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return errors.Join(left, right)
}
