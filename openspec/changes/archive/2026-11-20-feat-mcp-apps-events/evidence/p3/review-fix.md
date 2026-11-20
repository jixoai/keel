# P3 独立复核修复（P1-1/P1-2 + P2-1..P2-6）

日期：2026-11-20 · 复核基线：`472eaac`（无 P0，2×P1 + 6×P2）· 修复在 `task/keel-mcp-p3` 上增量提交（不 push/rebase）。

## P1

### P1-1 拒绝路径审计放大

- 新增 `recordSampledRejection`：按 `(appId, action, 秒窗口)` 采样，每窗口最多 1 行；窗口内后续拒绝只累加 `suppressed`，窗口关闭（真实 1s 定时器或测试显式 flush）时写出一行，detail 追加 `suppressed=N`。
- 适用动作：`bridge.rate`、`bridge.concurrency`、`bridge.size`（413）、`confirmation_overflow`。
- 首次拒绝仍被审计（行在窗口关闭时落库）；主响应从不等待审计。
- 测试 `TestMCPAppRejectionAuditSampling`：同一假时钟秒内 200 允许 + 1000 拒绝 → 恰好 1 行 `bridge.rate=refused` 且 `suppressed=999`；跨秒后第二个窗口 1 行，上游调用数不增加。

### P1-2 open decision 堆积

- `confirmMCPApp` 在 `Open` 前调用 `mcpAppOpenConfirmations`：统计该 `(tenant, session)` 下 `kind` 前缀 `mcp-app.` 且 `status=open` 的 decision；达到 `mcpAppMaxOpenConfirmations=16` 时直接 429 + JSON-RPC `-32000`，不创建新行；overflow 审计走 P1-1 采样。
- 已答/超时的 decision 不再计入（broker `List` 先过期归一化）。
- 测试 `TestMCPAppConfirmationOverflow`：预开 16 个未答 decision → 第 17 个 429 且行数仍 16、commit 零调用；答掉 1 个后同一请求成功开新 decision 并可答完。

## P2

### P2-1 attachments 严格校验

- `mcpAppValidateAttachments`：必须是数组；≤16 条；每条只允许 `{fileId,mime,name,size}`（未知字段包括 `url` 一律拒绝）；`fileId` 必须是 UUID；`name` 1..256B；`mime` ≤128B；`size` 0..1 GiB。
- 非法回 `-32602`、零账本、不开 decision；审计 `protocol.violation=refused` + `field=attachments reason=<code>`（不含值）。
- 确认卡 prompt 增加「含 N 个附件」，不展示附件内容；审计 detail 记 `attachments=N`。
- 测试 `TestMCPAppMessageAttachments`：2 个合法附件进入确认与 commit；7 类非法形状全部 -32602、零 decision、零 commit。

### P2-2 协议违规审计

- `role!=user`、`delivery!=queue`、content 非对象/含未知字段/非 text/空文本：返回 `-32602` 前写 `protocol.violation=refused`，detail 只有 `field=role|delivery|content reason=<固定码>`，不含内容。
- 测试 `TestMCPAppMessageProtocolAudits`（4 条分支）+ `TestMCPAppMessageAttachments`（attachments 分支）。

### P2-3 破坏性词表

- 后端词表 = 前端 `FAMILY_WORDS.changes` 冻结词 + 后端从严扩展：`drop/truncate/rm/reset/clear/flush/wipe/nuke/erase/purge/destroy/revoke/unlink/format/overwrite`；注释写明「前端展示表冻结，后端从严」。
- 测试：`TestMCPAppNameDestructive` 增加 6 个新词；`TestMCPAppDestructiveConfirmation` 中 `db_drop`（无注解）必须过确认门后才调用。

### P2-4 content 单对象容忍

- `content` 改为 `json.RawMessage` 后在 handler 解析：数组或单个 `{type:"text"}` 对象都接受；`type`/`text` 之外的成员、字符串/数字等形状一律 `-32602` + protocol 审计。
- 测试 `TestMCPAppMessageSingleContentObject`：单对象成功落账本；含未知成员被拒。

### P2-5 open-link userinfo / 非 ASCII host

- URL 校验新增 `parsed.User != nil` → `reason=userinfo`；`Hostname()` 含 >0x7F rune → `reason=non_ascii_host`；均在确认前拒绝 + 审计，无 decision。
- 测试 `TestMCPAppOpenLinkRejectsUserinfoAndNonASCII`。

### P2-6 open-link session 溯源

- 可选 session 在 scheme/host 校验前用 `mcpAppSessionValue` 解析（仍不强制）；有效会话写 `session=<uuid>`，非法/不存在写 `session_problem=invalid|not_found`，审计行仍为 `ui/open-link=refused`。
- 方案通过后若 session 非法/不存在，返回 400/404；缺失仍由 `confirmMCPApp` 回显式 400。
- 测试 `TestMCPAppOpenLinkSchemeAuditCarriesSession`：`javascript:` 拒绝行带 `session=`；`not-a-uuid` 带 `session_problem=invalid`。

## 测试与门禁

新增/更新测试集中在 `internal/agentapi/mcp_app_actions_test.go`；证据文件：

- `go-build-review-fix.txt`、`go-vet-review-fix.txt`、`go-test-race-review-fix.txt`
- `go-test-all-review-fix.txt`
- `conformance-review-fix.txt`、`openspec-validate-review-fix.txt`
- `agent-runtime-e2e-review-fix.txt`、`residue-review-fix.txt`

## 未做

- RESIDUAL rollup（主代理负责，已到 23/50）。
- 浏览器 E2E（前端 lane）。
