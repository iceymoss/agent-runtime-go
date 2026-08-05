# 生产集成检查表

iCoder 是 boundary-correct reference application，不是完整 sandbox 或 distributed host。

## Replace Before Production

| Demo | 生产替换 |
|---|---|
| 单 HTTP response provider | 真正流式 provider adapter + `agenttest.TestModel` |
| byte token counter | 精确 provider/model/tokenizer counter |
| 本地 workspace confinement | container、gVisor、Firecracker 或远端 sandbox |
| permission memory store | durable approval/grant store + fence validator |
| SQLite turn aggregate | session/message/event/checkpoint transaction adapters |
| subagent memory store | durable relationship/budget/wake store |
| live MCP only | persisted generation manifest 与 exact restoration |

## Durable Guarantees

SDK 不承诺天然 exactly-once：

- 模型调用在模糊恢复后可能重复。
- tool effect 默认可能重复。
- effectively-once 需要 downstream 使用 `ExecutionKey` 去重。
- non-replayable ambiguous effect 必须进入 unknown/operator-required，而不是盲目重试。

## Recommended Host

服务化后建议：

```text
app lifecycle
  -> coordinator immutable generation
  -> session admission / workers
  -> durable checkpoint + fences
  -> message/session transaction
  -> permission blockers
  -> event outbox
  -> MCP / skills generation leases
  -> subagent child runs
```

每个 persisted operation 必须包含 `agent.TenantKey`，adapter 负责 tenant 映射与数据库约束。SDK 不解析产品 tenant 格式。

## Verification

```bash
go test ./... -count=1
cd demo/icoder
go test ./... -count=1
go vet ./...
```

## 发布前检查

- 固定 module version，不依赖浮动分支。
- Provider adapter 通过 conformance 和 cancellation 测试。
- 每个工具声明 replay policy，并限制输入、输出和 timeout。
- Prompt、definition、skills、MCP 和 tokenizer 使用可追踪版本。
- Persisted operation 带 tenant scope、request idempotency、revision 和必要 fence。
- Observation 不参与权威状态判断。
- Event outbox 与 aggregate transaction 原子提交。
- Shutdown 停止 admission，等待有界 drain，再关闭依赖。
- 日志不记录 credential、完整敏感 Prompt 或未经处理的工具输出。
