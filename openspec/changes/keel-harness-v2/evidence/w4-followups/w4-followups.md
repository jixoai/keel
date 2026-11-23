# W4 复核 P2 收口（w4-followups）

Date: 2026-11-23. Change: `keel-harness-v2`，W4 面板的复核 P2。
Worktree: `.worktrees/keel-w4-followups`，branch `task/keel-w4-followups`，
base `main 9480bf8`。提交：`11a96f6`（后端分页/有界读）、`6de2ba2`（web 翻页/hash/
文案）、`cfff353`（证据）、`1c09863`（harness 类型守卫）、`0baea81`（attach fold
坏行失败闭合）。

本切片只收口 W4 面板的 5 个 P2；不新增面板真值、不写任务状态、不改账本。
测试用本机单实例 PostgreSQL 18（`KEEL_TEST_DSN=postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable`），
无 Docker，串行执行。

## 1. 收口了什么

### 1.1 P2-1 会话清单有界翻页

| 层 | 行为 |
| --- | --- |
| 服务端 `internal/harnessapi/completion.go` | `GET /v1/sessions/{id}/files?limit&cursor`：按 path 升序，`cursor` 是上一页最后一条 path（**排他**，下一页从 `path > cursor` 开始）；回答新增 `hasMore` / `nextCursor`，`truncated` 保留原义（后面还有页）。`limit` 仍为 1..1000（默认 200），`cursor` 长度上限 1024，越界都是 400，不静默 clamp。 |
| 客户端 `contract/http/identity-api/store` | `SessionFilesQuery {limit, cursor}`、`SessionFilesView.nextCursor/hasMore/limit`；`taskPanelAccess` 以 500 条一页读首屏，`loadMoreFiles` 读下一页并用 `mergeFilePage` 按 path 合并（同一 path 以新页为准，不重复）。 |
| 面板 `task-panel.tsx` | 资源组的文件清单在 `nextCursor` 存在时显示「加载更多文件」；`filesMoreError` 在已有行下面单独报错（读下一页失败不隐藏已读行）；只有「截断但没有游标」才显示不可翻页的缺口句。 |

测试：

- `internal/harnessapi/completion_test.go`：三页走完（a,b → c,d → e）、游标排他（`cursor=e` 为空页）、
  游标指向已删除 path 仍是合法边界、`hasMore/nextCursor/truncated` 形状、越界 cursor/limit 400、
  **无 hash 的行不发 hash 字段**。
- `task-panel.tui.test.tsx`「文件清单翻页」：`panel-files-paged` 第一页一行 + 游标，加载后两行都在，
  按钮消失，且没有假的截断句。

### 1.2 P2-2 清单行带内容 hash

| 层 | 行为 |
| --- | --- |
| 服务端 | `SessionFileView.Hash` 取 manifest 的 `layerfs.Hash`，仅在 `Valid()` 时以 `sha256:<hex>` 输出；没有 hash 的行不编造。 |
| 客户端 | `SessionFileView.hash`、`FileReference.hash` 原样传递；文件清单组打开预览时把 hash 作为 reference 发出，预览据此回答 `matchesReference`。 |
| 面板 | 清单文件预览的徽标现在是「与引用一致 / 文件已变化」；只有清单行**没有** hash 时才显示「清单当前版本」，不冒充核对。 |

测试：Go 路由测试断言有效 hash 前缀化、空 hash 不出现；`tasks.test.ts` 断言 `fileReferences` 透传/缺省；
TUI 用 CSV（有 hash）断言「与引用一致」，用 `reports/unhashed.csv`（无 hash）断言「清单当前版本」；
浏览器 e2e 断言 CSV 预览为「与引用一致」。

### 1.3 P2-3 面板读数有界、旧坏行不拖垮面板

| 读数 | 界 | 截断事实 |
| --- | --- | --- |
| own 卡 | `ListPanelCards` 按 `created_seq, created_at, task_id` 在 SQL 里 `LIMIT cardLimit+1`，保留最早的一页（根卡不会因上限被丢掉） | `limits.cardLimit` / `cardsTruncated`，前端缺口句 |
| 事件流 | `ListSessionEvents(limit=eventLimit+1)` 只取最新一页（SQL 子查询 `ORDER BY seq DESC LIMIT` 后恢复账本序）；**只解码这一页** | `eventsTruncated`（原有） |
| attach/detach 折叠 | 独立的 `ListSessionAttachmentEvents(limit=attachmentLimit+1)`，只读最新一页挂载/解绑历史 | `limits.attachmentLimit` / `attachmentsTruncated` |

- 旧行（例如一次修复遗留的坏 payload）不在最新页时**既不被读取也不被 `DescribeEvent` 解码**，
  面板照常回答；同一行落在请求的页内仍然是错误（不静默丢可见事实）。
- 资源折叠仍然按卡、按账本序进行；页外更早的引用不会被虚构，`attachmentsTruncated` 明说可能缺失。

测试：

- `panel_memory_test.go`：`TestPanelCardPageIsBounded`（1001 张卡 → 1000 张、根卡保留）、
  `TestPanelAttachmentPageIsBounded`（1001 条 attach → 1000 条、最旧一条不在）、
  `TestPanelOldCorruptEventOutsideThePageDoesNotFail`（页外坏行成功、页内坏行失败）。
- `panel_test.go`（PG）：`TestPanelOldCorruptProjectionRowOutsideThePage` 用一条负 seq 的坏投影行
  证明 SQL 只取最新页；同一页大小时失败；同时断言 `ListTaskCardRowsInCreationOrder` 的 SQL LIMIT。
- `tasks.test.ts`：新增卡页/资源折叠页的缺口句断言。

### 1.4 P2-4 fixture 保真

`FixtureIdentity.taskPanel` 现在是投影的折叠，而不是把样例任务全塞进成员：

- own 卡 = `sessionId === 被问会话` 的行（captain run 用 `CAPTAIN_TASKS`，其余样例会话用 `this.tasks`）；
- 成员 = 父卡是**本会话 own 卡**、行在别的会话的卡，按 projection 的顺序（updatedAt desc, taskId desc）；
- 祖先 = own 卡/成员卡的 `parentTaskId` 指向本会话之外时，从 fixture 已知卡里解析，链不往上走；
- 事件流只来自被问会话的 own 卡，成员卡的事件不抄进来；
- 事件页保留最新 N；`memberLimit` 截断给出精确 `truncatedMemberCount`（内存里读全了）；
- limits 现在也带 `cardLimit` / `attachmentLimit`。

分段计数：`resourceCount = attachments + fileReferences`，资源预览段不再漏掉「文件清单 · 按卡归属」
组里的行（`tasks.test.ts` + `PanelSegmented`）。

测试：`web/src/data/fixture-identity.test.ts`（成员只认真子卡、事件不跨会话、祖先/parentTaskId、
最新 N 页）；`task-panel.tui.test.tsx` 断言 `task-knowledge`/`task-aftersales` 不被画成本会话成员。

### 1.5 P2-5 客户端小谎言

- `previewSummary`：`size < 1024` 直接用字节（`0 B`、`300 B`），不再 `Math.max(1, …)` 把空文件说成 `1 KB`；
  KB/MB 档保持原格式。
- `shortHash`：只有 `^sha256:[0-9a-f]{64}$` 才截成 `sha256:xxxxxxxxxx…`；别的 token 原样打印，
  不截成另一个 digest，也不加一个它没有的 `sha256:` 前缀。
- `panelGaps` 的 count=1 句：改为「成员卡还有没画出来的（至少 1 张）；这次读取停在了自己的上界，数不到更多。」
  —— floor=1 可能来自读上界而非成员页上限，不再断言「达到 N 张上限」。

测试：`tasks.test.ts`（0/300 B、MB、非 sha256、count=1 句不含「达到/上限」）；
TUI 预览行断言 `\d+ B · n/n 行` 且不出现 `1 KB`。

## 2. 测试与证据

```
go build ./...                                             # pass
go vet ./internal/tasklog ./internal/store ./internal/harnessapi  # pass
KEEL_TEST_DSN=… go test ./internal/tasklog    -count=1      # ok 7.4s
KEEL_TEST_DSN=… go test ./internal/harnessapi -count=1      # ok 46.3s
KEEL_TEST_DSN=… go test ./internal/fileplugin -count=1      # ok 70.5s
```

```
nsh test --target tui src/data/tasks.test.ts                    # 15/15
nsh test --target tui src/data/fixture-identity.test.ts         # 3/3
nsh test --target tui src/screens/books/task-panel.tui.test.tsx # 9/9
```

浏览器：`nsh package --target web` → 一次性静态服务 `127.0.0.1:13753` →
`scripts/keel-w4-taskpanel-e2e/browser.mjs` → `report.json`。32 步全过，
console errors = 0，page errors = 0，failed requests = 0。截图（相对本文件）：

- `run-entry-1440.png` — 运行页与面板入口（当前计数 3 卡 · 6 事件，成员只有 fork）。
- `task-panel-graph-1440.png` — 树 + 分叉原点（`seq 12`）。
- `task-panel-events-1440.png` — 事件流。
- `task-panel-preview-1440.png` — 挂载资源预览（与引用一致）。
- `task-panel-resources-after-1440.png` — **改后**资源页：文件清单组 + CSV 预览
  「与引用一致」、`30 B · 4/4 行`（不再是 1 KB）。
- `task-panel-390.png` — 390 单列，无横向溢出。

关于测试环境的两点说明：

- 全量 `go test ./internal/store` 在一次运行里 `TestBootstrapTokenOnceAndAtomicSuperAdmin`
  报 `bootstrap already complete`（共享 `postgres` 库上的残留状态），单独跑该测试通过（1.5s）；
  本切片未改 store 的 bootstrap/auth 代码，最终门禁是上面 4 个目标包。
- 浏览器脚本自身创建的一次性 Chrome 由脚本 `browser.close()` 回收；收尾核对时看到的
  其它 headless Chrome 属于另一个 `lint-ui-runtime` 进程（不是本切片启动的），未触碰。

## 3. 负控（都能判失败）

| 控制 | 期望 | 证据 |
| --- | --- | --- |
| 旧坏事件在最新页之外 | 面板成功，只解码最新页 | `TestPanelOldCorruptEventOutsideThePageDoesNotFail` / PG 同名测试 |
| 坏事件落在页内 | 读失败，不静默丢 | 同上 |
| 翻页游标越界 / limit=0/1001 | 400，不 clamp | `completion_test.go` |
| 清单行没有 hash | 不发 hash 字段；预览徽标「清单当前版本」 | `completion_test.go` / TUI 文件归属 |
| 0 字节预览 | 显示 `0 B`，不显示 `1 KB` | `tasks.test.ts` / TUI |
| 非 sha256 token | 原样显示，不重新加前缀 | `tasks.test.ts` |
| 成员 count=1 | 只说「至少 1 张」，不说达到上限 | `tasks.test.ts` |
| 成员卡父在别的会话 | 不画成本会话成员 | `fixture-identity.test.ts` / TUI |
| 第一页还有下一页 | 加载更多可用；加载完按钮消失 | TUI「文件清单翻页」 |

## 4. 进程与端口

- 一次性静态服务器 `127.0.0.1:13753` 已 `kill`，`lsof` 无监听；`/tmp/w4-followups-web` 已删除。
- 没有 Docker；PG 测试通过 `KEEL_TEST_DSN` 连接本机实例，`store.Close` 在 cleanup 关闭。
- 本切片启动的浏览器由脚本关闭；未留下本切片的 daemon/服务器。

## 5. 仍未做 / 边界

- 5 个 P2 都已收口，无剩余 W4 复核 P2。
- `/files` 的分页契约在路由层；`fileplugin.Service.List` 仍是既有的「返回该会话 manifest」seam。
  若将来 manifest 本身出现病态行数，下一步才是给 Manifest 增加 SQL 级 path 游标读（本切片未改）。
- fixture 的 `src/composer.tsx` / `docs/release.md` 没有字节内容，因此没有 hash；它们不归属任何卡，
  不进入面板文件组。客户端对「无 hash」的处理有正反两条测试。
- 面板仍不渲染图片/二进制（只给文本页）；这是 W4 既有边界，不在本次 P2 内。

## 6. 改动文件

- 后端：`internal/harnessapi/{completion,completion_test,task_panel,task_panel_test}.go`、
  `internal/tasklog/{panel,panel_memory_test,panel_test,pg,service}.go`、`internal/store/tasks.go`。
- 前端：`web/src/data/{contract,http,identity-api,store,fixture,fixture-identity,fixture-identity.test,tasks,tasks.test}.ts`、
  `web/src/screens/books/{task-panel,task-panel.harness,task-panel.tui.test}.tsx`。
- 证据脚本：`scripts/keel-w4-taskpanel-e2e/browser.mjs`。
- 文档：本文件、`report.json`、截图；`tasks.md` 的 W4 面板与 P2 收口段。
