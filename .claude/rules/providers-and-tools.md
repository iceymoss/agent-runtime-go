## Model 适配器

- 只实现 `Model` 三个方法：`Name()` / `Capabilities()` / `Stream()`。适配器负责协议投影，不承载业务逻辑
- 上游失败必须分类成 `agent.ModelError`（kind、retryable、httpStatus、retryAfter、safeDetail）。`safeDetail` 不得包含 API key、完整 URL、请求体或堆栈
- 流式规则：只发送规范化的 `StreamChunk`；finish 之后不得再发任何 chunk；tool call 分片必须在适配器内部拼装完整后再交给运行时
- usage 要归一化（含 cache token）；拿不到的字段留零值，不要臆造或估算
- 能力差异通过 `Capabilities()` 声明（图片输入、tool choice 等），让运行时在装配期就拒绝非法请求，而不是运行到一半才报错
- 凭据只能由调用方显式传入，适配器不读环境变量、不查配置文件
- OpenAI 兼容端点优先复用 `providers/openaicompat` + Option（`WithoutStreaming`、`WithCapabilities`、`WithHeader`、`WithHTTPClient`），不要另写一个近似实现
- 新适配器必须跑通 `agenttest.TestModel` 一致性套件

## Tool

- 首选 `agent.NewTool[T]` / `MustNewTool[T]`，由输入结构体反射生成 JSON Schema：非指针且无 `omitempty` 的字段自动进 `required`，默认 strict（拒绝未声明属性）。确有需要才用 `WithoutStrictSchema()`
- 只有需要完全掌控 schema 时才直接实现 `Tool` 接口；schema 不得引用外部 `$ref`（registry 会拒绝），本地递归 `$ref` 可以
- 工具内部区分两类失败：模型可以改正的（参数不对、资源不存在）返回 `ToolResult{IsError: true}`；不可恢复的返回 Go error
- 有副作用的工具用 `WithToolReplayPolicy` 显式声明重放语义，并用 `ToolExecutionKeyFromContext` 做幂等去重
- 需要外部审批或等待时返回 `ToolSuspension`，让运行时挂起；不要在工具里阻塞等待或自己起后台轮询
- 工具名在 registry 内唯一且长期稳定，改名等于破坏性变更
- 权限必须在工具实现里做实（工作区路径限制、命令白名单、owner 校验）。prompt 和 skills 约束不算数
- 新工具要跑 `agenttest.TestTool`，覆盖合法输入、非法输入和错误路径
