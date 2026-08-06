# 快速开始

目标：10 分钟内跑通一个会调用工具的 Agent。要求 Go `1.25.0`。

## 先看效果（不需要 API key）

仓库自带两个确定性示例，克隆后直接运行：

```bash
go run ./examples/hello        # 一次模型直答
go run ./examples/tool-agent   # 完整的两步工具循环
```

运行输出：

```text
$ go run ./examples/hello
Hello from Agent Runtime for Go.

$ go run ./examples/tool-agent
Hangzhou is sunny and 28 C.
```

有任意 OpenAI 兼容的 API key（OpenAI / DeepSeek / Qwen / Kimi，或本地 Ollama）就可以跑真实模型：

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat
```

输出示意（实际内容因模型而异）：

```text
[tool call] get_time {"timezone":"Asia/Shanghai"}
The current date and time in Shanghai is Thu, 06 Aug 2026 12:45:10 CST.

[gpt-4o-mini] stop=complete steps=2 tokens=421
```

## 在自己的项目里从零搭一个

### 1. 安装

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/iceymoss/agent-runtime-go
```

### 2. 写 main.go

下面这个程序接入真实模型、定义一个工具、跑一轮对话，共约 60 行：

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
)

// 工具输入：JSON Schema 会从这个结构体自动生成。
// 非指针且没有 omitempty 的字段自动成为 required。
type TimeInput struct {
	Timezone string `json:"timezone,omitempty" description:"IANA 时区名，如 Asia/Shanghai"`
}

func main() {
	// 模型：换 baseURL 和模型名即可切换任何 OpenAI 兼容服务
	model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

	// 工具：一个函数 + 一个输入结构体
	getTime := agent.MustNewTool("get_time", "获取当前时间",
		func(ctx context.Context, in TimeInput) (agent.ToolResult, error) {
			loc := time.Local
			if in.Timezone != "" {
				l, err := time.LoadLocation(in.Timezone)
				if err != nil {
					// IsError=true：模型可见、可修正的错误，循环会继续
					return agent.ToolResult{IsError: true, Content: "未知时区: " + in.Timezone}, nil
				}
				loc = l
			}
			return agent.ToolResult{Content: time.Now().In(loc).Format(time.RFC3339)}, nil
		})

	registry := agent.NewRegistry()
	if err := registry.Register(getTime); err != nil {
		panic(err)
	}

	// 装配：配置错误（schema 非法、白名单缺失等）在这里就会报错
	runner, err := agent.New(agent.Config{
		Key:       "quickstart.time",
		ModelName: "deepseek-chat",
		MaxSteps:  8,
	}, model, registry)
	if err != nil {
		panic(err)
	}

	// 运行一轮：模型会决定是否调用 get_time
	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewUserMessage("现在东京几点了？"),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	fmt.Printf("(steps=%d tokens=%d)\n", len(result.Steps), result.Usage.TotalTokens)
}
```

### 3. 运行

```bash
DEEPSEEK_API_KEY=sk-... go run .
```

预期输出类似：

```text
东京现在是 2026年8月6日 12:26（JST）。
(steps=2 tokens=873)
```

`steps=2` 说明发生了完整的工具循环：第 1 步模型请求 `get_time`，运行时校验参数并执行；第 2 步模型看到工具结果后给出回答。

## 发生了什么

```text
你的消息
  → 模型返回 tool call: get_time({"timezone":"Asia/Tokyo"})
  → 运行时按 Schema 校验参数 → 执行你的函数
  → 工具结果追加进消息历史 → 再次调用模型
  → 模型输出最终回答 → RunResult{Outcome: completed}
```

运行时在中间做的事：参数不合法会回灌模型修正、重复调用同一工具会触发循环检测、达到 `MaxSteps` 会以 `OutcomeSuspended` 停下——这些都不需要你写代码。

## 下一步

- 想懂核心类型和执行语义 → [核心概念](concepts.md)
- 适配器全部选项（本地 Ollama、OpenRouter、非流式降级）→ [providers/openaicompat](packages/openaicompat.md)
- 核心执行能力（白名单、停止条件、进度观察）→ [agent](packages/agent.md)
- 多轮会话、持久化、崩溃恢复 → [session](packages/session.md)、[durable](packages/durable.md)
- 完整参考应用（权限、SQLite、Skills、MCP、子 Agent）→ [iCoder 教程](icoder.md)
