# SUPERSEDED — keel-agent-team-v1

> 状态：2026-11-22 由 Owner 裁决取代，并按
> `openspec archive keel-agent-team-v1 --skip-specs --yes` 归档。
> 不把本 change 的旧规格应用到 `openspec/specs`。

## 取代者

`keel-bot-identity-access`（2026-11-22 归档）已交付现行产品面：

- Account / Bot / Profile / Role / TeamBot 领域模型；Bot 是独立权限主体。
- permission-key + scope + 权限组 + ReBAC-lite 资源关系；Bot 有效权限 =
  Account 权限 ∩ Harness permissions。
- TeamBot = Mission + Roster，不是权限主体，不拥有 Session。
- Session 默认私有；viewer/editor 跨账号分享；`relation.changed` 等审计事实。
- 旧 `agents`/`agent_teams` 表、`/v1/agents`/`/v1/teams` 路由、`keel agent`/`keel team`
  CLI、设置页 Agent/Team 词汇已按 alpha 零兼容删除。

## 映射

| 本 change 的旧模型 | 现行模型 |
|---|---|
| Agent（配置文档） | Bot（Profile + ModelKind + Harness） |
| AgentTeam（队长 + 成员） | TeamBot（Mission + Roster；不是权限主体） |
| 模型预设 / 团队默认模型 | Bot 默认模型 + ModelKind；运行时可覆盖 |
| Team CLI 能力开关 | Bot/TeamBot permission-key 与 `can()` |
| Agent/Team 插件实体 | Bot/TeamBot 产品面与权限目录 |

## 最后一条未完成任务

`P0 部件级设计` 不再完成。`design/` 里的 Agent 卡、团队画布等资产只作历史参考，
不得据此恢复 Agent/AgentTeam 词汇、旧路由或旧 CLI。

## 为什么归档而不完成

- 旧板假设 Agent/Team 是配置文档；现行模型把 Bot 作为独立权限主体，Account 是权限上限，
  Harness 收窄权限，角色不再授予权限，语义已变。
- 继续完成旧任务会与 `keel-bot-identity-access` 的破坏性词汇重置直接冲突。
- 本 change 没有 specs delta；`--skip-specs` 保证归档不向 `openspec/specs` 新增
  AgentTeam 规格。

## 证据

- 归档 change：`openspec/changes/archive/2026-11-22-keel-bot-identity-access/`
- A6 E2E：`.agents/docs/E2E-REPORTS.md` 2026-11-22 条目；10/10 步、304/304 断言、
  P0/P1/P2=0。
