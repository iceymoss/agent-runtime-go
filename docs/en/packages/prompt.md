# prompt

The `prompt` package provides side-effect-free prompt template compilation and rendering: it surfaces template syntax errors at startup and assigns each template a deterministic version so you can trace which prompt version a run actually used.

## What it is

The package has one core type, `Prompt`: an immutable, precompiled template. It wraps the Go standard library `text/template` and forces the `missingkey=error` option—missing fields in render data fail immediately instead of silently emitting `<no value>`.

```go
func New(name, source string) (*Prompt, error)

func (p *Prompt) Name() string
func (p *Prompt) Source() string
func (p *Prompt) Version() string
func (p *Prompt) Render(data any) (string, error)
```

`New` parses the template at construction time; syntax errors return `ErrInvalid` immediately and do not wait until the first render. After successful construction, `Prompt` holds no mutable state; `Render` depends only on caller-supplied data and is safe for concurrent use from any number of goroutines.

`Version()` returns a string like `sha256:...`, the SHA-256 of the template-format marker (`go-text-template-v1`) plus the template source. It tracks semantic content only: two differently named templates with the same source share a version; changing one character of the source changes the version. That version is suitable for audit metadata on run events, checkpoints, or plans.

There are two package-level error variables: `ErrInvalid` (compile or render failure) and `ErrNotFound` (requested prompt does not exist—reserved for upper-layer prompt registry/lookup components; this package's own API never returns it).

## Why you need it

Using `text/template` directly leaves three problems for you:

- **Silent missing fields**: under the default config, <code v-pre>{{.Name}}</code> with no `Name` in the data emits `<no value>`, and bad prompts slip into model requests. This package forces `missingkey=error`, so missing fields fail at render time.
- **Late failure**: if template syntax errors wait until the first runtime render, you are usually already handling a user request. `New` moves failure to assembly time.
- **Version tracing**: change a prompt and Agent behavior changes. Without a deterministic version you cannot answer "did last Wednesday's anomalous run use the same prompt as now?". `Version()` gives a content-bound answer.

**When you do not need it**: if prompts are fixed strings with no variables, use string constants; if you need Jinja/Handlebars or other non-Go template syntax, this package will not help.

## How to use it

```go
package main

import (
	"fmt"

	"github.com/iceymoss/agent-runtime-go/prompt"
)

func main() {
	greeting, err := prompt.New("greeting", `你好，{{.Name}}。今天的任务是：{{.Task}}`)
	if err != nil {
		panic(err) // template syntax errors surface here
	}

	text, err := greeting.Render(map[string]any{
		"Name": "Ava",
		"Task": "整理订单数据",
	})
	if err != nil {
		panic(err) // missing fields also fail here
	}

	fmt.Println(text)
	fmt.Println(greeting.Version()) // sha256:..., safe to write into run metadata
}
```

Output:

```text
你好，Ava。今天的任务是：整理订单数据
sha256:fa9130ce104b484dd2de25a4a32fe68584551e4d555314962172c342ac27a13f
```

Key behavior:

- The first argument to `New` is the template name; it is used only for error messages and `Name()`, not for versioning.
- `Render` accepts any `data any`: maps or structs, following `text/template` lookup rules.
- Render failures (including missing fields and template execution errors) match `errors.Is(err, prompt.ErrInvalid)` and include the template name in the message.
- Once constructed successfully, `Prompt` is read-only and can live in a package-level variable for the process lifetime with no locking.

## FAQ

**Q: Do extra fields in the data that the template does not use cause an error?**
A: No. `missingkey=error` only constrains "referenced by the template but missing from the data"; extra fields are ignored.

**Q: Why do two differently named templates share the same `Version()`?**
A: Version is determined only by the format marker and source text (`sha256(go-text-template-v1 + "\x00" + source)`); the name is not included. That is intentional: version answers "is render behavior the same?", and same-source templates render identically.

**Q: When do syntax errors like <code v-pre>{{if}}</code> surface?**
A: In `New`. <code v-pre>prompt.New("broken", "{{if}}")</code> returns an error matching `errors.Is(err, prompt.ErrInvalid)`, and the process never gets a usable `Prompt`. Construct all templates on the startup path so a bad template blocks service bring-up.

**Q: What does a missing-field render error look like?**
A: It also matches `errors.Is(err, prompt.ErrInvalid)` and includes the template name plus the underlying `text/template` error (`map has no entry for key ...`). Note: missing keys on map data fail; referencing a non-existent struct field may pass parse in `New` and fail at `Render` with a field-not-found error.

**Q: When would I see `ErrNotFound`?**
A: This package's `New`/`Render` never return it. It is a sentinel reserved for upper-layer "lookup prompt by name" components—for example your own registry should return an error wrapping `prompt.ErrNotFound` when a name is missing so callers can use `errors.Is` uniformly.

**Q: Can templates call custom functions?**
A: No. `New` does not expose a `FuncMap` injection point; only built-in `text/template` features are supported. For complex logic, compute values in Go first and pass them as data—consistent with this package's "side-effect-free render; result determined only by data" stance.

**Q: What does the `go-text-template-v1` prefix in the version mean?**
A: It folds "template engine and its semantic version" into the hash input. If the package later switches dialect or changes render semantics, the prefix changes and versions diverge even when source text is unchanged. Comparing two `Version()` values, "equal" can safely mean "identical render behavior" without separately comparing engine versions.

**Q: Can `Prompt` be used concurrently? Do I need to cache the compile result?**
A: Yes, concurrent use is fine; `Render` mutates no internal state. Compilation happens once in `New`. The right pattern is to cache `*Prompt` itself: construct once at startup and reuse everywhere; do not call `New` on every request.
