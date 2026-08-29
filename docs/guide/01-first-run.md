# 1. 从一次调用开始

## 先跑起来

仓库自带的示例不需要 API key：

```bash
git clone https://github.com/iceymoss/agent-runtime-go
cd agent-runtime-go
go run ./examples/hello        # 模型直接回答
go run ./examples/tool-agent   # 模型先调工具再回答
```

第二个的输出是 `Hangzhou is sunny and 28 C.`——这句话不在任何模板里，是模型调用了一个 Go 函数拿到天气之后自己组织的。这份文档剩下的部分都在讲怎么让这件事在你的项目里发生。

## 这个库负责什么

> 已经做过 LLM 应用可以跳过这一节。

直接调模型 API 很简单：发一段文本，拿一段文本。但只要你想让模型**调用你的代码**，事情就变了——模型不会执行任何东西，它只会说"我想调用 `get_weather`，参数是 `{"city":"Hangzhou"}`"。真正要做的是：

```
你的消息
  → 模型说要调 get_weather({"city":"Hangzhou"})
  → 校验参数符合你声明的 schema
  → 执行你的函数
  → 把结果追加进消息历史
  → 再次调用模型          ← 循环
  → 模型输出最终回答
```

一次"调模型"叫一个 **step**。上面这个过程有两个 step。模型可以在一轮里连续调好几次工具，也就是好几个 step。

这个循环写对不难，写**稳**很难：参数是半截 JSON 怎么办、模型调了个不存在的工具怎么办、模型陷入死循环反复调同一个工具怎么办、历史超出上下文窗口怎么办、什么时候该停。这些问题的答案互相耦合，散在业务代码里就会各处实现不一致。

这个库只负责这个循环。模型适配、Prompt、工具实现、权限、存储都是你的应用的事——它不读环境变量、不选凭据、不连数据库。

## 最小的程序

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/iceymoss/agent-runtime-go
```

一个能跑的 agent 只需要八个符号：`Config`、`New`、`Run` 装配和驱动，`Message` 和它的构造函数承载对话，`Registry` 和 `NewTool` 提供工具，`RunResult` 报告结果。

这一章先不接真实模型——用一个固定回答的假模型，你能立刻跑起来看到形状：

```go
package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

// 模型端口只有三个方法。真实适配器把请求转成上游协议；
// 这里直接返回一个写死的回答。
type fakeModel struct{}

func (fakeModel) Name() string                     { return "fake" }
func (fakeModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (fakeModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	message := agent.NewAssistantMessage("你好，我是一个 agent。")
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "fake-v1",
	}), nil
}

func main() {
	runner, err := agent.New(agent.Config{
		Key:          "my.first",
		ModelName:    "fake-v1",
		MaxSteps:     4,
		AllowedTools: []string{}, // 这一章还没有工具
	}, fakeModel{}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("你是一个简洁的助手。"),
			agent.NewUserMessage("你好"),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	fmt.Println(result.Outcome, result.StopReason)
}
```

```bash
$ go run .
你好，我是一个 agent。
completed complete
```

## 需要注意的

**`Agent` 不保存任何对话状态**，可以在 goroutine 和会话之间安全共享。每轮变化的东西都在 `RunRequest` 里进、`RunResult` 里出。所以你的服务通常只装配一次 `Agent`，然后并发地调 `Run`。

**写 `Model` 适配器时，流式分片累加后必须严格等于终态 response。** 上面用 `agent.StreamResponse` 就是因为这条：最自然的写法（只发一个终态 chunk、不发任何增量）会被运行时拒绝，而 `StreamResponse` 帮你把一个完整回答拆成合规的分片序列。真正逐 token 流式的适配器才自己发增量。

**`Outcome` 和 `StopReason` 是两个正交的字段**，都要看。`Outcome` 是 `completed` / `suspended` / `failed`；`StopReason` 说明为什么停。只有 `completed` + `complete` 才是一个真正的最终答案——比如达到 `MaxSteps` 时模型还在调工具，那是 `suspended` + `max_steps`，不是错误，要由你决定是否继续。

## 深入

- [agent 包参考](../packages/agent.md) —— 白名单、停止条件、循环检测的完整配置
- [核心概念](../concepts.md) —— Message / Model / Tool / 停止语义的心智模型
