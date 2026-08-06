# Examples

示例按复杂度递进。`hello` 和 `tool-agent` 不需要 API Key；`openai-compat` 连接真实模型。

## Hello

```bash
go run ./examples/hello
```

展示：

- 最小 `agent.Model` 实现
- 构造无工具 Agent
- 执行一次 `Run`
- 读取最终文本

## Tool Agent

```bash
go run ./examples/tool-agent
```

展示：

- JSON Schema 工具声明
- Tool registry 和白名单
- 完整的 model -> tool -> model 循环
- 用 facade 封装特定用途 Agent

## OpenAI-Compat

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat

# 任何 OpenAI 兼容端点都可以：
OPENAI_BASE_URL=https://api.deepseek.com/v1 OPENAI_API_KEY=... OPENAI_MODEL=deepseek-chat go run ./examples/openai-compat
OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_MODEL=qwen2.5 go run ./examples/openai-compat
```

展示：

- 官方适配器 `providers/openaicompat` 连接真实模型（SSE 流式）
- `agent.NewTool` 从结构体生成工具 schema
- 通过 `ObservationEmitter` 实时打印文本增量和工具调用
- 读取 `StopReason`、步数和 token 用量

## iCoder

[`demo/icoder`](../demo/icoder/README.md) 是综合 reference application，包含真实 HTTP provider adapter、Workspace 工具、Permission、SQLite Session、Skills、MCP 和 Sub-Agent。

学习时先运行 `hello`，再阅读 `tool-agent`，最后按需要查看 iCoder 中对应的 adapter。业务项目通常应复制组合思路，而不是直接复制整个 demo。
