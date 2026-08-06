# prompt

`prompt` 包提供无副作用的提示词模板编译与渲染：在启动期把模板语法错误暴露出来，并为每个模板生成确定性的版本号，让"这次运行到底用的是哪版提示词"可追溯。

## 是什么

包里只有一个核心类型 `Prompt`，是一个不可变的、预编译好的模板。它包装 Go 标准库 `text/template`，并强制开启 `missingkey=error` 选项——渲染数据缺字段时直接报错，而不是静默输出 `<no value>`。

```go
func New(name, source string) (*Prompt, error)

func (p *Prompt) Name() string
func (p *Prompt) Source() string
func (p *Prompt) Version() string
func (p *Prompt) Render(data any) (string, error)
```

`New` 在构造期完成模板解析，语法错误立即返回 `ErrInvalid`，不会等到第一次渲染才失败。构造成功后 `Prompt` 不再持有任何可变状态，`Render` 只依赖调用方传入的数据，可以被任意多个 goroutine 并发调用。

`Version()` 返回形如 `sha256:...` 的字符串，是对模板格式标识（`go-text-template-v1`）加模板源文本做 SHA-256 得到的。它只跟随语义内容变化：两个不同名字、相同源文本的模板版本相同；源文本改一个字符，版本就变。这个版本号适合写进运行事件、checkpoint 或 plan 的元数据里做审计。

包级错误变量有两个：`ErrInvalid`（编译或渲染失败）和 `ErrNotFound`（请求的提示词不存在，供上层的提示词注册/查找组件复用，本包自身的 API 不返回它）。

## 为什么需要它

直接用 `text/template` 你需要自己处理三件事：

- **缺字段静默通过**：默认配置下 `{{.Name}}` 在数据里缺 `Name` 时输出 `<no value>`，坏提示词就这样混进模型请求。本包强制 `missingkey=error`，缺字段渲染直接失败。
- **延迟失败**：模板语法错误如果等到运行时第一次渲染才暴露，通常已经在处理用户请求了。`New` 把失败提前到组装期。
- **版本追溯**：提示词一改，Agent 行为就变。没有确定性版本号，你无法回答"上周三那次异常运行用的提示词和现在一样吗"。`Version()` 给出与内容一一对应的答案。

**什么时候不需要它**：提示词是不含任何变量的固定字符串时，直接用字符串常量即可；需要 Jinja/Handlebars 等非 Go 模板语法时本包也帮不上忙。

## 怎么用

```go
package main

import (
	"fmt"

	"github.com/iceymoss/agent-runtime-go/prompt"
)

func main() {
	greeting, err := prompt.New("greeting", `你好，{{.Name}}。今天的任务是：{{.Task}}`)
	if err != nil {
		panic(err) // 模板语法错误在这里就会暴露
	}

	text, err := greeting.Render(map[string]any{
		"Name": "Ava",
		"Task": "整理订单数据",
	})
	if err != nil {
		panic(err) // 数据缺字段同样在这里报错
	}

	fmt.Println(text)
	fmt.Println(greeting.Version()) // sha256:...，可写入运行元数据
}
```

关键行为：

- `New` 的第一个参数是模板名，只用于错误信息定位和 `Name()`，不参与版本计算。
- `Render` 接受任意 `data any`：map、结构体都可以，遵循 `text/template` 的取值规则。
- 渲染失败（包括缺字段、模板执行错误）返回的错误可用 `errors.Is(err, prompt.ErrInvalid)` 匹配，错误信息里带模板名。
- `Prompt` 一旦构造成功就是只读的，可以放在包级变量里在整个进程生命周期内复用，无需加锁。

## 常见问题

**Q: 数据里多给了模板用不到的字段会报错吗？**
A: 不会。`missingkey=error` 只约束"模板引用了但数据里没有"的方向；多余字段被忽略。

**Q: 为什么两个名字不同的模板 `Version()` 相同？**
A: 版本只由模板格式标识和源文本决定（`sha256(go-text-template-v1 + "\x00" + source)`），名字不参与。这是有意设计：版本回答的是"渲染行为是否相同"，同源文本的模板渲染行为完全一致。

**Q: `{{if}}` 这类语法错误什么时候暴露？**
A: 在 `New` 里。`prompt.New("broken", "{{if}}")` 返回的错误满足 `errors.Is(err, prompt.ErrInvalid)`，进程根本拿不到可用的 `Prompt`。建议把所有模板的构造放在启动路径上，让坏模板阻止服务上线。

**Q: 渲染时数据缺字段，错误长什么样？**
A: 同样满足 `errors.Is(err, prompt.ErrInvalid)`，并包含模板名和 `text/template` 的底层错误（`map has no entry for key ...`）。注意：对 map 数据缺 key 会报错；对结构体数据引用不存在的字段，在 `New` 阶段解析虽然通过，`Render` 执行时会报字段不存在的错误。

**Q: `ErrNotFound` 什么时候会遇到？**
A: 本包的 `New`/`Render` 不返回它。它是为上层"按名字查提示词"的组件预留的哨兵错误，比如你自己实现的提示词注册表在查不到名字时应返回包装了 `prompt.ErrNotFound` 的错误，让调用方统一用 `errors.Is` 判断。

**Q: 模板里能调用自定义函数吗？**
A: 不能。`New` 没有暴露注入 `FuncMap` 的入口，只支持 `text/template` 的内置能力。需要复杂逻辑时，先在 Go 代码里算好再作为数据传入——这也符合本包"渲染无副作用、结果只由数据决定"的定位。

**Q: 版本号里的 `go-text-template-v1` 前缀是什么意思？**
A: 它把"模板引擎及其语义版本"编进了哈希输入。如果未来包切换模板方言或改变渲染语义，前缀会变，即使源文本不变版本号也会变化。因此比较两个 `Version()` 时可以放心地把"相同"理解为"渲染行为完全相同"，不需要额外比对引擎版本。

**Q: `Prompt` 可以并发使用吗？需要缓存编译结果吗？**
A: 可以并发，`Render` 不修改任何内部状态。编译只在 `New` 发生一次，正确的用法就是缓存 `*Prompt` 本身：启动时统一构造，之后到处复用；不要在每次请求里重新 `New`。
