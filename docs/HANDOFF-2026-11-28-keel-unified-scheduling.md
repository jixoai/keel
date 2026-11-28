# Handoff — Keel 统一调度现场（2026-11-28）

> 目的：ZCode 已停止所有 lane，不再合并/推送。交给另一个 main-agent 统一调度。
> 范围：只涉及 `jixoai/keel` 与 `jixoai/keel-board`；不涉及酒店/BU/宿主。

## 0. 当前 canonical 状态

- `keel` origin/main：`fad3a19d`（已包含 4 个官方 OpenSpec change 文档）
- 官方 change：
  - `keel-release-closure`
  - `keel-board-sync-v2`
  - `keel-collab-p2`
  - `keel-pi-parity-closure-2`
- `keel-plugin-split` 文档只在本地 commit `45482766`，**尚未进 origin/main**
- 本地主 worktree 当前不在 main：分支 `keel-release-closure`，HEAD `45482766`，dirty 3 个文件：
  - `.github/workflows/release.yml`
  - `site/docs/harness/release.md`
  - `site/docs/zh/harness/release.md`
- 上述 dirty 文件与 release lane 的 `771a6cb1` 内容可能重复；**不要 reset --hard**，先 diff/保留。

## 1. 各 lane 现场

| lane | worktree/branch | HEAD | 未提交 | 说明 |
|---|---|---|---|---|
| release | `.worktrees/keel-release-closure` / `task/keel-release-closure` | `771a6cb1`（已 push） | 干净 | 自建 `chore-keel-release-closure`；改 release.yml、spec Purpose；基于 `2c1245e9`，不含官方 change |
| board backend | `.worktrees/keel-board-sync-v2` / `task/keel-board-sync-v2` | `2c1245e9` | 7 项 | 改 go.mod/go.sum、augment、moves 测试、board-wire；新增 `sync_health_pg_test.go`；自建 `openspec/changes/feat-keel-board-sync-v2/` |
| board UI | 无独立 worktree/branch | — | — | 未开始或无留存 |
| collab | `.worktrees/keel-collab-p2` / `task/keel-collab-p2` | `2c1245e9` | 22 项 | 改 core/host/checkpoint/kernel/agent；自建 `openspec/changes/feat-keel-collab-p2/` |
| PI parity | `.worktrees/keel-pi-parity-closure-2` / `task/keel-pi-parity-closure-2` | `2c1245e9` | 26 项 | 改 compaction/agent/skills/mcp/kernel；自建 `openspec/changes/feat-keel-pi-parity-closure-2/` |

> 所有 lane worktree 都基于 `2c1245e9`，所以看不到 `fad3a19d` 的四个官方 change；它们各自建了 `feat-*`/`chore-*`，这是当前主要冲突源。

## 2. 必须先解决的冲突

1. **变更 ID 重复**
   - 官方 `keel-release-closure` vs lane `chore-keel-release-closure`
   - 官方 `keel-board-sync-v2` vs lane `feat-keel-board-sync-v2`
   - 官方 `keel-collab-p2` vs lane `feat-keel-collab-p2`
   - 官方 `keel-pi-parity-closure-2` vs lane `feat-keel-pi-parity-closure-2`
   - 规则：只保留官方 ID；把 lane 的 tasks/evidence 迁入官方 change，删除 `feat-*`/`chore-*` 目录。
2. **主 worktree 被占用**
   - 不能继续在 keel 主 worktree 跑 lane；先把 dirty 的 release 文件保存成 patch/branch，再让 main 回到 `origin/main`。
   - `45482766` 的 `keel-plugin-split` 文档用 cherry-pick 或独立分支保留，不能丢。
3. **release 内容重复**
   - 对比 `771a6cb1` 与主 worktree 的 3 个 dirty 文件；保留一份实现，另一份丢弃/记录。
4. **board UI 未开始**
   - 等 board backend 的 service/DTO 契约落定后再开，避免再建第二个 change。

## 3. 建议的合并顺序

```text
Step 0  现场清理/去重（main-agent 独占）
Step 1  release closure        （官方 keel-release-closure）
Step 2  board backend          （官方 keel-board-sync-v2：storage + API）
Step 3  plugin split           （board 插件 → superagent 插件依赖 board）
Step 4  board UI/P2            （同一 board change，依赖 Step 2/3）
Step 5  collab P2              （官方 keel-collab-p2）
Step 6  PI parity              （官方 keel-pi-parity-closure-2）
Step 7  final gates + v0.6.0
```

为什么这个顺序：

- board backend 先定 task/channel/thread 服务与 DTO；
- plugin split 依赖 board 服务，必须先于 superagent UI；
- board UI 依赖 backend 字段；
- collab/PI 在 repo 清理后可并行；
- 最终门禁和发布只能串行。

## 4. 统一调度规则

- **只有 main-agent 写 keel main**；lane 只在自己的 worktree 工作，提交到 `task/*` 分支。
- 每个 lane 开工前必须 `git fetch origin && git rebase origin/main`；不允许在旧 `2c1245e9` 上继续。
- 每个 change 只有一个官方目录；禁止再建 `feat-*`/`chore-*` 副本。
- 一支 lane 只改自己的文件组：
  - board backend：keel-board、Go mount、go.mod；
  - board UI：web/src/screens/board + web data；
  - plugin split：plugin kernel/board/superagent，不得与 board backend 同时改同一 mount 文件；
  - collab/PI：各自 internal 包。
- 合并前 lane 必须 rebase 到最新 main；合并后 main-agent 跑该 lane 定向测试。
- 不 push main、不 force-push、不重跑已发布 tag。
- 不保留兼容层；board/superagent 拆分按 `keel-plugin-split` 的破坏性要求执行。

## 5. 验收标准

- 官方 change `openspec validate <id> --strict` 通过；任务只在证据存在时勾。
- 每个 lane 独立 reviewer P0/P1=0；实现方不自评。
- board：storage conformance + PG adapter + API + UI；`nsh check`、board TUI、UI 53/26。
- plugin split：board 先加载、superagent `PENDING`、无 `superagentboard` bridge、无 board 表直连。
- collab/PI：定向 Go/PG/race + 证据。
- 最终：Go `-p 1 ./...`、全 TUI、UI 53/26、file coverage UNCOVERED=0、OpenSpec strict、v0.6.0 Release。
- Windows updater 不在本轮；不阻塞。

## 6. main-agent 立即 checklist

1. 冻结所有 lane（已停）。
2. `git -C keel status` 确认主 worktree dirty；保存 release patch/branch，不要 reset。
3. 把 `45482766` 的 `keel-plugin-split` 文档 cherry-pick 到一个干净分支/主 main。
4. 对每个 lane：
   - rebase 到 `fad3a19d` 之后的 origin/main；
   - 把 `feat-*`/`chore-*` 的 tasks/evidence 合入官方 change；
   - 删除重复 change 目录；
   - 只提交该 lane 的文件。
5. 按 Step 1 → 7 合并；每步独立门禁。
6. 任何冲突先停在 lane，不要直接改 main。
