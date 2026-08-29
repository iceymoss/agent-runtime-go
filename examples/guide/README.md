# examples/guide

The application built across [docs/guide](../../docs/guide/index.md): an ops
assistant that can read logs, classify an incident, and restart a service.

Every code block in the guide comes from these files, so the documentation
cannot drift away from code that compiles.

```bash
# Deterministic, no API key needed
go run ./examples/guide

# Against a real model
OPENAI_API_KEY=sk-... OPENAI_BASE_URL=https://api.deepseek.com/v1 OPENAI_MODEL=deepseek-chat \
  go run ./examples/guide
```

| File | Chapter |
|---|---|
| `main.go` | 1, wiring everything together |
| `tools.go` | 2, a read-only tool and a dangerous one |
| `model.go` | 3, provider adapter plus retries |
| `conversation.go` | 4, carrying history between turns |
| `streaming.go` | 5, showing progress without losing text |
| `classify.go` | 6, structured output |
| `approval.go` | 7, stopping for a human |
