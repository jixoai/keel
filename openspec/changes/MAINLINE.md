# MAINLINE — Keel 程序板

> Owner 2026-11-22 裁决。本文件是唯一程序板（canonical board）；任务真值仍是各 change 的
> `tasks.md` + 证据。勾选数只表示板面状态，不表示本文件重新验收过实现。
> 更新：2026-11-22 · 分支：`task/keel-board` · 基线：`548e3ae`（main）

## 当前状态（板面快照）

| Change | 勾选 | 状态 | 处置 |
|---|---:|---|---|
| `keel-harness-v2` | 68/157 | **唯一活跃主线** | 只推进这一条 |
| `runtime-foundation` | 39/47 | 已折叠 | 8 项残余并入 W11；不单开线 |
| `keel-ecosystem` | 4/72 | 冻结参考/backlog | 只有 W11 实际需要才取用 |
| `keel-agent-team-v1` | 61/62 | 已被取代 | 归档；最后 1 条设计任务不做 |

已交付/已归档的锚点（只作事实引用，不重复验收）：

- 身份/Bot/TeamBot/共享/审计：`keel-bot-identity-access` 2026-11-22 归档，A6 10/10 步、
  304/304 断言、P0/P1/P2=0；见 `.agents/docs/E2E-REPORTS.md` 2026-11-22 条目。
- 任务→bash→capture→任务面板：W3/W4 主体已勾（W4 面板项仍开）。
- Web + TUI：W7/TUI 证据在；W10 触发器已勾。
- 恢复：W8 子代理 resume 已交付；W11 G2 的 kill -9/容器重启端到端证据仍开。

板面仍开的主要口子（按 W）：W0 多用户/清单/RSS/原生 target；W0.5 插件全景；W1 Rust+gRPC；
W2 组排队/ETA/OOM；W3 capture 策略覆写；W4 面板；W9 OAR；W11 收口门；W12 槽位/恢复/产品件。

## Owner 裁决（2026-11-22，记录，不再讨论）

1. `keel-harness-v2` 是唯一活跃主线；其他 change 不得并行推进。
2. `runtime-foundation` 残余折叠进 W11 G4/G10（RLS/quota/RSS/chaos/cleanup），不单开线。
3. `keel-ecosystem` 冻结为参考/backlog；只有 W11 实际需要的条目才可后续取用。
4. `keel-agent-team-v1` 已被交付的 Bot/TeamBot 模型取代；最后一条设计任务不完成。
5. Alpha 范围是垂直切片（见下）；范围外一律 defer，不扩线。
6. Skill 脚本默认允许（Owner override）；不新增 deny-by-default。

## Alpha 垂直切片（只做这些）

- 身份/Bot/TeamBot/共享/审计。
- tasks → bash → capture → task panel。
- 一个 OAR 外部工具（Codex）。
- UI slots + 一个 Native chunk 插件。
- 重启恢复 + memory gate + resource cleanup。
- Web + TUI。

## 活动切片与依赖顺序

```text
W0.5 插件全景/扩展面冻结
  -> W9 OAR（Codex；实现前先冻结 OAR 契约，W0-6）
  -> W12 slots + Native chunk
  -> W2/W12 recovery（组恢复、supervisor、配置化 resume）
  -> W11 收口门（E0/G0-G11）
```

- **W0.5**：catalog id/status/wave、layer、四层装配、seam、extensibility 格式冻结；后续
  slots 和 OAR 都依赖它。
- **W9**：OAR 契约冻结 + sidecar + record stream + 外部工具渲染器；只接 Codex 一条真链路。
- **W12 slots**：`keel.ui` 槽位机制 + 基础槽位 + 四形态 + `chunk` 验收；只要求一个 Native
  chunk 插件，不铺 42 个 W12 插件。
- **W2/W12 recovery**：组槽位/排队或 429 口径、组 OOM、supervisor/升级阶梯、resume 配置；
  复用已交付的 W8 subagent resume，不重做。
- **W11**：按 `acceptance.md` 的 E0→G0→G1→…→G11 顺序收口；前置门未绿，后续门不成立。

## 明确 Defer（Alpha 不做）

- macOS 原生打包/签名/更新器（保留 web/TUI；macOS target 产物后置）。
- layerfs Rust + gRPC（Phase A Go CAS 继续用；ADR 保留为扩展实现）。
- 多租户插件面（`plugin_state(tenant_id,...)`、`/v1/plugins` 租户过滤）。
- super-agent。
- 完整 spill / compaction / checkpoint / prompt-cache。
- bundles / presets。
- 全部 42 个 W12 插件（只做 slots + 一个 Native chunk 验收所需的条目）。

## 规则

- **只有 `keel-harness-v2` 前进**；`runtime-foundation`/`keel-ecosystem`/`keel-agent-team-v1`
  不再勾任务、不再开分支、不再单独验收。
- 任何新需求先判断是否属于 Alpha 垂直切片；不属于则登记到本文件 Defer 或 `FROZEN.md`。
- 本文件不替代 `tasks.md`；勾选/证据变更回各 change，程序级状态只改本文件与
  `.agents/docs/MAINLINE-STATUS.md`。
- 归档不改变历史：`keel-agent-team-v1` 以 `--skip-specs` 归档，不把旧 AgentTeam 规格写进
  `openspec/specs`。

## 链接

- `openspec/changes/keel-harness-v2/tasks.md`
- `openspec/changes/runtime-foundation/FOLDED.md`
- `openspec/changes/keel-ecosystem/FROZEN.md`
- `openspec/changes/archive/2026-11-22-keel-agent-team-v1/SUPERSEDED.md`
