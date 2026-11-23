# W12 产品切片证据：session-title / session-query / deliverables

日期：2026-11-23。分支：`task/keel-w12-product`（rebase 到 main `a2640b8` =
`a549e87` + W11 + W3 + W2；原切片基于 `9480bf8`）。

## 交付范围

| 能力 | 代码 | 规格 |
|---|---|---|
| `keel.session-title` | `internal/sessiontitle`、`internal/harnessapi/sessions.go`、`web/src/ui/session-title.ts` | `openspec/changes/keel-harness-v2/specs/session-title/spec.md` |
| `keel.session-query` | `internal/harnessapi/session_query.go`、`internal/harnessapi/session_query_pg.go`、`web/src/data/session-query.ts`、`web/src/screens/session-list.tsx`、`web/src/screens/session-status-pane.tsx` | `.../specs/session-query/spec.md` |
| `keel.deliverables` | `internal/harnessapi/session_query.go`（deliverables 段）、`web/src/data/deliverables.ts`、`web/src/screens/session-status-pane.tsx` | `.../specs/deliverables/spec.md` |

三项都是 core 能力：没有新增同名插件清单/插件实体（不伪造 manifest）。`keel.attachment`
明确留作后续，未在本切片实现。

## 可执行证据（产物文件在本目录）

| 文件 | 命令 | 结果 |
|---|---|---|
| `go-build.txt` | `go build -o /tmp/keel-w12-build ./cmd/keel && go vet ./internal/harnessapi ./internal/sessiontitle ./internal/accessgate` | 无输出（成功）。**必须带 `-o`**：仓根的 `keel` 是入库二进制，裸 `go build ./cmd/keel` 会覆盖它（复核时踩到并已恢复） |
| `go-targeted.txt` | `KEEL_TEST_DSN=postgres://kzf@127.0.0.1:5432/postgres?sslmode=disable go test ./internal/sessiontitle ./internal/accessgate ./internal/harnessapi -run 'TestDerive|TestSession|TestDeliverables|TestBoundedExport|TestPGSession|TestMatrix|TestCheckRoutes|TestTaskPanelRoutesAreDeclared' -count=1 -p 1` | 3 包全 ok |
| `go-targeted-final.txt` | 同 DSN，最终一轮 W12 用例 + 既有列表/标题用例 | 3 包全 ok |
| `go-pg-query-after-index.txt` | 同上 DSN，`go test ./internal/harnessapi -run TestPGSessionQuery -count=1 -p 1` | ok（含迁移后的 `events_transcript_created` 索引） |
| `go-store-migrate.txt` | 同上 DSN，`go test ./internal/store -run 'TestMigrate|TestSchema|TestRLS|TestTenant'` | ok |
| `nsh-check.txt` | `cd web && nsh check`（pinned nsh 0.4.0 bba76ef） | exit=0 |
| `nsh-check-0.6.0.txt` | 同命令（本机 nsh 0.6.0 f577c51，升级后） | **exit 0 / 0 errors**（speech 的 tooLarge 空值已处理；surface gate 已移到 app 根） |
| `tui-new-tests.txt` | `cd web && nsh test --target tui --filter <三个新用例>` | 3/3 通过（capability 修复后重跑） |
| `browser-new-tests.txt` | `cd web && nsh test --target web App.tui.test.tsx --filter <三个新用例>` | 3/3 通过（headless Chrome；capability 修复后） |
| `tui-full.txt` | `cd web && nsh test --target tui`（final base + nsh 0.6.0 升级后） | **395/396 通过**；唯一失败=既有 settings raw-JSON（见下「环境性失败」第 2 条） |
| `openspec-strict.txt` | `openspec validate keel-harness-v2 --strict` | `Change 'keel-harness-v2' is valid` |

## 能力门（复核 P1 修复，2026-11-23）

W12 的可用性判断原本是传输层探测（`typeof identityApi().sessionSearch === "function"`）。
这会让 fixture/旧传输凭「实现了方法」把 UI 打开，而部署能力并未声明；已改为能力门：

- `web/src/data/session-query.ts`：`searchAvailable()` / `exportAvailable()` →
  `can("session.search")` / `can("session.export")`；
- `web/src/data/deliverables.ts`：`available()` → `can("session.deliverables")`；
- `web/App.tsx`：客户端半边注册三个 feature（所有宿主都能渲染）；
- `web/src/data/fixture.ts`：开发传输的 `/v1/capabilities` 同步广告三个 feature
  （否则 fixture 下 UI 会按能力门关闭）；
- 回归测试 `web/src/data/session-capabilities.test.ts`（3 条）：只客户端半边 → 关、
  只部署半边 → 关、两边都开 → 开；
- 服务器半边 `internal/harnessapi/api.go` 已按 `SessionSearch`/`SessionExport`/
  `SessionFiles` seam 上报这三个 feature。

浏览器首跑失败（`id:session-message-hits showing never happened`）就是这个缺口的实证：
fixture 能应答但没广告；补齐后 TUI 与浏览器两套宿主都 3/3 通过。

## 复核修复（独立复核 8/10，2026-11-23）

独立复核（PASS 8/10）留下的 1 个 P1 与 5 个 P2 已逐条处理：

- **P1 残留库**：`keel_w12_test` / `keel_w12_gate` 已 `DROP ... WITH (FORCE)`；最终检查无
  `keel_%` 库（本轮证据用的 `keel_w12_fix` 也在收尾时删除）。
- **注释失真**：`SessionSearcher`/`SessionExporter`/Config 注释原写「Nil answers 503」，
  实际 seam nil 走 501（无 Access 才是 503）；已改为「Nil answers 501（缺 Access 是单独的
  503 路径）」。
- **format=400 分支无测试**：`TestSessionExportRefusalsAndBounds` 拆成
  `SessionExists=true`（format/xml、limit 0/501、afterSeq -1 逐个断言 400）与
  `SessionExists=false`（404）两段，再加 unwired 501。
- **能力位与 handler 不对称**：`session.search/export/deliverables` 能力位现在同时要求
  seam 与 `Access != nil`（与 `sessions.list` 同口径）；生产接线仍同源。
- **搜索框文案**：部署没有 search 能力时 placeholder 只承诺「搜索会话」，不再写「或消息」。
- **`tui-full.txt` 时效**：旧文件是 rebase 前 `9480bf8` 的记录；已用 final base 全量重跑替换
  （见下表与「环境性失败」）。

## 负控（测试里构造的失败面）

- **跨租户检索**：`TestPGSessionQuerySearchScopeAndPagination` 在另一租户写入相同文本，检索只返回本租户会话；归档会话也被排除。
- **可见性**：`TestSessionSearchVisibilityFiltersHits` 让 fake `Visible` 只放行一个会话，结果只留一个；键级拒绝返回 403。
- **未接线 fail closed**：`TestSessionSearchRouteBoundsAndRefusals` 证明无 `Access` seam 时 503、无检索 seam 时 501。
- **导出不越界**：`TestPGSessionQueryExportIsBoundedAndTenantScoped` 证明另一租户导出为空、`tool.result` 不出现、`limit=1` 分页有 `nextAfterSeq`。
- **没有整会话 dump**：`TestBoundedExportPageHonoursByteAndMessageBounds` 证明单条 32KiB、单页 512KiB 上限和 `truncated/hasMore`；`TestSessionExportRefusalsAndBounds` 证明 `limit` 上限 500、非法 `format`/`afterSeq` 400。
- **交付物只有一个真值**：`TestDeliverablesRouteReadsTheManifest` 断言 hash/size/mode/updatedSeq/lastTaskId 与 manifest 行完全一致，`previewUrl` 由服务端给出；`TestDeliverablesPaginationAndRefusals` 断言 path keyset、404、501。
- **标题只跟首条消息**：`internal/sessiontitle` 的 `TestDeriveIsStable` 与既有 `TestPGSessionListerTitlesFromFirstUserMessage`（本次仍运行）证明后续消息不移动标题、附件/选择不进入标题。

## 环境性失败（与切片无关，已记录）

1. `TestMCPAppRejectionAuditSampling` 是**负载敏感的既有用例**：旧基线在并发负载下出现过
   「4 个采样窗口而非 1 个」（`go-mcp-audit-rerun.txt` 是那次记录；1000 次 HTTP 拒绝请求在
   慢机上超过 1 秒，真实定时器多切了窗口）。在 final base 的串行全量里
   `go test ./internal/harnessapi` 通过（`go-full-packages.txt`，7.788s），全仓
   `go test -p 1 ./...` 也通过；本切片没有改 `mcp_app_actions*`。
2. `nsh test --target tui` 的 396 个用例中 1 个失败：
   `TUI: 设置页——模型分区两层结构…`。它是**既有**失败（0.4.0 时代在旧基线 position 177
   失败；本机 0.6.0 在 position 52 失败），失败点是原始 JSON 编辑器逐段输入后文档变成非法
   JSON（保存按钮因此 disabled）。该用例路径不经过任何 W12 文件；本切片没有改
   `settings.tsx`/模型 fixture 的既有形状。新加的 3 个 TUI 用例与所有其他设置/模型用例通过。
   已在合入前单独定位：用 `app.paste` 替换逐段 `type` 可以让「坏文档 → 400」这一段通过，
   但第二次保存仍不落版本；属于既有测试/输入路径问题，另行修。
3. **24 个 realtime/speech 失败已随 nsh 0.6.0 升级修掉**：0.6.0 要求 app 导入的资源
   在 app 目录内，而这两个 gate 在 `src/surfaces/*` 下经 fixture 拉 `web/assets/demo-shot.png`。
   已把两个测试文件与 harness 移到 `web/`（产品 app 根，`assets/` 在根内），并删除只为旧
   app 根存在的 surface `App.tsx`/`nsh.json`；`nsh check` 也从 2 个 speech 空值错误降到 **0**
   （speech 上传路径补了 `PickedFile.file == null`（tooLarge）的提示）。
   final base 的全量 TUI：**396 tests, 395 passed, 1 failed**（只剩下面第 2 条的 settings
   raw-JSON 既有失败）。

## 已知限制

- 检索是参数化 `strpos(lower(text), lower(q))` 子串匹配；新增部分索引
  `events_transcript_created` 让排序/扫描落在 user/assistant 文本行上，但没有 trigram/全文
  索引，成本随租户内 transcript 行数增长。输出（行数/片段源 4096 字符）有界，扫描本身没有
  独立 per-request 超时。
- 客户端导出动作复制**一页**（默认 200 条）并在提示里说明「还有更多」；没有多页下载/合并
  的 UI。API 已提供 `afterSeq` keyset，可作为后续入口。
- `keel.attachment` 未实现；`session-title` 不做重命名/LLM 标题，规则固定为首条用户消息。
- 浏览器 target 已在 final rebase 上重跑（`browser-new-tests.txt`，headless Chrome 3/3）；
  TUI 仍是一等 target，但两套宿主都走了同一份 App.tsx 与同一组断言。
- 本机 nsh 是 0.6.0 nightly（pinned 0.4.0 bba76ef 不可得）；`nsh check` 的两条错误是 0.6.0
  的 `PickedFile.file` 空值语义（既有 speech surface），不是本切片引入；pinned 0.4.0 的
  `nsh-check.txt` 仍是 exit=0。
- deliverables 的分页在 handler 里是「manifest seam 全量列表 + 内存按 path 排序截断」，
  对外游标仍是 path keyset（客户端语义正确），但服务端成本是 O(session manifest)。复用既有
  `SessionFiles` seam（不另造文件真值）是有意取舍；manifest 行数失控时再把
  `ORDER BY path` + `WHERE path > $after` 下推到 `PostgresManifest.List`。
