# 8. 扩展工具来源

## 你现在遇到的问题

到现在为止工具都是你写的 Go 函数。但有些能力在别处：公司内部已经有一批 MCP server，或者你想让运维不改代码就能给 agent 加一段专用指令。

## 远端工具：MCP

Model Context Protocol 让工具住在另一个进程或另一台机器上。`mcp` 子包负责发现和调用，支持 stdio 和 streamable HTTP：

```go
import "github.com/iceymoss/agent-runtime-go/mcp"
```

关键在于**远端工具进入和本地工具同一套生命周期**：同样的权限判定、同样的 effect class、同样的重放策略。这不是锦上添花——MCP 工具由远端服务定义，它想让你执行什么你事先并不知道，所以默认应该按"未知网络副作用"逐次询问审批，而不是直接放行。

连接器只应指向经过批准的 endpoint。

## 非可信指令：Skills

Skills 是一份可以由运维或用户提供的指令目录——"处理退款工单时先查这三个字段"这类。它们会被注入 prompt。

`skills` 子包按需选择而不是全量注入，并且把它们当作**非可信文本**对待：

```go
import "github.com/iceymoss/agent-runtime-go/skills"
```

再说一次第 7 章那条：**skills 不是权限边界**。一份 skill 说"你可以直接删除临时文件"不会让工具的路径检查失效，因为检查在工具里。把 skills 当成可以被任何人写的输入来设计。

## 深入

- [mcp](../packages/mcp.md) —— 传输、发现、把远端工具映射进统一生命周期
- [skills](../packages/skills.md) —— 目录、优先级、注入边界
