# 快速开始：最小可运行 Agent

以下示例不访问网络，使用确定性的本地 model，因此可以直接验证运行时协议。要求 Go `1.25.0`。

## 准备目录

在仓库根目录执行：

```bash
mkdir -p /tmp/agent-runtime-quickstart
cd /tmp/agent-runtime-quickstart
go mod init quickstart
go mod edit -replace github.com/iceymoss/agent-runtime-go=/path/to/agent-runtime-go
go get github.com/iceymoss/agent-runtime-go
```

将 `/path/to/agent-runtime-go` 替换为当前仓库的绝对路径。如果直接使用仓库自带示例，可以跳过临时模块，运行：

```bash
go run ./examples/hello
go run ./examples/tool-agent
```

## 最小 model 示例

下面的 `Model` 发出一个文本增量和一个终止 chunk。终止 `Response` 中的完整文本必须与之前所有 `ChunkText` 拼接结果完全相同。

```go
package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

type model struct{}

func (model) Name() string { return "local" }

func (model) Capabilities() agent.Capabilities { return agent.Capabilities{} }

func (model) Stream(ctx context.Context, _ *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	chunks := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(chunks)
		message := agent.NewAssistantMessage("你好，Agent Runtime。")
		select {
		case chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: message.Text()}:
		case <-ctx.Done():
			return
		}
		select {
		case chunks <- agent.StreamChunk{
			Type: agent.ChunkFinish,
			Response: &agent.Response{
				Message: message, FinishReason: agent.FinishStop, ModelName: "local-v1",
			},
		}:
		case <-ctx.Done():
		}
	}()
	return chunks, nil
}

func main() {
	runner, err := agent.New(agent.Config{
		Key: "quickstart.hello", ModelName: "local-v1", MaxSteps: 4,
		AllowedTools: []string{},
	}, model{}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("打个招呼")},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s (%s/%s)\n", result.Text, result.Outcome, result.StopReason)
}
```

保存为临时模块的 `main.go` 后执行 `go run .`，输出应为：

```text
你好，Agent Runtime。 (completed/complete)
```

调用链为：

```text
main → agent.New → Agent.Run → model.Stream
     → ChunkText → ChunkFinish(FinishStop) → ValidateResponse
     → RunResult{Outcome: completed, StopReason: complete}
```

## 最小 tool 示例

这个完整程序模拟两次模型调用：第一次请求 `add`，运行时校验参数并执行工具；第二次模型看到 tool 消息后给出答案。

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

type addTool struct{}

func (addTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "add", Description: "将两个整数相加", Strict: true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "integer"},
				"b": map[string]any{"type": "integer"},
			},
			"required": []any{"a", "b"},
		},
	}
}

func (addTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }

func (addTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	var input struct{ A, B int }
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: fmt.Sprintf(`{"sum":%d}`, input.A+input.B)}, nil
}

type scriptedModel struct{}

func (scriptedModel) Name() string { return "scripted" }
func (scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (scriptedModel) Stream(_ context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	chunks := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(chunks)
		if req.Messages[len(req.Messages)-1].Role != agent.RoleTool {
			call := agent.ToolCall{ID: "call-1", Name: "add", Input: `{"a":20,"b":22}`}
			message := agent.Message{
				Role: agent.RoleAssistant,
				Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}},
				FinishReason: agent.FinishToolCalls,
			}
			chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
			chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
				Message: message, FinishReason: agent.FinishToolCalls, ModelName: "scripted-v1",
			}}
			return
		}
		message := agent.NewAssistantMessage("结果是 42。")
		chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: message.Text()}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
			Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
		}}
	}()
	return chunks, nil
}

func main() {
	registry := agent.NewRegistry()
	if err := registry.Register(addTool{}); err != nil {
		panic(err)
	}
	runner, err := agent.New(agent.Config{
		Key: "quickstart.add", ModelName: "scripted-v1", MaxSteps: 4,
		AllowedTools: []string{"add"},
	}, scriptedModel{}, registry)
	if err != nil {
		panic(err)
	}
	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("20 加 22 是多少？")},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s steps=%d\n", result.Text, len(result.Steps))
}
```

执行 `go run .`，输出应为：

```text
结果是 42。 steps=2
```

预期调用链：

```text
Agent.Run
  → 第 0 步 model.Stream
  → 完整 ToolCall(add, {"a":20,"b":22})
  → ToolSet 白名单检查 + JSON 对象检查 + JSON Schema 校验
  → addTool.Execute
  → ToolResult 被包装为 RoleTool 消息并追加到 history
  → 第 1 步 model.Stream
  → FinishStop
  → completed/complete
```

## 接入真实模型时必须保持的协议

- `Stream` 返回的 channel 最终必须关闭。
- 正常流必须恰有一个 `ChunkFinish`；关闭前没有终止响应是协议错误。
- `ChunkToolCall` 必须包含已经拼装完成的调用及完整 JSON 字符串，不能把 provider 的参数碎片直接交给运行时。
- 所有文本 chunk 拼接值、所有工具调用列表，必须与终止 `Response.Message` 完全一致。
- provider 错误应使用 `agent.NewModelError` 分类；`context.Canceled` 和 `context.DeadlineExceeded` 保持原值。
- 模型声明的 `Capabilities` 必须覆盖实际请求的工具、tool choice 和图片能力。

继续阅读：[总览：核心概念与包地图](03-overview.md)。
