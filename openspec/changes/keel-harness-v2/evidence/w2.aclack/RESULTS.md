# w2.aclack — A 轴安全复核非阻塞项清理（2026-11-20）

基线：`597ad7e`（分支 `task/keel-harness-v2`）；本切片分支 `task/keel-w2-aclean`。
范围：`wave2-a-review.md` 处置表中标记「本轮修复」的 #1/#3(token)/#4/#7 + #8 文档注记。
无 P0/P1；未扩大范围。

## 逐项改动

### 1) spawn 子代理 selection 走提交门（A 轴 #1）

- `internal/agentext/subagent.go`：`SubagentOptions.ValidateSelection` 新增；`SpawnAsync` 在
  创建 child session / 写 user.message **之前**用与 `/input` 相同的 `ValidateSelection` seam
  校验 `SpawnRequest.childSelection()` 的结果。校验失败返回 `ErrInvalidSpawn`（既有类型化错误），
  不创建子会话、不写事件、不登记 sub-agent record。nil 校验器为 fail-closed（有 selection 即拒绝）。
- `cmd/keel/serve.go`：runner 绑定 `selectFromCurrentGeneration(rt)` —— 与 `/input`、`/events`
  完全同一个 per-generation 闭包，不复制校验逻辑。
- HTTP spawn（`internal/agentapi/ext.go` 原样透传的 `selection`）与模型 `agent.spawn`（agentId
  派生 selection）共用 runner 入口，因此两条 spawn 路径同门。
- 测试：
  - `internal/agentext/subagent_binding_test.go`：`TestSubagentSpawnCarriesTheMemberBinding` 断言
    validator 收到且仅收到派生 selection；新增
    `TestSubagentSpawnRefusesAnInvalidSelectionBeforeCommit`：非法 selection → `ErrInvalidSpawn`、
    零 child session、零事件、零 registry record；同请求换通过校验后正常创建。
  - `internal/agentapi/api_test.go` `TestSubagentRoutes`：非法 selection → 400 且 committer 提交数
    不变；合法 selection → 202。

### 2) notify ack 清理 raw token（A 轴 #3 token 部分）

- `internal/notify/notify.go`：
  - `Ack` 成功后（含幂等重复 ack）调用 `forgetTokens(notificationID)`，该通知**所有** consumer 的
    raw capability token 立即从进程内移除，不再等 `finishPush`。
  - `OpenURL` / `TrackDelivery` 在 token 本身通过校验后，对 `StatusConsumed` 的行返回 `ErrConsumed`
    （HTTP 410）。因此任一 consumer 的 ack（API/tool/URL）都会让其它 consumer 的 open/pixel
    capability 立即失效；错误 token 仍先按 `ErrTokenInvalid` 返回 404，不新增 oracle。
- 测试：`internal/notify/notify_test.go` 新增
  `TestAnyAckInvalidatesEveryCapabilityToken`（API ack 后其它 mail/pixel token 均 410，raw token
  清空，重复 ack 幂等且不新增 ack fact）、`TestURLConsumptionInvalidatesOtherTokens`（URL 消费
  同样使 sibling token 失效）。
- `internal/agentapi/approvals_notifications_test.go` 的 `TestNotificationRoutesAckListAndURL`
  原断言「一个 consumer ack 后另一个 URL 仍可打开」正是被修复的旧语义，改为：先 open 成功、重放
  410、sibling URL 410、再 ack 另一 consumer 仍成功。

### 3) MCP image 内容字节上限（A 轴 #4）

- `internal/mcp/output_limit.go`：新增 `MCPImageMaxBytes = 2 MiB`（按进入上下文的编码 payload
  计：`Data` 或 URL 长度；约 1.5 MiB 原始图像，低于多数 provider 单图 5 MB 上限）。超限 image
  块替换为有界文本占位
  `[image omitted: <mime>, <n> bytes exceeds the <limit>-byte limit]`（mime 截断到 64B），
  原图字节不进入上下文；文本 20 KiB 截断语义不变。
- 测试：`internal/mcp/output_limit_test.go` 新增小图原样同 slice、超限图占位且位置/大小有界、
  文本+超限图同时发生时两者都降级。

### 4) `mcp.call` arguments 必须是 JSON object（A 轴 #7）

- `internal/toolsurface/mcp.go`：`mcpCallArguments` 在调用 backend 前强制 schema 契约：
  缺失/null → `{}`；非 object（array/string/number/bool）→ `invalid_arguments`，backend 不被调用。
  object 原样（trim 后）传给 backend。
- 测试：`internal/toolsurface/toolsurface_test.go` 新增
  `TestMCPCallArgumentsMustBeAnObject`；`TestMCPActions` 断言 object 原样到达 backend。

### 5) 文档注记（A 轴 #8）

- `internal/approval/approval.go` 包注释：`X-Keel-Actor` 是租户自证的咨询性归因，不是授权事实；
  audit UI/策略不得当作密码学身份。

## 门禁（最终树）

| 门禁 | 结果 | 证据 |
|---|---|---|
| `go build ./...` | exit 0 | `01-build.txt` |
| `go vet` 改动包 | exit 0 | `02-vet.txt` |
| `go test -race -count=1` 改动包（含 PG 测试） | exit 0，7 包全 ok | `03-race.txt` |
| `KEEL_TEST_DSN=... go test -count=1 ./...` | exit 0，54 包 ok，零 FAIL | `04-full-test.txt` |
| `openspec validate keel-harness-v2 --strict` | exit 0（openspec 1.14.1） | `05-openspec-validate.txt` |
| `./scripts/agent-runtime-e2e.sh --evidence ...` | exit 0（PASS） | `06-agent-runtime-e2e.txt` + `agent-runtime-e2e/` |
| 端口/进程残留 | 无 keel 进程；13610/13611 已释放；e2e DB 已删 | `07-residue.txt`、`agent-runtime-e2e/port-release.txt` |

测试库：本 worktree `.env.local` 的 `KEEL_TEST_DSN_W2_ACLEAN`（派生库
`keel_test_w2_aclean`，不入库）；DSN/密码/Bearer 未出现在任何日志或证据中。

## Backlog（本轮明确不做）

- **#2 模型可 ack 自己的通知**：`tool-surface` spec 明确把 `ack` 列入 `notify` 动作，属注意力层
  产品语义；本轮不改，待产品语义复核。
- **#5 `Materialize` 的 `O_NOFOLLOW`/`EvalSymlinks` 守卫**：当前生产未启用 CaptureRoot；
  fork/materialize 生产接线前必须补。
- **#6 `shellScope` 无 run scope 回落 boot 租户**：当前不可达；多租户 pack 前改为类型化拒绝。
- **通知表租户配额**：需要配额模型（窗口/租户维度），不在本切片。

## 偏离/说明

- 本仓库 worktree 无 `package.json`，`pnpm exec openspec` 不可用；使用 `openspec` 1.14.1
  （与父仓 `package.json` 固定版本一致）直接校验，exit 0。
- `TestNotificationRoutesAckListAndURL` 的断言按其新语义重排，属于修复的一部分，非测试放水。
