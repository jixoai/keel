# Proposal: fix-session-delete-soft-archive

> 缺陷修复。`openspec/specs/app-shell/spec.md` 的「会话列表」要求 MUST 支持删除；
> 但服务端从未注册 `DELETE /v1/sessions/{id}`——web 客户端 `deleteSession()`
> （`web/src/data/http.ts:596`）恒得 404，UI 删除入口对真实部署 100% 失败
> （fixture 在内存里实现了删除，掩盖了缺路由）。

## Why

**实证（2026-11-14，13760 真库）**：

```
DELETE /v1/sessions/d029f6b1-… → 404 page not found   （Go mux 默认文本 = 路由不存在）
grep -rn 'DELETE /v1/sessions' internal/ → 零命中
```

- 影响面：会话列表「删除」是一条死路（弹「删除会话失败」）；这是 spec 要求 vs 实现的硬缺口，
  不是新功能。
- 语义裁决：**会话的事件账本是计量、回放与审计的真值**（usage/run/decision 均自它派生），
  产品面删除 MUST NOT 硬删账本。「删除」= 软删除（归档）：从列表消失、账本保留。
  与 agents/teams 的 `archived` 模式一致，也为未来的「最近删除/恢复」留了载体。

## What Changes

- **schema**：`sessions.archived_at timestamptz`（幂等 `ALTER`，沿用单文件 `schema.sql` 惯例）。
- **store**：`ArchiveSession(ctx, tenantID, sessionID) (bool, error)`——`UPDATE` 置
  `archived_at`，**不触碰 `updated_at`**（归档不重写会话自己的时间线）；不存在/跨租户 → `false`。
- **agentapi**：新 seam `Config.ArchiveSession`（nil → 503）；
  `DELETE /v1/sessions/{id}` → 200 `{"sessionId","archived":true}`；
  `found=false` → 404 `session not found`；**幂等**（重复删除仍 200）。
- **列表**：`PGSessionLister.sessionSelect` 增加 `AND s.archived_at IS NULL`。
- **事件读/流**：不变——软删除保留账本；直接按 id 访问归档会话仍可读（预期行为）。

## Non-Goals

- 硬删除 / GC sweep（未来独立 change，需配 TTL 与计量豁免规则）。
- 「最近删除 / 恢复」界面。
- 外部会话（`state_owner=external`）的特殊删除语义——本期统一软删除，不额外区分。
