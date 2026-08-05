# Prompt、Skills 与 Context

三者都参与模型输入，但所有权不同：Prompt 是可信应用策略，Skills 是非可信动态说明，Context 是本次调用经过预算处理的消息。

## Prompt

`prompt` 编译不可变 Go text template：

```go
compiled, err := prompt.New("code-agent/v1", source)
rendered, err := compiled.Render(data)
version := compiled.Version()
```

模板使用 `missingkey=error`。Prompt source、选择策略和模板数据属于应用层。

## Skills

`skills` 管理 tenant-scoped、不可变、非可信的 instruction/artifact catalog。Skill 不会执行代码，也不会授予工具权限。

```go
source := skills.NewFilesystemSource(skills.FilesystemOptions{
    Name: "local-skills",
    Kind: skills.SourcePlatformFilesystem,
    Root: root,
})
catalog, err := skills.NewCatalog(skills.Options{
    Sources: []skills.SourceRegistration{{Source: source, Required: true}},
})
err = catalog.StartScope(ctx, skills.Scope{TenantKey: tenant})
```

先固定 catalog generation，再 Resolve descriptor 和 Read exact version。将内容放入明确的 `<untrusted-skill>` 边界。`ToolRequirements` 是 metadata，应用仍需验证并注册实际工具。

iCoder 附带两个 skill：

- `skills/code-review`：通用代码审查。
- `skills/go-code-review`：面向 Go 的 correctness、concurrency、resource lifecycle、CAS/fence 和测试审查流程。

运行时通过 `--skills ./skills` 加载它们。示例为简化流程加载 catalog 中的全部 descriptor；产品通常应根据用户选择、Agent recipe 或 capability policy 显式 Resolve selectors。

## Context

先归一化持久化历史：

```go
normalized, err := agentcontext.NormalizeHistory(agentcontext.NormalizeRequest{
    Messages: history,
    Policy:   agentcontext.RepairReject,
})
```

再建立 revision-bound plan：

```go
planner, err := agentcontext.NewPlanner(counter, planStore)
plan, err := planner.Prepare(ctx, agentcontext.PrepareRequest{
    Source: agentcontext.SourceRef{TenantKey: tenant, SessionKey: key, SessionRevision: revision},
    Runtime: agentcontext.RuntimeArtifacts{DefinitionDigest: digest, TokenizerID: counter.ID()},
    MainlineMessages: normalized.Messages,
    InvocationMessages: []agent.Message{agent.NewUserMessage(input)},
    Budget: budget,
})
```

`TokenCounter` 是 consumer port。iCoder 的 byte counter 只是保守 demo；生产实现应绑定 provider/model/tokenizer 版本。工具 schema 和 media token 需要调用方纳入预算。

根 runtime 不会在每个 tool step 重新调用 planner。工具输出必须限制大小，后续步骤由 provider usage 和 `ContextWindow` 兜底。

## 推荐顺序

```text
select immutable prompt version
  -> resolve exact skill generation
  -> normalize persisted history
  -> prepare a revision-bound context plan
  -> append current user message
  -> call root runtime
```

不要把 Skill 文本拼进 system prompt 后就视为可信，也不要让 compaction 摘要改变工具调用 ID、权限事实或未完成的交互状态。
