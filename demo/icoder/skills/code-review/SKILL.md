---
name: code-review
description: Review code changes for correctness and regressions
version: 1.0.0
schema_version: 1
tool_requirements:
  - list_files
  - read_file
  - search_code
---
Inspect the changed implementation and its callers before drawing conclusions.
Prioritize correctness, security, behavioral regressions, and missing tests.
Report concrete file locations and do not invent repository state.
