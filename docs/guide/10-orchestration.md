# 10. 多 Agent 与可复现

## 你现在遇到的问题

两件事会同时出现：一是有些任务该交给一个独立的 agent 去做（让一个只读的 reviewer 去审代码，别把它的搜索过程塞进主对话的上下文）；二是三个月后有人问"上周那次运行到底用的什么模型、什么 prompt、什么工具集"，你答不上来。

## 委派给子 Agent

`subagent` 子包管理独立的 child run：spawn、claim、完成、唤醒父运行，以及深度、扇出、环检测和树预算：

```go
import "github.com/iceymoss/agent-runtime-go/subagent"
```

父运行拿到的是**结构化结果**，不是 child 的完整历史——这正是委派的价值，child 花掉的上下文不会污染父对话。

深度、扇出、预算是安全限额而不是记账：没有它们，一个能委派的 agent 可以委派出一棵无限深的树，把配额烧光。库把这些判定导出成纯函数（`ValidateDepth`、`ValidateFanout`、`ValidateNoCycle`、`BudgetSnapshot.Reserve`），存储适配器应该调用它们而不是自己重写一份——重写一份不会大声报错，只会悄悄放宽某个你从未授权的限额。

child 跑起来需要时间，所以委派通常是**异步**的：工具 spawn 之后返回一个 `ToolSuspensionExternal` 让父运行挂起（第 7 章的挂起机制），child 完成后凭 handle 恢复。这样进程在 child 执行期间崩溃，父运行仍然可以从 checkpoint 恢复。

## 让一次运行可复现

`coordinator` 把"模型 + prompt + 工具集 + 执行设置 + 策略"组合成一个**不可变的 RuntimeDefinition**，并算出一个 digest：

```go
import "github.com/iceymoss/agent-runtime-go/coordinator"
```

这个 digest 就是那次运行的身份。记下它，以后可以把同一个组合重新解析出来，验证当前二进制是否还能复现它。审计和排查线上问题时，"这次运行用的是哪一版"有了确定的答案。

## 深入

- [subagent](../packages/subagent.md) —— 生命周期、树预算、取消、唤醒
- [coordinator](../packages/coordinator.md) —— 组合、manifest、按 digest 重建
- [iCoder 教程](../icoder.md) —— 一个真实应用怎么把这两者接起来
