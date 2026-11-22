# Orchestration: keel-bot-identity-access

> 本文件是这条 change 的**推进编排**（切片、依赖、文件所有权、门禁、证据）。
> 判据正文不在这里：交付门禁 → `.agents/skills/fde2e-delivery-gates/`；流程 → `.agents/docs/WORKFLOW.md`；
> 验收 → 本 change `acceptance.md`；设计 → `fde2e-frontend-standards` + `ui-experience-gate`。

## 0. 头部

| 项 | 值 |
|---|---|
| change | `keel-bot-identity-access` |
| 分支 | `task/keel-bot-identity-access` |
| worktree | `/Users/kzf/Development/GitHub/jixoai/fde2e/.worktrees/keel-bot-identity-access` |
| 基线 | `6fc1b2c`（设计提交 `1a7220e` 起） |
| 证据目录 | `openspec/changes/keel-bot-identity-access/evidence/` |
| 实现 owner | 各切片子代理（见 §2）；MainAgent 只编排/验收/集成 |
| 设计 | OpenDesign 项目 `keel-bot-identity-access` + `design/` 设计说明 |

## 1. 阶段序列

```
D0 设计冻结（已完成 1a7220e）
  ↓
P0 产品设计（OpenDesign：方向/IA/五态/关键原型）——评审选定
  ↓
L1 Schema + 领域模型（进行中）
  ├→ L2 认证（依赖 L1）
  ├→ L3 权限引擎（依赖 L1；与 L2 文件分区并行）
  ↓
L4 产品面重命名（Bot/TeamBot API/CLI/服务端）——依赖 L2/L3
  ├→ L5 UI 实现（依赖 L4 + P0 设计）
  └→ L6 迁移/文档/旧入口删除
  ↓
冻结快照 → Gate A/B/C（三个独立子代理）
  ↓
E2E（每步截图 + HTML）→ Fix → Re-gate
  ↓
Integrate（rebase/squash）→ alpha 部署 → Verify → Archive
```

## 2. 切片台账

| # | 切片 | owner | 文件范围（主） | 依赖 | 验收命令 | 状态 |
|---|---|---|---|---|---|---|
| P0 | 产品设计 | product-manager + OpenDesign | `design/`；OpenDesign 项目 | D0 | 2–3 个方向 + seed challenger；独立 fresh-context 视觉评审；Owner 选定后才实现 | 待做 |
| L1 | Schema + 领域模型 | slow-agent | `internal/store/schema.sql`、`internal/store/{accounts,bots,team_bots,permissions,relations,bot_keys}.go`、`internal/botteam/` | D0 | `go build ./...`、`go vet ./...`、`go test -count=1 ./internal/store ./internal/botteam` | **进行中** |
| L2 | 认证 | slow-agent | `cmd/keel/auth*.go`、`internal/auth/`、`internal/httpapi/auth*`、`cmd/keel/admin*` | L1 | L2 包测试 + 登录/bootstrap/CSRF/限速 E2E | 待做 |
| L3 | 权限引擎 | slow-agent | `internal/access/`、`internal/httpapi/access*`、`internal/toolsurface/access*` | L1 | registry 覆盖率门 + 并集/scope/关系/`grant ⊆ own` 测试 | 待做 |
| L4 | 产品面重命名 | slow-agent | `internal/harnessapi/`（原 agentapi）、`cmd/keel/bot*`、`cmd/keel/team_bot*`、去掉旧路由 | L2,L3 | `go build ./...`、API/CLI E2E、旧入口 404 | 待做 |
| L5 | UI 实现 | UI slow-agent | `web/src/screens/{login,settings-accounts,settings-permissions,settings-bots,settings-team-bots,team-run-dag}*`、`web/src/data/*` | L4,P0 | `nsh check`、`nsh test --target tui`、四档截图 | 待做 |
| L6 | 迁移/文档/清理 | slow-agent | `docs/`、`README.md`、`site/docs/`、旧表/旧测试删除 | L4 | 旧 schema 拒绝启动测试、文档链接检查 | 待做 |
| G | 三轴 + E2E | 独立子代理 | `evidence/gates/`、`evidence/e2e/` | L1–L6 冻结 | `pnpm workflow:gate`、report.html | 待做 |

> 状态：待做 / 进行中 / 待门禁 / 已提交 / 阻塞。

## 3. 文件所有权矩阵（防并行冲突）

| 范围 | 唯一 owner | 说明 |
|---|---|---|
| `internal/store/schema.sql` + 新 store 文件 | L1 | L2/L3 只加自己的方法文件，不改 schema |
| `internal/botteam/` | L1 | 领域纯数据；UI/HTTP 只消费 |
| `cmd/keel/auth*.go`、`internal/auth/` | L2 | L3 不改登录 |
| `internal/access/` | L3 | L2 只调用 `access` 接口；L4 接 middleware |
| `internal/harnessapi/`（原 agentapi） | L4 | L2/L3 不改路由文件 |
| `cmd/keel/bot*.go`、`team_bot*.go` | L4 | 旧 `agentteam.go` 由 L4 删除/替换 |
| `web/src/screens/*` | L5 | 按页面分文件；设计稿 owner P0 |
| `docs/`、`site/docs/` | L6 | L4 只改代码注释，不写文档 |
| `openspec/changes/keel-bot-identity-access/` | MainAgent | 子代理不改设计/验收文档；发现矛盾先报告 |

**串行段**：D0 → P0 设计评审 → L1 → L2/L3 → L4 → L5/L6 → 冻结 → 三轴 → 集成。
**可并行**：L2 与 L3（文件不重叠）；P0 设计与 L1（设计不改代码）；L5 页面之间按文件分。

## 4. 子代理简报模板（每个切片必须含）

- 工作目录绝对路径、分支、基线 commit。
- 必读：`proposal.md`、`design.md`、`specs/identity-access/spec.md`、`acceptance.md`、本文件。
- 只做哪个切片；文件范围；不得改别的 owner 文件。
- 绿门命令（具体到包/脚本）；证据路径。
- 不 `git add -A`、不 push/rebase；显式路径提交；每片立即 commit。
- 完成报告：commit hash、文件、测试、已知缺口、需要裁决点。
- 进程/临时 DB 回收方式并留证。

## 5. 门禁与证据

### 5.1 切片级

- L1：schema/RLS 目录断言 + 领域校验 + CRUD/跨租户。
- L2：Argon2id、cookie、CSRF、限速、bootstrap、Bot key、旧凭证负控。
- L3：registry 覆盖率门、并集/scope、`grant ⊆ own`、ReBAC、超管短路。
- L4：新旧路由/CLI 对照、旧入口 404、API 契约快照。
- L5：`nsh check`、TUI、四档截图、五态、令牌/对比度/触控/焦点。
- L6：旧 schema 拒绝启动、文档词汇扫描（产品面 0 个 Agent）。

### 5.2 冻结后三轴

- 冻结快照：L1–L6 全部提交后的 commit hash；冻结后禁写。
- A 安全红队 / B 功能与链路 / C 视觉与设备：三个独立子代理，实现方不兼任。
- `pnpm workflow:gate -- openspec/changes/keel-bot-identity-access/evidence/gates` 退出码 0。
- P0/P1 = 0 才可写“完成”；P2 进 backlog。

### 5.3 E2E

- `acceptance.md` §5 的 10 步；每步截图；`steps.json` → `report.html`（脚本生成）。
- 改动文件 before/after 覆盖表，UNCOVERED=0；无视觉面三类豁免逐行登记。
- 报告公网链接（禁 localhost）；索引写 `.agents/docs/E2E-REPORTS.md`。

## 6. 回合与升级

- 每轴最多 3 轮修复；连续两轮 P0+P1 不降或新增 P0 → 升级（换更强子代理 / 并行实现线 / 回问 Owner）。
- 修复后重跑该轴**全量**，不只跑失败项。
- 每轮记录：轮次/轴/失败项/修复 commit/重跑结果/分差/是否升级。

## 7. 集成与交付

- 合回 main 一律 rebase/squash，禁 merge commit；子仓先推、主仓指针后跟。
- 预交付走 `alpha-*` 子域；部署后 fresh 子代理复验关键旅程 + 链接可开 + 回滚点。
- 常驻进程/端口/临时 DB 回收并留证。
- 归档：tasks 全勾、三轴 P0/P1 清零、E2E/覆盖表交付、部署复验通过 → `openspec archive`。

## 8. 当前批次

- **A1（L1）**：schema/domain 子代理进行中；产出后 MainAgent 初审并提交。
- **P0 设计**：product-manager 产出产品方向/IA/五态；OpenDesign 项目同步；评审选定后交 L5。
- **W10**：独立线，干净库全量测试中；通过后先合入 main，不与本 change 混提。
