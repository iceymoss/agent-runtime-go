# 7. 危险操作要问人

## 你现在遇到的问题

工具能查天气是一回事，能删文件、能转账、能给客户发邮件是另一回事。你需要某些操作在执行前停下来等人点头，而且要能记下"谁在什么时候批准了什么"。

## 权限只能在工具里做实

先说最重要的：**prompt、skills、模型输出都不是权限边界**。system prompt 里写"未经确认不要删除文件"拦不住任何东西——模型是概率的，而且用户输入可以覆盖它。

真正的检查必须在 `Execute` 里，或者在你的适配层：

```go
func (t *deleteTool) Execute(ctx context.Context, in agent.ToolInvocation) (agent.ToolResult, error) {
	path, err := t.workspace.Resolve(in.Path) // 越界直接拒绝，不是提示模型别越界
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "路径超出工作区"}, nil
	}
	...
}
```

## 让运行停下来等人

`permission` 子包把"允许 / 拒绝 / 问一下"做成可持久化的决策，并且能跨进程回答——CLI 里发起的审批，可以由 Web 界面上的人批准。

```go
import "github.com/iceymoss/agent-runtime-go/permission"
```

判定结果是 `ask` 时，工具返回一个挂起，运行时会把整轮运行以 `suspended` + `tool_suspended` 停下并落 checkpoint。人做出决定后，用同一个 run key 恢复，工具带着审批结果继续执行。

挂起不只用于审批。等待外部事件——子 agent 跑完、webhook 回调、排队任务——都用同一套机制，只是 `ToolSuspensionKind` 不同。

## 副作用只发生一次

一旦运行可以中断和恢复，就有了新问题：崩溃前那次转账到底执行了没有？

`tool` 子包提供完整的执行生命周期和副作用台账：每次调用先登记（prepare）、再执行、最后记录结果，配合稳定的 `ToolExecutionKey` 让下游做幂等去重。有副作用的工具还要用 `WithToolReplayPolicy` 显式声明重放语义。

```go
import "github.com/iceymoss/agent-runtime-go/tool"
```

运行时**不保证**外部副作用 exactly-once——它保证的是给你一个稳定的去重锚点，真正的幂等要下游系统配合。

只做只读工具的应用不需要这一层。

## 深入

- [permission](../packages/permission.md) —— 策略、审批、授权重校验
- [tool](../packages/tool.md) —— 执行生命周期、effect ledger、与根包 Registry 的桥接
