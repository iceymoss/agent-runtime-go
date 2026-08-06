# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the project is at `v0.x`, minor versions may contain breaking changes.

## [Unreleased]

## [0.1.1] - 2026-08-06

### Added

- Apache License 2.0 (`LICENSE`) and `CHANGELOG.md`.

## [0.1.0] - 2026-08-06

The first public release.

### Added

- Core runtime: multi-step model/tool loop with JSON Schema validation,
  tool allowlists, stop conditions, context budgets, and loop detection.
- `providers/openaicompat`: official adapter for any OpenAI-compatible
  endpoint (OpenAI, DeepSeek, Qwen, Kimi, vLLM, Ollama) with SSE streaming,
  tool-call fragment assembly, usage normalization, and classified errors.
- `agent.NewTool` / `agent.MustNewTool`: define tools from typed Go
  functions; the JSON Schema is generated from the input struct.
- Optional subpackages: `session`, `durable`, `permission`, `event`,
  `skills`, `mcp`, `coordinator`, `subagent`, `provider`, `prompt`,
  `context`, `message`, `tool`, `app`, and `agenttest`.
- Runnable examples: `examples/hello`, `examples/tool-agent`,
  `examples/openai-compat`, and the `demo/icoder` reference application.
- Bilingual README (English / Simplified Chinese) and GitHub Actions CI
  (gofmt, `go vet`, tests with race detector for both modules).

[Unreleased]: https://github.com/iceymoss/agent-runtime-go/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/iceymoss/agent-runtime-go/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/iceymoss/agent-runtime-go/releases/tag/v0.1.0
