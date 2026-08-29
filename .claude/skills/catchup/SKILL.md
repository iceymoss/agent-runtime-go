---
name: catchup
description: 恢复上下文。在 /clear 后或新 session 开始时使用。当用户说恢复上下文、继续之前的工作、catchup 时触发。
---

恢复上下文。在 /clear 后或新 session 开始时使用。

## 步骤

1. 运行 `git log --oneline -20` 查看最近提交
2. 运行 `git status --short` 和 `git diff --stat HEAD` 查看未提交变更
3. 运行 `git diff --stat HEAD~5` 查看最近改动集中在哪些包
4. 读 `CHANGELOG.md` 的 `## [Unreleased]` 段，确认有哪些已完成但未发布的变更
5. 若近期改动集中在 `demo/icoder`，读 `demo/icoder/PLAN.md` 了解 demo 的推进计划
6. 运行 `git branch -a` 查看分支状态

## 输出格式

```
## 当前状态
- 分支: xxx
- 最近工作: （从 git log + Unreleased 段总结，说明动的是根包 / 哪个子包 / demo）
- 未完成的事: （从未提交变更、PLAN.md 或 Unreleased 推断）

## 建议下一步
- （基于上下文给出 1-3 个建议；若有未提交变更，提示先跑 CI 门槛）
```

简洁输出，不要重复 git log 的原始内容。
