# A6 共享面（resource_relations 产品面）证据

> 切片：A6 P1 `no-session-share-surface` / `no-bot-share-surface` 收口。本文件是证据摘要，
> 不是设计正文；判据仍以 `proposal.md` / `design.md` / `specs/identity-access/spec.md` /
> `acceptance.md` 为准。Alpha 零兼容：没有旧共享入口、旧词汇或迁移垫片。

## 1. 基线

- worktree：`.worktrees/keel-bot-share`
- 分支：`task/keel-bot-share`
- 基线：`9f1de3d`
- 实现提交：见本节末尾（本文件与实现同批提交）

## 2. 路由与授权

九条路由，三族同构；`resource key + owner relation` 由 `requireResource` 统一判定：

| 路由 | key | 关系 |
|---|---|---|
| `GET /v1/sessions/{id}/shares` | `session.share` | session owner |
| `POST /v1/sessions/{id}/shares` | `session.share` | session owner |
| `POST /v1/sessions/{id}/shares/revoke` | `session.share` | session owner |
| `GET /v1/bots/{id}/shares` | `bot.manage` | bot owner |
| `POST /v1/bots/{id}/shares` | `bot.manage` | bot owner |
| `POST /v1/bots/{id}/shares/revoke` | `bot.manage` | bot owner |
| `GET /v1/team-bots/{id}/shares` | `team_bot.manage` | team_bot owner |
| `POST /v1/team-bots/{id}/shares` | `team_bot.manage` | team_bot owner |
| `POST /v1/team-bots/{id}/shares/revoke` | `team_bot.manage` | team_bot owner |

关键语义（handler 语义不在 UI）：

- 只可写 `viewer` / `editor`；`owner` / `team_member` 一律 400。
- subject 只接受 `account` / `group` / `team_bot`，写前在租户内解析；不存在/跨租户均 404。
- account subject 不能等于调用者（自共享 400）；`owner`/`team_member` 永不由 HTTP 授予。
- 重复 tuple 幂等：`created:false` + 200；撤销缺失 tuple 404。
- 每次成功写入/撤销调用 `Access.AuditRelation`（action `share`/`revoke`）；审计失败 500。
- 先 key+relation 后读行：无权限调用者对存在/不存在资源返回同一个 403。
- 撤销即时生效：engine 每次请求读 live relation，handler/UI 不缓存 tuple。
- team_bot 共享只是可见性：viewer/editor 允许 `team_bot.read`；启动仍需 owner（`team_bot.manage`）。

## 3. 后端接线

- `internal/harnessapi/shares.go`：九条路由的共享实现（三族共用，防漂移）。
- `internal/harnessapi/identity.go`：mux 注册 + `identityEndpointCatalog` 九项。
- `internal/accessgate/matrix.go`：九条 `resource_key` 声明（path:id）。
- `internal/store/relations.go`：`ResourceOwnerAccounts`、`ResourceRelationCounts`（列表 badge /
  TeamBot owner 的单查询批量读取；无 N+1）。
- `internal/harnessapi/sessions.go` / `sessions_pg.go`：`SessionSummary` 增加
  `ownerAccountId`、`createdByAccountId`、`ownerDisplayName`、`shared`、`sharedWithMe`
  （后者按调用者相对计算，供「我的 / 共享给我」筛选与「由 X 共享」）。
- `internal/harnessapi/bots.go` / `team_bots.go`：`sharedCount`；TeamBot 增加 `ownerAccountId`
  （owner-only 共享动作的展示门与引擎判定同源）。

## 4. 路由矩阵

`internal/httpapi/route_matrix_test.go:TestEveryV1RouteHasPermissionDeclaration` 实测：

```text
route permission matrix: 176 declarations, 176 registered routes
PASS
```

`TestMatchSelectsTheDeclaredRuleForEveryRoute` 通过（九条新路由均被正确的 literal 规则选中）。

## 5. 测试与结果

### 5.1 新增 PG 测试（真 DB + httptest，5 个用例）

命令（本地 `keel-pg`，独立空库）：

```bash
KEEL_TEST_DSN='postgres://postgres:runtime@127.0.0.1:13741/keel_a6_share_test?sslmode=disable' \
  go test ./internal/harnessapi -run \
  'TestSessionShareSurface|TestBotShareSurface|TestTeamBotShareSurface|TestShareSubjectValidation|TestShareTenantBoundary' \
  -count=1 -v
```

结果：`5 passed`。覆盖：

- owner 共享 viewer → viewer 读、不可写；editor 可写、不可再共享（缺 owner）。
- `session.write`-only 不能共享；撤销后下一次 engine decision 拒绝；重复幂等；缺失撤销 404。
- Bot 共享 → 跨账号 roster + invoke 成功；viewer 不能 manage/re-share；撤销后读/invoke 403。
- TeamBot 共享 → viewer 可读、不能 start；非 owner 即使持有 key 也不能共享；撤销后读 403。
- group / team_bot subject 解析；跨租户/缺失 subject 404；malformed 400；owner/team_member 400。
- 审计行存在（`relation.changed` + `detail.action=share`）。
- 存在性不泄漏：同一无权限账号对存在/不存在资源得到同一 403（同 `requiredKey`）。
- 既有资源随机 id：super admin 也得到 404，不伪造行。

### 5.2 相关 Go 包

```bash
go test ./internal/harnessapi ./internal/httpapi ./internal/accessgate \
        ./internal/store ./internal/access ./internal/accesswire -count=1
```

结果：全部 `ok`（18.4s / 18.7s / 0.3s / 15.3s / 0.3s / 2.2s）。

全量 `go test ./...` 同批运行时出现两处并行资源耗尽：

- `internal/httpapi / TestSessionSingleResourceNegativeControls`：PG `deadlock detected`；
- `internal/toolauth / TestEveryToolWithGrantsIsAuthorized`：`too many clients already`。

两者单包重跑均 `ok`（16.6s / 7.5s），判定为全量并发占用本地 PG 连接，不是本切片回归。

### 5.3 Web

```bash
cd web
nsh check
nsh test src/data/identity.test.ts
nsh test src/data/http.identity.test.ts
nsh test src/data/shares.test.ts
nsh test --target tui src/screens/share.tui.test.tsx
nsh test --target tui App.tui.test.tsx
nsh test --target tui src/screens/books/bots-teams.tui.test.tsx
nsh test --target tui src/screens/books/books.tui.test.tsx
```

结果：

- `nsh check` 通过。
- 数据层：identity 10/10、http.identity 10/10、shares 2/2。
- TUI：share 3/3；App 12/12；bots-teams 22/22；books 15/15。

## 6. Web 产品面

- 会话：工作区 header「共享」动作（仅 `session.share` + owner，super admin 例外）；对话框列出
  viewer/editor、添加账号/权限组（可查看/可编辑）、破坏性确认撤销、空/加载/错/权限不足态；
  撤销后重读服务端列表并即时更新。
- Bot：Bot 详情「更多 → 共享 Bot」；列表/详情显示「已共享 N」。
- TeamBot：团队详情「更多 → 共享团队」；文案明确「共享只影响可见性，启动/修改仍需管理权」。
- 会话列表：全部 / 我的 / 共享给我；共享行显示「由 X 共享 · 预览」；owner 来自服务端
  `ownerAccountId`/`ownerDisplayName`，fixture 同步更新，不在客户端推测。
- 仅使用 `web/src/ui/kit` 的 Dialog/AlertDialog/Select/Badge/Button 与主题令牌；未修改 A4.4f
  归属文件（`web/src/ui/kit/**`、`books/shell.tsx`、`chapters.tsx`、`settings.tsx`、`common.tsx`、
  `web/kit/kit-manifest.tsx`、`books.tui.test.tsx`）。

## 7. 已知 P2（不在本切片扩大）

1. **添加共享的候选目录**使用现有 `account.read` / `permission.read` 读取；只有 `session.share`
   （或 `bot.manage` / `team_bot.manage`）而没有目录 key 的 owner 可以撤销已有共享，但 UI 不
   提供候选列表。共享写入本身不要求目录 key。若产品要求「任何 owner 都能搜索成员」，应另立
   共享对象检索端点（由共享 key + owner 门控、按前缀限流），不在此处偷读目录。
2. 共享列表的显示名解析按唯一 subject 逐条读取（上限 200）；当前规模可接受，若单资源共享
   数量增长，改为批量解析。
3. `sharedWithMe` 是「可见且 owner 不是我」的汇总，没有单独的「团队」页签；group/team_bot 共享
   都归入「共享给我」。如产品要区分来源，需要 `SessionSummary` 增加 `sharedVia` 投影。
