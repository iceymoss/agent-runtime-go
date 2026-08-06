# 子包职责与接入指南

所有子包都位于 `github.com/iceymoss/agent-runtime-go/<name>`。根包只提供无状态执行内核；子包按需组合，不存在一个必须全部启用的“大框架”。本章覆盖仓库中的全部实际子包。

## 总览与常见陷阱

| 子包 | 主要 owner |
|---|---|
| `app` | 应用 readiness、admission 和有界 shutdown |
| `agenttest` | model/tool adapter 合约测试 |
| `context` | revision-bound 上下文 plan、规范化和压缩 artifact |
| `coordinator` | selector 到不可变 runtime generation 的解析与 manifest |
| `durable` | 生产 run/lease/fence/effect/usage 权威状态与 reconcile |
| `event` | 可靠事件、outbox、dispatch、replay，以及独立的 best-effort bus |
| `mcp` | tenant-scoped MCP live connection generation |
| `message` | 持久化消息聚合、revision/fence 与可见性 |
| `permission` | effect 授权、审批、grant、恢复重校验 |
| `prompt` | 无副作用模板编译、渲染和版本 |
| `provider` | provider/model catalog generation 与 model factory |
| `session` | 会话/分支聚合，以及队列、worker、合并宿主 |
| `skills` | 不可信 instruction/artifact catalog generation |
| `subagent` | 独立 durable child run、预算、取消和 wake |
| `tool` | 生产工具 generation、拦截器、权限和 effect lifecycle |

先明确四个容易误接的边界：

1. 根包 `agent.CheckpointStore` 和子包 `durable.Store` 是两套 durable API。前者由根包 runner 直接驱动；后者是生产宿主的权威 lease/effect/usage 模型。它们不满足彼此接口，需要 adapter 或选定一套 owner。
2. 根包 `agent.Registry`/`agent.ToolSet` 和子包 `tool.Registry`/`tool.Generation` 是两层工具系统。前者负责模型可见定义、schema、白名单和根包工具循环；后者负责版本化 generation、interceptor、permission 和 execution ledger。
3. 根包 `agent.MemoryStore`、`session.Memory`、`session.MemoryRunStore`、`message.Memory` 都是不同接口的内存实现，不能因为都叫 Memory 就互换。
4. 根包 `ObservationEmitter` 和 `event.Bus` 都是 best-effort；`event.Store` 才是可靠事件/outbox 边界。Observation 绝不能驱动权威终态。

## `app`

**职责：** 编排组件依赖、required/optional readiness、admission 开关，以及 drain、cancel、checkpoint、wait、flush、close 的有界关停顺序。

**关键符号：** `Component`、`ComponentHealth`、`Reporter`、`Dependencies`、`RunController`、`Checkpointer`、`Flusher`、`Budgets`、`App`、`New`、`Start`、`WaitReady`、`StopAdmission`、`Shutdown`。

**接入方式：** 把 provider catalog、MCP、skills 等包装为 `Component`；把 `session.SessionAgent`、durable reconciler 和 event dispatcher 包装为三个窄依赖端口。组件按拓扑序启动、逆序关闭，required 未 ready 时不开放 admission。

**边界：** `app` 不执行 Agent，不拥有 checkpoint/event 数据，也不自动把其他包类型适配过来。所有 budget 必须为正；shutdown 发现 run 泄漏时会跳过不安全的 component close。

**小例子：**

```go
application, err := app.New(app.Config{Budgets: app.Budgets{
	Startup: 20 * time.Second, StartupRollback: 10 * time.Second,
	Drain: 30 * time.Second, Cancel: 10 * time.Second,
	Checkpoint: 10 * time.Second, Wait: 10 * time.Second,
	Flush: 10 * time.Second, Close: 10 * time.Second,
}}, app.Dependencies{Runs: runs, Checkpoints: checkpoints, Events: events}, components...)
if err != nil { return err }
if err := application.Start(ctx); err != nil { return err }
defer application.Shutdown(context.Background())
```

## `agenttest`

**职责：** 对外部 adapter 运行可复用的公共合约测试。

**关键符号：** `TestModel`、`ModelFactory`、`ModelCase*`、`TestTool`、`ToolCase`。

**接入方式：** provider adapter 测试按 `ModelCase` 构造对应 fake upstream；工具测试提供 invocation/result/error case。

**边界：** 这是测试包，生产包按仓库依赖规则不得 import 它。`TestModel` 的 valid fixture预期完整 tools/tool-choice/usage-details capability，且检查流关闭、错误归一化和 cancellation。

**小例子：**

```go
func TestProviderAdapter(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, c agenttest.ModelCase) agent.Model {
		return newFixtureBackedModel(t, c)
	})
}
```

## `context`

**职责：** 规范化消息历史、验证 tool call/result 配对，按精确 session revision 和 artifact generation 构造不可变 `Plan`，并通过 summary artifact 压缩安全前缀。

**关键符号：** `NormalizeHistory`、`RepairPolicy`、`SourceRef`、`Budget`、`TokenCounter`、`Plan`/`PlanRef`、`Planner`、`NewPlanner`、`PrepareRequest`、`ProtectedFact`/`NewFactSet`、`Compactor`/`NewCompactor`、`PivotRef`、`PlanStore`、`ArtifactStore`。

**接入方式：** 实现与 provider/model/version 精确绑定的 `TokenCounter`，用 `NewPlanner(counter, store)`；从 session/message owner 读固定 revision，填 `PrepareRequest`，再把 `plan.Messages()` 交根包 runner。

**边界：** 不拥有 session/message storage，不维护“当前 pivot”，不自行调用模型。`Budget.InputLimit()` 只扣输出和 safety margin，最终 estimate 还会加 tool schema/media tokens。超限返回 compaction-required 或 context-too-large，而不是静默截断。

**小例子：**

```go
planner, err := agentcontext.NewPlanner(counter, agentcontext.NewMemoryStore())
if err != nil { return err }
plan, err := planner.Prepare(ctx, agentcontext.PrepareRequest{
	Source: agentcontext.SourceRef{TenantKey: "tenant-a", SessionKey: "s1", SessionRevision: 7},
	Runtime: agentcontext.RuntimeArtifacts{
		DefinitionDigest: definitionDigest, ProjectionVersion: "messages/v1", TokenizerID: counter.ID(),
		SystemMessages: []agent.Message{agent.NewSystemMessage("遵循订单政策。")},
	},
	MainlineMessages: history,
	InvocationMessages: []agent.Message{agent.NewUserMessage("查询订单")},
	Budget: agentcontext.Budget{ContextTokens: 32_000, ReservedOutputTokens: 2_000, SafetyMarginTokens: 1_000},
})
if err != nil { return err }
result, err := runner.Run(ctx, agent.RunRequest{Messages: plan.Messages()})
```

## `coordinator`

**职责：** 把 tenant-scoped selector 解析为不可变、可执行的 `RuntimeDefinition` generation；只持久化 canonical value manifest，并按精确 generation 重建。

**关键符号：** `Selector`、`BuildRequest`、`Builder`、`Reconstructor`、`ManifestWire`、`ArtifactManifest`、`NewArtifactManifest`、`ManifestStore`、`ResolvedRuntime`、`Coordinator`、`New`、`Resolve`、`ResolveGeneration`。

**接入方式：** 应用实现 `Builder` 组装 provider、prompt、root tools、skills/MCP artifact；实现 `Reconstructor` 按 manifest 中 tenant+generation+digest 精确恢复；提供 durable `ManifestStore`。

**边界：** `RuntimeDefinition` 和回调函数永不序列化。manifest 不允许 executable `StopConditions`，应用用 policy version 重建。`MemoryManifestStore` 适合测试，不是跨进程 artifact registry。

**小例子：**

```go
coord, err := coordinator.New(coordinator.Options{
	Builder: builder, Reconstructor: reconstructor,
	Artifacts: coordinator.NewMemoryManifestStore(),
})
if err != nil { return err }
runtime, err := coord.Resolve(ctx, coordinator.ResolveRequest{
	Selector: coordinator.Selector{AgentKey: "support", TenantKey: "tenant-a"},
	ModelRole: provider.RolePrimary,
})
if err != nil { return err }
runner, err := runtime.Definition.NewAgent()
```

## `durable`

**职责：** 提供生产宿主用的 run snapshot、lease/fence/revision、effect ledger、usage ledger、扫描与 reconcile 协议。

**关键符号：** `Snapshot`、`Identity`、`Guard`、`Store`、`ExecutionLedger`、`UsageLedger`、`EffectRecord`、`UsageFact`、`NewReconciler`、`ReconcileBatch`、`MarshalSnapshot`、`UnmarshalSnapshot`、`NewMemoryStore`。

**接入方式：** 数据库 adapter 应原子实现 `Store.Save` 的 owner+fence+revision CAS、`RevokeLease` 的 fence 推进和 running effect unknown 标记；effect/usage ledger 可以由同一个物理数据库实现。reconciler 扫描过期 run 并恢复或转 unknown/operator-required。

**边界：** 这是与根包 checkpoint 协议并列的第二套 durable 模型，不是 `agent.CheckpointStore` 的实现。它使用自己的 `Snapshot`、`Guard`、`Status`、`Phase`，虽然部分 checkpoint/input/config 类型 alias 到根包。`NewMemoryStore` 同时实现 store、effect、usage 接口，但只用于测试和嵌入。

**小例子：**

```go
store := durable.NewMemoryStore()
snapshot, created, err := store.Begin(ctx, durable.BeginRequest{
	Identity: durable.Identity{RunKey: "run-1", AgentKey: "support", SessionID: "s1", RequestID: "r1"},
	InputDigest: inputDigest, ConfigDigest: configDigest,
	Checkpoint: agent.Checkpoint{History: input},
})
if err != nil { return err }
_ = created
snapshot, err = store.Acquire(ctx, durable.AcquireRequest{
	RunKey: durable.RunKey(snapshot.Identity.RunKey), Owner: "worker-a",
	Now: time.Now(), LeaseUntil: time.Now().Add(30 * time.Second),
})
```

## `event`

**职责：** 定义 tenant-scoped 可靠事件 envelope、事务内 append、outbox lease/ack/nack、dispatcher、consumer inbox 和 replay gap/reset；另提供独立的 best-effort observation bus。

**关键符号：** `Envelope`、`Reliability*`、`Store`、`AppendBatchCommand`、`ClaimCommand`、`AckCommand`、`NackCommand`、`ReplayQuery`/`ReplayResult`、`Dispatcher`、`NewDispatcher`、`Inbox`、`NewInbox`、`Bus`、`NewBus`。

**接入方式：** 聚合数据库 adapter 在业务事务内实现 `AppendBatch`；后台 `Dispatcher.RunOnce` claim 后 publish，并按结果 ack/nack；consumer 用持久化 inbox 去重，SDK `Inbox` 只提供进程内去重参考。

**边界：** delivery 是 at-least-once，publisher 和 consumer 必须幂等。replay 遇到 retention gap 不会静默跳过，而要求 authoritative snapshot reconciliation。`Bus` 仅接受 `ReliabilityObservation`，满时返回 false 且不重试。

**小例子：**

```go
stored, err := events.Append(ctx, event.AppendCommand{Envelope: event.Envelope{
	TenantKey: "tenant-a", EventID: "evt-1", StreamKey: "session:s1",
	Type: "session.completed", SchemaVersion: 1,
	Reliability: event.ReliabilityTerminal,
	AggregateType: "session", AggregateKey: "s1", AggregateRevision: 8,
	OccurredAt: time.Now(), Payload: []byte(`{"session":"s1"}`),
}})
if err != nil { return err }
_ = stored // Sequence/PersistedAt 由 store 分配
```

## `mcp`

**职责：** 管理每 tenant 的 MCP stdio/streamable HTTP 连接、初始化、工具发现、canonical tool names、live generation lease、超时和结果大小限制。

**关键符号：** `ConfigSource`、`ServerConfig`、`Connector`/`Client`、`Manager`、`NewManager`、`Snapshot`、`GenerationLease`、`ToolDefinition.AgentDefinition`、`ToolCall`、`StdioConnector`、`HTTPConnector`。

**接入方式：** 实现 config/secret/transport adapter，`StartScope` 后等待 ready；构建 runtime 时固定 `Snapshot.Generation` 和工具定义；执行时 `AcquireGeneration`，使用结束后 `Close` lease。

**边界：** manager 只拥有 live transport generation，不持久化历史 generation manifest。进程重启后的 exact restoration 必须由 coordinator/application 保存和验证 artifact，再确保对应 MCP generation 可重建。MCP 工具不会自动进入根包 registry 或生产 `tool.Registry`。

**小例子：**

```go
manager, err := mcp.NewManager(configSource, connector)
if err != nil { return err }
scope := mcp.Scope{TenantKey: "tenant-a"}
if err := manager.StartScope(ctx, scope); err != nil { return err }
snapshot, ok := manager.Snapshot(scope)
if !ok { return errors.New("MCP generation unavailable") }
lease, err := manager.AcquireGeneration(ctx, scope, snapshot.Generation)
if err != nil { return err }
defer lease.Close()
result, err := lease.CallTool(ctx, mcp.ToolCall{
	Scope: scope, Generation: snapshot.Generation, ServerID: "crm", Name: "lookup",
	Arguments: map[string]any{"id": "42"},
})
```

## `message`

**职责：** 持久化单条消息的累计 snapshot，处理 branch ordinal、streaming/building 到终态的 revision CAS、fence、可见 revision 和 tombstone。

**关键符号：** `MessageKey`、`State`、`Snapshot`、`CreateCommand`、`SaveCommand`、`TombstoneCommand`、`MutationFact`、`Service`、`NewMemory`。

**接入方式：** session/attempt owner 在相同事务中创建或更新消息，传 stable key、attempt、fence、expected revision；`ListVisible` 用固定 session revision 读取一致视图。

**边界：** 它不是根包 `MessageStore` 的替代签名，而是更严格的聚合端口。`SaveSnapshot` 替换全部可变累计字段，不是追加 delta。可靠事件需由数据库 adapter 根据 `MutationFact` 在 owning transaction 中写 outbox。

**小例子：**

```go
messages := message.NewMemory()
snapshot, err := messages.Create(ctx, message.CreateCommand{
	TenantKey: "tenant-a", MessageKey: "msg-1", SessionKey: "s1", BranchKey: "main",
	Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartText, Text: "处理中"}},
	State: message.StateBuilding, RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 3,
	MutationMeta: message.MutationMeta{OccurredAt: time.Now()},
})
if err != nil { return err }
snapshot, err = messages.SaveSnapshot(ctx, message.SaveCommand{
	TenantKey: "tenant-a", MessageKey: snapshot.MessageKey, ExpectedRevision: snapshot.Revision,
	AttemptKey: "attempt-1", FenceToken: 3, State: message.StateComplete,
	FinishReason: agent.FinishStop,
	Parts: []agent.ContentPart{{Type: agent.PartText, Text: "处理完成"}},
	MutationMeta: message.MutationMeta{OccurredAt: time.Now()},
})
```

## `permission`

**职责：** 对规范化后的精确 effect 做 allow/deny/ask 决策，持久化 approval、resolution 和 grant，并在恢复时校验 resume token、input/policy/tool generation 与 fence。

**关键符号：** `Policy`/`PolicyFunc`、`CheckRequest`/`CheckResult`、`Service`、`Store`、`FenceValidator`、`ApprovalRequest`、`SuspensionBlocker`、`Grant`、`ResolveCommand`、`RevalidateCommand`、`NewService`、`NewMemoryStore`。

**接入方式：** 提供 policy、durable store、clock 和连接 durable owner 的 `FenceValidator`；生产 `tool.Executor` 会用 `PreparedExecution` 自动构造 `CheckRequest`。ask 时保存 blocker 并暂停 run，批准后 resume 前必须 `Revalidate`。

**边界：** 权限检查必须发生在 interceptor rewrite 和 canonical input 之后，否则批准的不是实际 effect。permission 不 import durable 包，fence bridge 由应用实现。内存 store 不适合跨进程审批。

**小例子：**

```go
policy := permission.PolicyFunc{PolicyVersion: "policy/v2", EvaluateFunc:
	func(ctx context.Context, req permission.CheckRequest, grants []permission.Grant) (permission.CheckResult, error) {
		if req.ToolName == "delete_file" {
			return permission.CheckResult{Decision: permission.DecisionAsk, PolicyVersion: "policy/v2", InputDigest: req.InputDigest}, nil
		}
		return permission.CheckResult{Decision: permission.DecisionAllow, PolicyVersion: "policy/v2", InputDigest: req.InputDigest}, nil
	},
}
service, err := permission.NewService(permission.ServiceOptions{
	Policy: policy, Store: permission.NewMemoryStore(), Fences: fenceBridge,
})
```

## `prompt`

**职责：** 使用 `text/template` 预编译并纯函数渲染 prompt，同时产生确定性版本摘要。

**关键符号：** `Prompt`、`New`、`Name`、`Source`、`Version`、`Render`、`ErrInvalid`。

**接入方式：** 启动/构建 generation 时编译，在请求前渲染为 system message，把 `Version()` 写入 `RuntimeDefinitionSpec.PromptVersion` 或 manifest。

**边界：** `missingkey=error`；不读取文件、不缓存远程内容、不做 tenant 隔离。输入可信度和模板 source 的发布流程由应用负责。

**小例子：**

```go
p, err := prompt.New("support", "你是 {{.Product}} 的支持助手。")
if err != nil { return err }
text, err := p.Render(struct{ Product string }{"Acme"})
system := agent.NewSystemMessage(text)
_ = p.Version()
```

## `providers/openaicompat`

**职责：** 官方 OpenAI-compatible 模型适配器。将 `GenerateRequest` 投影为 `/chat/completions` 协议（默认 SSE 流式），把上游增量拼装为 canonical `StreamChunk`，归一化 usage（含 cache/reasoning tokens），并把失败分类为 `agent.ModelError`。

**关键符号：** `New`、`Model`、`WithName`、`WithHTTPClient`、`WithCapabilities`、`WithHeader`、`WithoutStreaming`、`WithoutStreamUsage`。

**接入方式：** `openaicompat.New(baseURL, apiKey)` 直接得到根包 `agent.Model`，适用于 OpenAI、DeepSeek、Qwen、Kimi、vLLM、Ollama 等端点。本地服务可传空 apiKey。

**边界：** 只覆盖 Chat Completions 协议；暂不支持图片输入。Anthropic Messages 等其他协议仍需自行实现 `Model`。

**小例子：**

```go
model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))
runner, err := agent.New(agent.Config{
	Key: "assistant.support", ModelName: "deepseek-chat", MaxSteps: 8,
}, model, registry)
```

## `provider`

**职责：** 定义 tenant-scoped provider/model catalog generation、model metadata、capability、默认参数、价格版本和创建 `agent.Model` 的 factory。

**关键符号：** `ProviderID`、`ModelID`、`Role`、`ModelDescriptor`、`ProviderDescriptor`、`CatalogSnapshot`、`NewCatalogSnapshot`、`CatalogSource`、`Factory`、`BuildRequest`、`MemoryCatalog`。

**接入方式：** 配置层发布 immutable catalog；coordinator 按 selector/role 选择 descriptor，再调用 `Factory.Build` 注入 tenant credential 并创建根包 `Model` adapter。

**边界：** provider 包本身不实现任何厂商协议、不保存 credential，也不做 model routing。factory 返回 model 后仍必须保证其 capability 与 descriptor 一致。价格使用十进制字符串和显式 version，避免浮点计费漂移。

**小例子：**

```go
catalog := provider.NewMemoryCatalog()
snapshot, err := provider.NewCatalogSnapshot("catalog-2026-08", time.Now(), []provider.ProviderDescriptor{{
	ID: "vendor", Version: "adapter/v3", Models: []provider.ModelDescriptor{{
		Ref: provider.ModelRef{Provider: "vendor", Model: "large"}, Version: "2026-07-01",
		ContextWindow: 128_000, DefaultMaxTokens: 4_096,
		Capabilities: agent.Capabilities{Tools: true},
	}},
}})
if err != nil { return err }
err = catalog.Publish(ctx, provider.Scope{TenantKey: "tenant-a"}, snapshot)
```

## `session`

**职责：** 包含两部分：session/branch 权威聚合，以及 admission、queue、worker、attempt、fast-forward merge 的 `SessionAgent` 宿主。

**关键符号：** 聚合层的 `Snapshot`、`Branch`、`Service`、`CreateBranch`、`CommitMerge`、`NewMemory`；宿主层的 `SessionAgent`、`Options`、`DefinitionResolver`、`AttemptRunner`、`Store`、`RunRequest`、`RunReceipt`、`RunNext`、`StartWorkers`、`Drain`、`Shutdown`、`NewMemoryRunStore`；另有可持久化 `StepPolicyArtifact`。

**接入方式：** 用 coordinator adapter 实现 `DefinitionResolver`，用 durable/root runner adapter 实现 `AttemptRunner`，数据库实现 host `Store` 的 admission+begin 和 merge+finalize 事务。`Run` 先持久化 admission 再返回 receipt，worker 与调用者 HTTP context 解耦。

**边界：** `session.Service` 与 host `Store` 是同包内两套不同端口。`Run` 是排队，不等于完成；用 `Await`/`Get`。只有 fast-forward merge，base revision 漂移会冲突。`EventSink` 明确是 post-commit 通知，不承担事务内 outbox 写入。

**小例子：**

```go
host, err := session.New(session.Options{
	Store: session.NewMemoryRunStore(), Definitions: resolver, Attempts: attempts,
	WorkerID: "worker-a", LeaseDuration: 30 * time.Second,
	Limits: session.Limits{MaxActiveGlobal: 32, MaxActivePerTenant: 8, MaxActivePerSession: 1, MaxQueuedPerTenant: 100, MaxQueuedPerSession: 10},
})
if err != nil { return err }
receipt, err := host.Run(ctx, session.RunRequest{
	TenantKey: "tenant-a", SessionKey: "s1", RequestID: "request-1",
	AgentKey: "support", Messages: []agent.Message{agent.NewUserMessage("你好")},
	Merge: session.MergeFastForward,
})
if err != nil { return err }
result, err := host.Await(ctx, receipt.RunKey)
```

## `skills`

**职责：** 聚合 platform filesystem、tenant filesystem/DB 中的 skill descriptor、instructions 和 artifacts，发布 tenant-scoped immutable generation，并通过 lease 保留旧 generation。

**关键符号：** `Descriptor`、`Source`、`FilesystemSource`、`RepositorySource`、`Catalog`、`Manager`、`NewCatalog`、`Snapshot`、`Selection`、`Resolve`、`Read`、`Acquire`/`Lease`、`TrustLevel`、`Limits`。

**接入方式：** 注册 required/optional source 和允许的 trust level；`StartScope`/`Refresh` 后固定 generation，`Resolve` 选择 descriptor，再按需 `Read` 内容；运行期间持有 lease。

**边界：** skill 内容始终是 `untrusted_instructions` 或 `untrusted_artifact`，包不会执行内容，也不会因 `ToolRequirements` 自动授予工具。source precedence/replacement 必须显式；filesystem 实现会限制 traversal、symlink、大小和 UTF-8。

**小例子：**

```go
catalog, err := skills.NewCatalog(skills.Options{
	Sources: []skills.SourceRegistration{{Source: platformSource, Required: true}},
	AllowedTrust: []skills.TrustLevel{skills.TrustPlatform},
	Limits: skills.DefaultLimits(),
})
if err != nil { return err }
scope := skills.Scope{TenantKey: "tenant-a"}
if err := catalog.StartScope(ctx, scope); err != nil { return err }
snapshot, _ := catalog.Current(scope)
lease, err := catalog.Acquire(scope, snapshot.Generation)
if err != nil { return err }
defer lease.Release()
selection, err := catalog.Resolve(skills.ResolveRequest{Scope: scope, Generation: snapshot.Generation, Selectors: selectors})
```

## `subagent`

**职责：** 编排与父 run 分离的 durable child runs，原子管理 relationship、tree budget、parent blocker、取消传播、usage settlement 和 commit-before-wake。

**关键符号：** `ParentRef`、`ChildRef`、`SpawnRequest`/`SpawnReceipt`、`Limits`、`Reservation`、`Runner`、`ParentWaker`、`Store`、`Service`、`New`、`Spawn`、`RunNext`、`Cancel`、`SettleUsage`、`Reconcile`。

**接入方式：** 应用实现 child `Runner`，通常调用 session host；实现幂等 `ParentWaker`；数据库 store 在一个事务里保存 child terminal 和 wake intent，后台重试 wake。

**边界：** subagent 不是根包工具调用的 goroutine，也不共享父 run 内存 history。parent/child 各有独立 session/run/attempt；预算 reservation 与最终 usage 要结算。`Wake` 必须按 `WakeKey` 幂等。

**小例子：**

```go
svc, err := subagent.New(subagent.Options{
	Store: subagent.NewMemoryStore(), Runner: childRunner, ParentWaker: waker,
	WorkerID: "child-worker", LeaseDuration: 30 * time.Second,
})
if err != nil { return err }
receipt, err := svc.Spawn(ctx, subagent.SpawnRequest{
	RequestKey: "spawn-1",
	Parent: subagent.ParentRef{TenantKey: "tenant-a", SessionKey: "s1", RunKey: "parent", AttemptKey: "a1", Fence: 4, TreeKey: "tree-1"},
	AgentKey: "researcher", Input: []byte(`{"question":"..."}`),
	Limits: subagent.Limits{MaxDepth: 3, MaxFanout: 4, MaxRuntime: time.Minute},
	Reserve: subagent.Reservation{Runtime: 30 * time.Second},
})
```

## `tool`

**职责：** 为本地工具提供生产 lifecycle：冻结 generation、preflight interceptor/rewrite、canonical input、permission、effect ledger、fence、panic/error 分类和非阻塞 observation。

**关键符号：** `Registry`、`Metadata`、`Generation`、`Freeze`、`Interceptor`、`PreparedExecution`、`ExecutionLedger`、`Executor`、`NewExecutor`、`ExecuteRequest`/`ExecuteResult`、`ClassifiedError`。

**接入方式：** 用 `tool.Registry.Register(agent.Tool, Metadata)` 注册实现并 `Freeze`；把 durable effect owner 适配成 `tool.ExecutionLedger`，注入 `permission.Service`，由 attempt runner 为每次调用构造完整 `InvocationIdentity`。

**边界：** 这是第二层工具 lifecycle，不会被根包 `Agent` 自动调用。若要同时用根包循环，可把 `Generation` 中的 frozen tools 注册到根包 registry，或提供一个实现 `agent.Tool`、内部调用 `Executor` 的桥；必须确保只有一个账本 owner。`ExecutionLedger` 注释明确它是 consumer port，不是第二份持久化权威记录。

**小例子：**

```go
registry := tool.NewRegistry()
err := registry.Register(deleteFileTool, tool.Metadata{
	Version: "delete/v2", SchemaVersion: "schema/v1", Action: "file.delete",
	EffectGroup: "workspace", EffectClass: tool.EffectWrite,
	Idempotency: tool.IdempotencyExecutionKey,
	Concurrency: tool.ConcurrencyExclusive,
	ReplayPolicy: agent.ReplayPolicyIdempotent,
})
if err != nil { return err }
generation, err := registry.Freeze(auditInterceptor)
if err != nil { return err }
executor, err := tool.NewExecutor(tool.ExecutorOptions{
	Generation: generation, Ledger: ledgerAdapter, Permission: permissionService,
})
if err != nil { return err }
execution, err := executor.Execute(ctx, tool.ExecuteRequest{Invocation: tool.InvocationIdentity{
	TenantKey: "tenant-a", RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 5,
	CallID: "call-1", ToolName: "delete_file", RawInput: `{"path":"tmp.txt"}`,
	PrincipalKey: "user-7", SessionRef: "s1",
}})
if errors.Is(err, tool.ErrApprovalPending) {
	// 持久化 execution.Blocker 后暂停父 run。
}
```

## 组合原则

- 先确定权威 owner：session、message、durable effect、approval、event 各只有一份可写记录。
- generation 一经用于 run 就按精确 digest 恢复，不用“当前配置”替代历史配置。
- `agent.TenantKey` 必须贯穿所有持久化操作；opaque key 不应在 SDK adapter 中被解析。
- 所有内存实现都适合测试和单进程演示，不等价于生产 durability。
- 跨 package 原子性由应用数据库 adapter 实现，不能靠依次调用两个内存 service 推导出来。
