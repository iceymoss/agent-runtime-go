# Core 运行原理

根包 `agent` 提供 canonical values 和无状态 model/tool runtime。它不保存会话、不读取配置，也不选择模型供应商。

## 一次 Run

```text
1. 复制并校验 RunRequest.Messages
2. 构造 GenerateRequest
3. 调用 Model.Stream
4. 校验并聚合一个模型步骤
5. 如果模型返回最终文本，结束
6. 如果模型返回工具调用：
   - 检查工具白名单
   - 校验 JSON Schema
   - 按顺序执行工具
   - 追加 assistant/tool messages
   - 返回第 2 步
```

Runtime 负责：

- canonical message、tool call、stream 和 usage 契约
- 模型 capability 与流协议校验
- 工具白名单、参数 schema 和有限自动修正
- steps、messages 和 usage 累计
- 最大步数、停止条件、上下文预算与循环检测

调用方负责：

- 实现 `agent.Model` 和 `agent.Tool`
- 选择 Prompt、模型、工具和策略
- 提供历史消息和当前 user message
- 保存 user message 与 `RunResult.Messages`
- 将 runtime error 映射到 HTTP、CLI 或领域错误

## 消息变化

输入：

```text
system: Use tools when facts are needed.
user: What is the weather?
```

模型请求工具后，runtime 在内存中形成：

```text
system
user
assistant: tool_call(get_weather)
tool: {"condition":"sunny"}
```

然后以完整历史再次请求模型。`RunResult.Messages` 只返回本轮新增的 assistant 和 tool messages，不重复返回输入历史。

## 流协议

一个模型步骤可以产生多个 `ChunkText` 和 `ChunkToolCall`，最后必须有一个 `ChunkFinish`。`ChunkFinish.Response.Message` 必须与前面的增量一致。

Provider adapter 应在发送 `ChunkToolCall` 前拼好完整参数，runtime 不接收半截 JSON argument delta。上游失败使用 `ChunkError`，不能伪装成正常 finish。

## 结果判断

同时检查返回 error、`Outcome` 和 `StopReason`：

```go
result, err := runner.Run(ctx, request)
if err != nil {
	return err
}
switch result.Outcome {
case agent.OutcomeCompleted:
	// Persist the completed turn.
case agent.OutcomeSuspended:
	// Resume or ask the user for an action.
case agent.OutcomeFailed:
	// Persist a terminal failure if required.
}
```

`StopReason` 用于解释结束原因，例如完成、最大步数、循环检测或上下文预算。

## 并发与不可变性

`Agent` 在构造时取得不可变 `ToolSet` 快照，可以并发复用。之后修改原 registry 不会改变已经创建的 Agent。每次 `Run` 的消息和步骤状态只属于该调用。

## 下一步

- 最小运行：`examples/hello`
- 完整工具循环：`examples/tool-agent`
- 业务封装：[封装自己的 Agent](02-build-your-agent.md)
