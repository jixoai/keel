# W4 · 任务面板：任务图 / 事件流 / 资源预览（纯投影）

Date: 2026-11-23. Change: `keel-harness-v2`, W4 最后一项
（`tasks.md`: 面板：任务图、事件流、资源预览；纯投影，不写额外真值）.
Worktree: `.worktrees/keel-w4-taskpanel`, branch `task/keel-w4-taskpanel`.

第二轮收口（同日）：事件页改为保留**最新**一页；成员截断计数在读数停在自己
上界时是 floor=1 而不是伪造精确值；资源视图增加会话清单的按卡归属组
（`lastTaskId`）及失败/空/未接三态。见 §1.1、§1.2、§2、§3。

第三轮（W4 follow-up，同日）：清单改为 `limit/cursor` 分页并带 manifest hash；
own cards / 事件流 / attach 历史均有界读取；fixture 对齐投影；0 字节、短 hash、
floor-1 文案修正。本文旧描述已按当前代码更正，完整证据见
`evidence/w4-followups/w4-followups.md`。

本证据只覆盖**面板这一项**。W11 的 alpha 切片
「任务创建 → bash 执行 → capture → 面板可见 → fork → 合并 → 卸载插件零残留」里，
面板可见这一段由本切片交付；其余段（bash/capture/fork/合并/卸载）各自的状态见
`tasks.md` 与 `RECONCILIATION.md`，本文件**不声称 W11 已完成**。

## 1. 交付了什么

### 1.1 读侧投影（Go）

| 件 | 文件 | 行为 |
| --- | --- | --- |
| 事件解码 | `internal/tasklog/view.go`（新） | `DescribeEvent(Event)` 把已物化的 `task_events` payload 解成面板字段（title/status/parent/origin/originSeq/path/hash/text/reason）。kind 与 seq 只取自行本身，payload 不能改自己的 kind；不在六种词汇里的 kind 标 `Unparsed` 保留，不丢行；**契约不符的 payload 是错误**而不是"空事件"。 |
| 面板折叠 | `internal/tasklog/panel.go`（新） | `Service.Panel(scope, memberLimit, eventLimit)`：本会话 own cards（按 createdSeq 排序）＋ `parentTaskId` 指过来的成员卡（跨会话，行仍在子会话）＋ 会话外父卡解析成 `Ancestors`＋每人卡的 attach/detach 折叠成 live `Attachments`＋整会话 task 事件流。事件页超过上界时**保留最新的一页**（`EventsTruncated` 说更早的没读入）；成员页的 `TruncatedMemberCount` 在 over-fetch 读完整个关联集时是精确未画数，在读本身停在自己的上界时是 floor=1（它数不到没读的行，不把「不知道」说成「没有」）。own child 卡不占成员名额（读时按 own 卡数 over-fetch）。**只依赖 `Projection`**：`Panel` 的守卫只查 projection，连 writer 都拿不到。 |
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
| 折叠与状态 | `web/src/data/tasks.ts`（新） | `taskTree`（own 卡按创建序、成员按读数序、外层父卡置顶、`externalParent` 标"另一会话"）、`forkBranches`、`eventLines`/`eventDetail`（每种 kind 只说自己带的字段）、`panelGaps`（每个上限一句、外来 kind 只报一次；成员截断不冒充精确总数：count>1 说"至少还有 N 张"，count=1 只说"至少 1 张"，不说"达到/上限"）、`panelPhase`（五态）、`resourceChanged`/`previewSummary`、`shouldShowTaskPanel`；`fileReferences`（会话清单按 `lastTaskId` 归属到本会话 own 卡；别的会话的卡不冒充；已挂载的 path 交给资源组，不重复打印；组序按树序）；`taskPanelAccess` 单例（同一 `store.identityApi()` seam，读一次/会话，`ensure` 对重入与失败重读有守卫）。 |
| 清单读数 | `web/src/data/{store,identity-api}.ts` | 面板与任务投影**并行**读会话清单 `GET /v1/sessions/{id}/files`，走同一个 `identityApi()` seam（fixture / harness / live 一致）；`files` / `filesAvailable` / `filesTruncated` / `filesError` 分开记录：失败不伪装成空清单，late answer 不落到已切换的会话，未接清单的部署整组不出现。 |
| 面板 | `web/src/screens/books/task-panel.tsx`（新） | `TaskPanelBody`（五态 + 图 / 事件流 / 资源三视图）；资源视图的 `task-panel-files` 组：按卡归属的清单文件、空态 `task-panel-files-empty`、失败态 `task-panel-files-error`、清单截断提示 `task-panel-files-truncated`；打开清单文件按 path 预览；清单行带 hash 时做真实比对（一致/文件已变化），只有行本身无 hash 才显示「清单当前版本」。`TaskPanelEntry`（运行页一行入口）；`TaskPanelSheet`（390 的底部 Sheet）。全部 kit 组件 + solid-glass 令牌，无硬编码色值，无新顶部栏元素（A4.4f 未动）。 |
| 入口 | `web/src/screens/books/run.tsx` | 运行页纸面上的一个低噪声 band：`run-task-panel-entry`；1440 展开在 run paper 内（`run-task-panel-band`），≤767 走 `TaskPanelSheet`。不新增章节，不改 `shell.tsx`/`App.tsx`/`settings.tsx`（W12 与身份 lane 的文件未碰）。 |
| 数据 seam | `web/src/data/{contract,http,identity-api,store,fixture,fixture-identity}.ts` | `TaskPanelView`/`FilePreviewView` wire 类型；`taskPanel`/`filePreview` 解析（缺字段保持缺）；`IdentityApi`/`Client` 可选方法（部署未接则入口消失）；fixture 真状态：captain 两张 own 卡、sub-agent 成员、fork 成员（`originSeq 12`）、两份 attach 资源与预览内容，外加一份**只有清单归属**的 `reports/refund-by-channel.csv`（summary 卡写入，没有 attach 事件）；fixture `capabilities` 报 `task.panel: true`。**fixture 的 fork 父卡是 captain 卡**，与投影的 join 语义一致（fork 挂在成员卡下时，读不到；见 §5 注）。 |

### 1.3 五态与响应式

| 态 | 触发 | 呈现 |
| --- | --- | --- |
| loading | 首读未回 | `task-panel-loading`（PageLoading 骨架，不显示假卡） |
| empty | 会话无任何卡/事件 | `task-panel-empty` + 刷新 |
| error | 非 403 失败 | `task-panel-error`，带服务端 requestId 与重试 |
| permission | 403 | `task-panel-permission`，四段式（缺的键、找谁、下一步） |
| success | 有读数 | 三视图 + 计数 + 缺口说明 |
| 清单失败 | 面板可读、`/files` 失败 | 资源区一条 `task-panel-files-error`（服务端 message），不把失败画成"没有文件"；面板其余部分照常 |
| 清单空 | 清单可读，但没有归属到本会话 own 卡的额外路径 | `task-panel-files-empty` 说明；已挂载的资源仍在上面的资源组里 |
| 清单未接 | transport 没有 `sessionFiles` | 整组不出现（没有空态、没有错误），入口照常 |

1440：纸面上一条 band（宽 1040 内，检查器列不动）。390：入口仍是单行，
面板进底部 Sheet；单列、44px 触控位、无横向溢出（见 §3）。

## 2. 测试与证据

### 2.1 Go

```
go vet ./internal/tasklog ./internal/harnessapi ./internal/fileplugin   # pass
go test ./internal/tasklog    -count=1            # ok (0.4s；PG 套件无 DSN 时 skip)
go test ./internal/harnessapi -count=1            # ok (2.8s；含 panel/preview 路由测试)
go test ./internal/fileplugin -count=1            # ok (54.7s)
```

本轮按切片只复跑上述目标包；`go build ./...`、`cmd/keel`、`architecture`
不在本轮复跑范围（16GB 主机不跑并行全仓套件）。

新增测试：

- `internal/tasklog/panel_memory_test.go`：**无数据库**证明折叠语义——
  树序、跨会话成员、外层祖先、attachment 折叠只读本会话事件、
  坏的 payload 使读失败。截断语义单列四组：
  `TestPanelEventPageKeepsTheNewest`（超界保留最新 N 条、恰好等于上界不截断、
  小于上界不截断）、`TestPanelMemberPageBounds`（全部装下不报截断；溢出一张时
  精确 count=1；limit=1 时读上界 3 只看到 4 个成员中的 3 个，count=2 而不是
  实际 3，证明计的是**读到的**未画行）、`TestPanelMemberPageIgnoresOwnChildren`
  （own child 不占名额、不制造截断）、
  `TestPanelMemberPageExactReadBoundFloorsTheCount`（500 行读满、overflow=0
  时仍 `Truncated=true` 且 count=floor 1）。
  该套件同时把 `Panel` 的只读性做成结构断言：fake Projection 的
  `UpsertCard`/`InsertEvent`/`ReplaceSession` 直接 panic。
- `internal/tasklog/panel_test.go`：PostgreSQL 集成（`KEEL_TEST_DSN`）——
  own 卡序、成员卡行在子会话、fork 的 `origin=fork`/`originSeq`/`parentTaskId` 与
  子会话把原点卡报成 ancestor、attach/detach 的 live 集与历史、
  事件流账本序与外来 kind、界与租户边界、坏行拒绝。复核时用一次性
  `postgres:18`（127.0.0.1:13742，`keel-w4-pg`）实跑：最初两处失败均为测试
  自身——`LinkSubagent` 取的是父会话最新的 active 卡（不是第一张），以及 fork
  测试没有先建子会话；两处已修。最终
  `KEEL_TEST_DSN=… go test ./internal/tasklog -count=1` → ok，容器已删除。
- `internal/harnessapi/task_panel_test.go`：路由 wire 形状（own/member/ancestor
  标记、fork 字段、附件、事件、limits）、未接 501 + capability false、
  外部会话 404、越界 limit 400；file-preview 的 200/404/文件已变化
  三态与 `session.filePreview` capability。

### 2.2 TUI（`nsh test --target tui`）

```
nsh test --target tui src/data/tasks.test.ts            # 11/11
nsh test --target tui src/screens/books/task-panel.tui.test.tsx   # 8/8
```

（`bots-teams.tui.test.tsx` 的 22/22 回归属于上一轮记录；本轮只复跑面板
两个套件。）

面板 TUI 覆盖：

1. 运行页入口走真实 fixture 读数；图含根卡、own child、成员卡与
   `分叉原点 · seq 12`；
2. 三视图：事件流带 kind（创建/挂载资源/注记）与卡名、按卡过滤与「看全部」；
   资源列表列出两条 `(path, hash)`；文件清单组列出 summary 卡写的
   `reports/refund-by-channel.csv`，已挂载的 `refund-report.json` 不在组里重复；
3. 资源预览按会话清单打开，`与引用一致`、尺寸行、内容可见；
4. 五态：loading / empty / error（请求编号 `k-panel-1`）/ permission（`查看会话`）；
5. 资源预览的「文件已变化」态：说明"下面是当前内容，不是卡记录的那一份"，
   且不出现"与引用一致"；
6. 清单归属文件可直接预览：带 manifest hash 时徽标 `与引用一致`（hash 不匹配
   则 `文件已变化`）；只有清单行本身无 hash 才显示 `清单当前版本`；内容是该 CSV 的当前页；
7. 清单三态：失败显示 `task-panel-files-error` 且不出现文件组；空清单显示
   `task-panel-files-empty`；transport 未接则整组不出现；
8. 390：入口在、展开进 Sheet、单列、`run-task-panel-band` 不出现；切到资源
   视图后归属文件行仍在。

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
| 文件归属组 | `task-panel-files`、`task-panel-file-reports/refund-by-channel.csv` | pass |
| 挂载资源不重复 | `task-panel-file-reports/refund-analysis.md` count = 0（它只在资源组） | pass |
| 清单文件预览 | 打开 `refund-by-channel.csv`：manifest hash 与清单行一致时 `与引用一致` + `渠道,退款单,占比` | pass |
| 横向溢出 | 1440/1440/…/390/390（doc/body scrollWidth ≤ innerWidth） | pass |
| 控制台 | console errors = 0，page errors = 0，failed requests = 0 | pass |

截图（相对本文件）：

- `w4-task-panel/run-entry-1440.png` — 运行页与面板入口（计数 7 卡 · 17 事件）。
- `w4-task-panel/task-panel-graph-1440.png` — 任务图 + 分叉原点。
- `w4-task-panel/task-panel-events-1440.png` — 事件流（挂载资源带 path 与短 hash）。
- `w4-task-panel/task-panel-preview-1440.png` — 资源预览（与引用一致、1 KB · 7/7 行）。
- `w4-task-panel/task-panel-390.png` — 390 单列（入口在，无溢出）。
- `w4-task-panel/task-panel-preview-at-0ad61e2-1440.png` — 旧版本预览截图（来自
  `0ad61e2`），仅作历史状态参考；它不是同视图的 before 对照，本次未采集同视图
  before（原文件名为 `task-panel-resources-before-1440.png`，已按事实改名）。
- `w4-task-panel/task-panel-resources-after-1440.png` — **改后**：新增
  `文件清单 · 按卡归属（lastTaskId）` 组、CSV 行与带 hash 的清单预览。
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
| own child 占成员名额 | 不占；成员页校验 over-fetch 过滤 | `panel_memory_test.go` |
| 事件页截断方向反了（留最旧） | 超界留最新 N 条且 `EventsTruncated=true` | `TestPanelEventPageKeepsTheNewest` |
| 成员截断把没读到的行算进 count | 读停在上界时 count 只报读到的未画行 / floor 1 | `TestPanelMemberPageBounds` / `…ExactReadBoundFloorsTheCount` |
| 清单里的文件归属到别的会话卡 | 不列（只认本会话 own 卡） | `tasks.test.ts` `fileReferences` |
| 清单文件已挂载还重复列一次 | 不重复；同一 (taskId, path) 交给资源组 | `tasks.test.ts` / TUI 2 |
| 清单读失败被画成"没有文件" | `task-panel-files-error`，消息来自服务端；不出现空态 | TUI 7 / harness `panel-files-error` |
| 清单文件没有引用 hash 却显示"与引用一致/未核对" | 徽标 `清单当前版本`；请求不带空 hash | TUI 6 / harness / browser |
| 面板打开后内存回落 / 新增常驻进程 | 无新 daemon；浏览器脚本只用一次性 Chrome 与静态服务器 | §4 回收 |

## 4. 进程与端口

- 静态服务器：`python3 -m http.server 13752`（临时，非登记端口）。
  `13752` 未进 `PORT.md`，属**一次性证据进程**。第二轮收口时发现上一轮中断留下了
  旧进程（PID 74644，`/private/tmp/w4-task-panel-web/Keel`）：先停旧进程、重新
  `nsh package` 到 `/tmp/w4-task-panel-web`、起一次性静态服务跑浏览器脚本（最终
  截图来自第二次重跑，去掉脚本里的 debug 截图步骤后），收尾 `kill` 并删除
  `/tmp/w4-task-panel-web`；`lsof -iTCP:13752` 现无监听。
- 无其它常驻进程；复核时发现证据脚本遗留的一只 headless Chrome（pid 42823,
  `--remote-debugging-port=9231`）已回收。一次性测试 Postgres 容器
  `keel-w4-pg` 已 `docker rm -f`，13742 无监听。
- 测试库：复核用一次性 Postgres 18 跑通 `panel_test.go`；fixture/TUI 全内存，
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
6. **清单归属的边界**：`fileReferences` 只认本会话 own 卡的 `lastTaskId`；
   成员卡（行在子会话）与祖先卡不参与，因为它们的文件写进的是自己的会话清单。
   `/v1/sessions/{id}/files` 的路由默认只读前 200 行（`limit` 上限 1000，本
   transport 未传参），清单被截断时面板显示 `task-panel-files-truncated`；
   要枚举更多需后续给读数加分页参数（additive）。
7. **清单文件没有引用 hash**：清单行只有 (path, size, updatedSeq, lastTaskId)，
   没有 attach 事件记录的 hash，所以打开时按 path 预览、不传 `hash` 参数，
   徽标是「清单当前版本」而不是「与引用一致/已变化/未核对」；要精确核对
   需要清单读数将来带内容 hash（additive）。

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
- 本轮收口（面板截断语义 + 清单归属组）：`internal/tasklog/panel.go`、
  `internal/tasklog/panel_memory_test.go`、`internal/harnessapi/task_panel.go`、
  `web/src/data/{tasks,tasks.test,store,identity-api,fixture,fixture-identity}.ts`、
  `web/src/screens/books/{task-panel,task-panel.harness,task-panel.tui.test}.tsx`、
  `scripts/keel-w4-taskpanel-e2e/browser.mjs`、本文件与
  `w4-task-panel/{report.json,task-panel-*.png,task-panel-resources-{before,after}-1440.png}`。
- 未碰：`shell.tsx`、`settings.tsx`、`App.tsx`（W12/身份 lane 的文件）、
  `internal/pluginpkg/**`、`plugins/keel.plugin-market/catalog.json`、
  `cmd/r1diff`、`internal/store/schema.sql`、`plugins/keel.oar/**`、
  `internal/harnessext/**`。
