# Proposal: keel-pi-parity-audit

> 立项：2026-11-25。P3 主线要求的只读 PI parity 审计；不解冻 `keel-ecosystem`，
> 也不在本 change 里改产品代码。

## Why

`keel-ecosystem` 自 2026-11-22 冻结，解冻前需要回答：68 条未勾任务里有多少其实已由
v2/归档 change 以不同形态交付，哪些是真缺口。审计还发现 `docs/HOST.md` 与
`docs/MATURITY.md` 有与代码不符的“已交付”声明，必须先校正，避免后续按错误文档排期。

## What Changes

1. 固化 2026-11-25 只读审计：68 条未勾任务的去向统计、PI 契约能力对照、P0/P1/P2 缺口。
2. 校正文档真值：
   - `docs/HOST.md`：`skill.allowed-tools` 执行期交集未接线、hooks 未接出货二进制、
     turn/tool/compaction 子 span 未接线。
   - `docs/MATURITY.md`：Skills/MCP/Hooks/可观测的“当前等级/缺口”按代码事实更新。
   - `openspec/changes/keel-ecosystem/FROZEN.md`：登记本次审计的取用分类与后续推荐。
3. 不修改 `keel-ecosystem/tasks.md`；不新增/删除 spec（docs-only，`skip_specs: true`）。

## Findings

- 68 条未勾：**44 无需重新实现**（38 条已等价交付 + 6 条库已实现）、**7 不适用**、
  **10 部分**、**7 缺失**（去重后 6 个能力）。
- P0：hooks 产品接线、遥测 span 树、文档真值。
- P1：turns/tool/context 指标、OTLP adapter、真实 MCP server E2E、nightly chaos、
  `allowed-tools` 执行期交集、usage ledger/observed stream 接线。
- 建议：不解冻整板；另开 `keel-pi-parity-closure`，按 T1 审计入库 → T2 hooks 接线 →
  T3 span 树+指标 → T4 OTLP → T5 真实 MCP E2E 推进。

## Impact

- 仅 `docs/`、`openspec/changes/keel-pi-parity-audit/`、`FROZEN.md`；无代码/运行时变化。
- 不改 `keel-ecosystem/tasks.md` 字符；6 份旧 spec 仍冻结，真源继续以 `openspec/specs/*`
  与归档 change 为准。
