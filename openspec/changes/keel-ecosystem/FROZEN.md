# FROZEN — keel-ecosystem

> 状态：2026-11-22 Owner 裁决冻结为参考/backlog。**不是已交付**；不得按 delivered 归档。
> 本文件不改变 `tasks.md`：68 条未勾保持未勾，不补勾、不改写、不删除历史注记。

## 定位

- 历史 PI 对齐/生态任务板：observability、recoverability、compaction、skills、MCP、
  hooks、provider files 与收口项。
- 2026-11-08 已冻结并转 `keel-service-graph`；2026-11-14 逐条对账见
  `openspec/changes/keel-harness-v2/RECONCILIATION.md` §2。
- 2026-11-22 再次确认：不作为活跃线，不单独归档，不与 `keel-harness-v2` 并行推进。

## 取用规则

- 只有 `keel-harness-v2` **W11 实际需要**的条目，才可后续以新任务/新 change 形式取用；
  取用前必须在对账表登记来源、理由和落点。
- 不得把本板勾选当作交付证据；已交付项以归档 change + 代码/测试/报告为准。
- 不得把未交付项写成完成；`4/72` 只是历史勾选数，不是交付率。
- 不得恢复已废弃条目（例如 `mcp__<server>__<tool>`、脚本默认禁止、抽 `pkg/` 稳定 SDK），
  这些已被后续 Owner 裁决或 `keel-harness-v2` 口径取代。

## 2026-11-25 parity 审计（只读）

- 68 条未勾：**44 无需重新实现**（38 条已等价交付 + 6 条库已实现）、**7 不适用**、
  **10 部分**、**7 缺失**（去重后 6 个能力）。
- §6 的 6 条 hooks（94/97/98/99/100/101）**库已实现但未接线**：`internal/hooks` 不在
  `cmd/keel` 依赖图，租户 `hooks.commands` 只校验/计数；**不得按 delivered 记**。
- 真缺口：hooks 产品接线、turn/tool/compaction span 树、真实 MCP server E2E；
  P1 见 `keel-pi-parity-audit/audit.md`。
- 建议保持本板冻结；另开 `keel-pi-parity-closure`（T1 审计入库 → T2 hooks → T3 span
  树+指标 → T4 OTLP → T5 真实 MCP E2E）。本文件的审计不改变 `tasks.md` 字符。

## 相关

- `openspec/changes/keel-harness-v2/RECONCILIATION.md` §2（生态板逐条去向）
- `openspec/changes/keel-harness-v2/tasks.md` W11（收口门）
- `openspec/changes/runtime-foundation/FOLDED.md`（另一条冻结线的折叠）
- `openspec/changes/MAINLINE.md`（程序板与 Defer 清单）
