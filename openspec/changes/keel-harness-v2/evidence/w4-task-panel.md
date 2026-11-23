# W4 · 任务面板：任务图 / 事件流 / 资源预览（纯投影）

Date: 2026-11-23. Change: `keel-harness-v2`, W4 最后一项
（`tasks.md`: 面板：任务图、事件流、资源预览；纯投影，不写额外真值）.
Worktree: `.worktrees/keel-w4-taskpanel`, branch `task/keel-w4-taskpanel`.

本证据只覆盖**面板这一项**。W11 的 alpha 切片
「任务创建 → bash 执行 → capture → 面板可见 → fork → 合并 → 卸载插件零残留」里，
面板可见这一段由本切片交付；其余段（bash/capture/fork/合并/卸载）各自的状态见
`tasks.md` 与 `RECONCILIATION.md`，本文件**不声称 W11 已完成**。

## 1. 交付了什么

### 1.1 读侧投影（Go）

| 件 | 文件 | 行为 |
| --- | --- | --- |
| 事件解码 | `internal/tasklog/view.go`（新） | `DescribeEvent(Event)` 把已物化的 `task_events` payload 解成面板字段（title/status/parent/origin/originSeq/path/hash/text/reason）。kind 与 seq 只取自行本身，payload 不能改自己的 kind；不在六种词汇里的 kind 标 `Unparsed` 保留，不丢行；**契约不符的 payload 是错误**而不是"空事件"。 |
| 面板折叠 | `internal/tasklog/panel.go`（新） | `Service.Panel(scope, memberLimit, eventLimit)`：本会话 own cards（按 createdSeq 排序）＋ `parentTaskId` 指过来的成员卡（跨会话，行仍在子会话）＋ 会话外父卡解析成 `Ancestors`＋每人卡的 attach/detach 折叠成 live `Attachments`＋整会话 task 事件流。成员页与事件页都有上界与精确的截断计数；own child 卡不占成员名额（读时按 own 卡数 over-fetch）。**只依赖 `Projection`**：`Panel` 的守卫只查 projection，连 writer 都拿不到。 |
| 整会话事件读 | `internal/tasklog/pg.go`、`internal/store/tasks.go` | `Projection.ListSessionEvents` + `Store.ListSessionTaskEventRows`（`WHERE tenant_id, session_id … ORDER BY seq`），一次读完整会话事件流。 |
| 路由 | `internal/harnessapi/task_panel.go`（新） | `GET /v1/sessions/{id}/task-panel?memberLimit&eventLimit`：树、成员、祖先、资源引用、事件流、界与截断；`member` / `ancestor` 标记由 fold 决定。`GET /v1/sessions/{id}/file-preview?path&hash&offset`：按**会话清单**解析一个 path，给一页有界内容 + 清单当前 hash + `matchesReference`。`task.panel` 进 `GET /v1/capabilities`；`SessionExists` 缺省先 404，防止空 200 泄露会话存在。 |
| 预览读 | `internal/fileplugin/service.go` | `Service.ReadEntry(ctx, scope, path, offset, limit)`：`Read` 的 scope-explicit 形式（同一有界页、同一 overlay 解析），供面板按会话读；handler 不编路径。 |
| 装配 | `cmd/keel/serve.go` | `TaskPanel: taskLog`、`FilePreview: filePreviewReader{opts.FileService}`，都是工具已在用的服务。 |

**没有新真值**：`internal/store/schema.sql`、`internal/pluginpkg/**`、
`plugins/keel.plugin-market/catalog.json`、`cmd/r1diff`、`plugins/keel.oar/**`、
`internal/harnessext/**` 均未改动；面板没有任何写路由。

### 1.2 前端（web，纯投影）

| 件 | 文件 | 行为 |
| --- | --- | --- |
| 折叠与状态 | `web/src/data/tasks.ts`（新） | `taskTree`（own 卡按创建序、成员按读数序、外层父卡置顶、`externalParent` 标"另一会话"）、`forkBranches`、`eventLines`/`eventDetail`（每种 kind 只说自己带的字段）、`panelGaps`（每个上限一句、外来 kind 只报一次）、`panelPhase`（五态）、`resourceChanged`/`previewSummary`、`shouldShowTaskPanel`；`taskPanelAccess` 单例（同一 `store.identityApi()` seam，读一次/会话，`ensure` 对重入与失败重读有守卫）。 |
| 面板 | `web/src/screens/books/task-panel.tsx`（新） | `TaskPanelBody`（五态 + 图 / 事件流 / 资源三视图）；`TaskPanelEntry`（运行页一行入口）；`TaskPanelSheet`（390 的底部 Sheet）。全部 kit 组件 + solid-glass 令牌，无硬编码色值，无新顶部栏元素（A4.4f 未动）。 |
| 入口 | `web/src/screens/books/run.tsx` | 运行页纸面上的一个低噪声 band：`run-task-panel-entry`；1440 展开在 run paper 内（`run-task-panel-band`），≤767 走 `TaskPanelSheet`。不新增章节，不改 `shell.tsx`/`App.tsx`/`settings.tsx`（W12 与身份 lane 的文件未碰）。 |
| 数据 seam | `web/src/data/{contract,http,identity-api,store,fixture,fixture-identity}.ts` | `TaskPanelView`/`FilePreviewView` wire 类型；`taskPanel`/`filePreview` 解析（缺字段保持缺）；`IdentityApi`/`Client` 可选方法（部署未接则入口消失）；fixture 真状态：captain 两张 own 卡、sub-agent 成员、fork 成员（`originSeq 12`）、两份 attach 资源与预览内容；fixture `capabilities` 报 `task.panel: true`。**fixture 的 fork 父卡是 captain 卡**，与投影的 join 语义一致（fork 挂在成员卡下时，读不到；见 §5 注）。 |

### 1.3 五态与响应式

| 态 | 触发 | 呈现 |
| --- | --- | --- |
| loading | 首读未回 | `task-panel-loading`（PageLoading 骨架，不显示假卡） |
| empty | 会话无任何卡/事件 | `task-panel-empty` + 刷新 |
| error | 非 403 失败 | `task-panel-error`，带服务端 requestId 与重试 |
| permission | 403 | `task-panel-permission`，四段式（缺的键、找谁、下一步） |
| success | 有读数 | 三视图 + 计数 + 缺口说明 |

1440：纸面上一条 band（宽 1040 内，检查器列不动）。390：入口仍是单行，
面板进底部 Sheet；单列、44px 触控位、无横向溢出（见 §3）。

## 2. 测试与证据

### 2.1 Go

```
go build ./...                                   # pass
go vet ./internal/tasklog ./internal/harnessapi ./internal/fileplugin ./cmd/keel
go test ./internal/tasklog  -count=1              # ok (0.5s；PG 套件无 DSN 时 skip)
go test ./internal/fileplugin -count=1            # ok (193s)
go test ./internal/harnessapi -count=1            # ok (2.5s；含新 panel/preview 路由测试)
go test ./cmd/keel -count=1                       # ok (1.1s)
go test ./internal/architecture -count=1          # ok (14.8s；内核不 import 产品层仍成立)
```

新增测试：

- `internal/tasklog/panel_memory_test.go`：**无数据库**证明折叠语义——
  树序、跨会话成员、外层祖先、attachment 折叠只读本会话事件、
  成员页/事件页截断计数、own child 不占成员名额、坏 payload 使读失败。
  该套件同时把 `Panel` 的只读性做成结构断言：fake Projection 的
  `UpsertCard`/`InsertEvent`/`ReplaceSession` 直接 panic。
- `internal/tasklog/panel_test.go`：PostgreSQL 集成（`KEEL_TEST_DSN`）——
  own 卡序、成员卡行在子会话、fork 的 `origin=fork`/`originSeq`/`parentTaskId` 与
  子会话把原点卡报成 ancestor、attach/detach 的 live 集与历史、
  事件流账本序与外来 kind、界与租户边界、坏行拒绝。**本机 Docker/PostgreSQL
  在本次会话不可用（`127.0.0.1:13741` 关闭），该文件在本机 skip；需在有
  `KEEL_TEST_DSN` 的环境复跑。**
- `internal/harnessapi/task_panel_test.go`：路由 wire 形状（own/member/ancestor
  标记、fork 字段、附件、事件、limits）、未接 501 + capability false、
  外部会话 404、越界 limit 400；file-preview 的 200/404/文件已变化
  三态与 `session.filePreview` capability。

### 2.2 TUI（`nsh test --target tui`）

```
nsh test --target tui src/data/tasks.test.ts            # 10/10
nsh test --target tui src/screens/books/task-panel.tui.test.tsx   # 6/6
nsh test --target tui src/screens/books/bots-teams.tui.test.tsx   # 22/22（回归）
```

面板 TUI 覆盖：

1. 运行页入口走真实 fixture 读数；图含根卡、own child、成员卡与
   `分叉原点 · seq 12`；
2. 三视图：事件流带 kind（创建/挂载资源/注记）与卡名、按卡过滤与「看全部」；
   资源列表列出两条 `(path, hash)`；
3. 资源预览按会话清单打开，`与引用一致`、尺寸行、内容可见；
4. 五态：loading / empty / error（请求编号 `k-panel-1`）/ permission（`查看会话`）；
5. 资源预览的「文件已变化」态：说明"下面是当前内容，不是卡记录的那一份"，
   且不出现"与引用一致"；
6. 390：入口在、展开进 Sheet、单列、`run-task-panel-band` 不出现。

### 2.3 浏览器（1440 / 390，真 fixture）

打包产物 `nsh package --target web`，静态托管在 `127.0.0.1:13752`，
`?fixture=1`，headless Chrome（playwright-core）：
`scripts/keel-w4-taskpanel-e2e/browser.mjs` → `evidence/w4-task-panel/report.json`。

| 步骤 | 断言 | 结果 |
| --- | --- | --- |
| 团队页 → 启动 | `team-roster`、启动回执提示 | pass |
| 运行页 | `task-panel` 入口、`run-task-panel-entry` | pass |
| 任务图 | `task-panel`、`task-captain`、`task-refund-fork`（分叉原点 seq 12） | pass |
| 事件流 | `task-panel-events`（创建/挂载资源/注记，全局 seq 21…） | pass |
| 资源预览 | `task-panel-resources`、打开 `reports/refund-analysis.md`、`与引用一致` | pass |
| 横向溢出 | 1440/1440/1440/1440、390/390/390/390（doc/body scrollWidth ≤ innerWidth） | pass |
| 控制台 | console errors = 0，page errors = 0，failed requests = 0 | pass |

截图（相对本文件）：

- `w4-task-panel/run-entry-1440.png` — 运行页与面板入口（计数 7 卡 · 17 事件）。
- `w4-task-panel/task-panel-graph-1440.png` — 任务图 + 分叉原点。
- `w4-task-panel/task-panel-events-1440.png` — 事件流（挂载资源带 path 与短 hash）。
- `w4-task-panel/task-panel-preview-1440.png` — 资源预览（与引用一致、1 KB · 7/7 行）。
- `w4-task-panel/task-panel-390.png` — 390 单列（入口在，无溢出）。
- `w4-task-panel/report.json` — 断言、溢出数字、console/page error。

## 3. 负控（都能判失败）

| 控制 | 期望 | 证据 |
| --- | --- | --- |
| 面板写任务状态 | 不可能：`Panel` 只查 projection；fake writer 方法 panic | `panel_memory_test.go` |
| 坏 `task.attached` payload（无 path/hash） | 读失败（不静默成空事件） | `panel_memory_test.go` / `view.go` |
| 外来 kind | 保留行 + `Unparsed`，只显示 kind 与序号；`panelGaps` 报一次 | `tasks.test.ts` / `task_panel_test.go` |
| 路径不在会话清单 | 404（不编路径、不读别处） | `task_panel_test.go` |
| 引用的文件已变 | 给当前内容 + `matchesReference:false` + "文件已变化" | `task_panel_test.go` / TUI 5 |
| 越界 `memberLimit`/`eventLimit` | 400（不是静默 clamp） | `task_panel_test.go` |
| 外部/不存在会话 | 404，先于投影读 | `task_panel_test.go` |
| 部署未接投影 | 501 + `task.panel:false`（入口不出现） | `task_panel_test.go` |
| own child 占成员名额 | 不占；成员页精确计数 | `panel_memory_test.go` |
| 面板打开后内存回落 / 新增常驻进程 | 无新 daemon；浏览器脚本只用一次性 Chrome 与静态服务器 | §4 回收 |

## 4. 进程与端口

- 静态服务器：`python3 -m http.server 13752`（临时，非登记端口；收尾已停）。
  `13752` 未进 `PORT.md`，属**一次性证据进程**，收尾 `kill`。
- 无其它常驻进程；Docker/`keel-pg` 在本会话不可用，未启动。
- 测试库：PG 套件在本机未跑（DSN 缺省 skip）；fixture/TUI 全内存，
  无残留库。有 DSN 的环境跑 `panel_test.go`/`team_tasks_pg_test.go` 时
  `openTestStore` 自建库并在 cleanup `DROP DATABASE ... WITH FORCE`。

## 5. 说明与遗留

1. **成员卡的子卡**（fork 挂在成员卡下）：投影的 `ListChildCards` 以
   "父卡属于本会话" 为 join 条件，因此本会话只看得到直接成员卡；成员卡自己的
   子卡（哪怕同租户）在本会话面板上不可见，需打开它的会话看。这是投影的
   真实边界，面板不复制别会话的卡来补——fixture 的 fork 卡因此挂在 captain
   卡下（与真投影一致）。若后续要让"父卡视角"看到二级分支，应在
   `internal/tasklog` 增加一次按 `parentTaskId` 集合的递归读（additive），
   而不是在面板拼。
2. **`task.panel` 的装配**：`cmd/keel/serve.go` 注入了两个 seam；本机未连
   PG，未跑真 `keel serve` 路由。`internal/harnessapi` 的路由测试用 fake
   projection 覆盖 wire；PG 集成在 `internal/tasklog/panel_test.go`（需 DSN）。
3. **事件时间**：`task_events` 的 `created_at` 是物化时间，不是账本事件时间；
   面板按 `seq` 排序与显示（`at` 只作参考）。要精确事件时刻，需要
   `kernel.Event` 的时间字段进投影（additive，未做）。
4. **图片/二进制资源**：`file-preview` 给的是按行的文本页；本切片不渲染图片
   （blob 字节走 `GET /v1/files/{id}`，attached hash 与 blob id 不是同一命名空间）。
   面板的资源行如实只给 `(path, hash)` 与文本页。
5. **未改**：`openspec/changes/keel-harness-v2/tasks.md`（维护者收口时勾
   `面板：任务图、事件流、资源预览；纯投影`）。

## 6. 本切片文件

- 新：`internal/tasklog/{view,panel,panel_test,panel_memory_test}.go`、
  `internal/harnessapi/{task_panel,task_panel_test}.go`、
  `web/src/data/{tasks,tasks.test}.ts`、
  `web/src/screens/books/{task-panel,task-panel.harness,task-panel.tui.test}.tsx`、
  `scripts/keel-w4-taskpanel-e2e/browser.mjs`、本目录。
- 改：`internal/tasklog/{service,pg}.go`、`internal/store/tasks.go`、
  `internal/fileplugin/service.go`、`internal/harnessapi/api.go`、
  `cmd/keel/serve.go`、`web/src/data/{contract,http,identity-api,store,fixture,fixture-identity}.ts`、
  `web/src/screens/books/run.tsx`。
- 未碰：`shell.tsx`、`settings.tsx`、`App.tsx`（W12/身份 lane 的文件）、
  `internal/pluginpkg/**`、`plugins/keel.plugin-market/catalog.json`、
  `cmd/r1diff`、`internal/store/schema.sql`、`plugins/keel.oar/**`、
  `internal/harnessext/**`。
