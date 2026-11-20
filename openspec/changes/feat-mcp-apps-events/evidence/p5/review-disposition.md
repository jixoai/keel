# P5 复核处置（A/B 独立复核 → 修复 → 复审对象）

日期：2026-11-20。冻结快照：`7fe4eea`（P5 合入）；修复后快照：见文末 commit。

## 1. 独立复核结论（快照 7fe4eea）

| 轴 | P0 | P1 | P2 | 评分 | 结论 |
|---|---|---|---|---|---|
| A 安全 | 0 | 1 | 7 | 7.5 | fail（P1 未清） |
| B 功能 | 0 | 2 | 5 | 7 | fail（P1 未清） |

## 2. P1 修复

| 编号 | 问题 | 修复 | 回归测试 |
|---|---|---|---|
| A-P1-1 | `POST /v1/mcp/apps/{appId}/context` 未走 `mcpAppGate`，绕过 200/s + 并发 4 + 413 审计 | `internal/agentapi/mcp_app_context.go` 在 app 解析后、decode 前调用同一 gate | `TestMCPAppContextRouteUsesTheBridgeGate`（占满 4 槽后第 5 个 429、store 零行、`bridge.concurrency=refused` 审计） |
| B-P1-1 | sampling `maxTokens` 只在 executor/审计存在，最后一层 provider 调用未下发（MUST 预算失效） | `internal/servicehost/mrtr_sampling.go` 以 `StreamConfig.Extra["max_tokens"]` 下发；仅 >0 | `TestRuntimeSamplerForwardsMaxTokensToTheProvider`（4096/128 到达 StreamFn，0 不下发） |
| B-P1-2 | elicitation 关闭/未装 decision 时回 `-32603`，冻结 spec 要求 `-32601` | `internal/mcphost/mrtr.go` 改 `CodeMethodNotFound`；no-scope 仍 `-32603` | `TestMRTRElicitationDisabledIsMethodNotFound`（两分支） |

## 3. P2 修复（本轮收口）

| 编号 | 问题 | 修复 | 回归测试 |
|---|---|---|---|
| A-P2-1 | `inputRequests` 键数/键长/params 无界，可锁死调用 | 16 keys / 128B key / 64KiB params；elicitation 32 properties / 64 enum options / 4KiB literal | `TestInputRequiredRequestsAreBounded`、`TestMRTRElicitationSchemaBounds` |
| A-P2-2 | legacy server request 在 receiveLoop 同步执行，可冻结整条连接 | 4 槽有界异步；溢出立即 `-32603`；快照 active call 保留 scope；per-call mutex 串行化共享 state | `TestLegacyServerRequestDoesNotBlockTheReceiveLoop` |
| B-P2-4 | elicitation「取消/拒绝 + 残留文本」被映射成 accept | 显式 decline/cancel/projected option 优先于 Text | `TestElicitationAnswerTerminalOptionBeatsText` |
| A-P2-3/B-P2-1 | sampling 不读会话/attempt 的模型 selection | `agentcore.WithToolModelSelection` + runner stamp + sampler `Resolve(scope, selection)`；无 key 用部署默认（app 直调） | `TestRunnerStampsTheAttemptSelectionOnToolContext`、`TestRuntimeSamplerResolvesTheAttemptModelSelection` |
| A-P2-5 | 任意 ≥ modern 字符串被接受为协议版本 | 严格 `YYYY-MM-DD` + 日历校验，再比较 modern 下限 | `TestSupportedProtocolVersionIsStrict` |
| A-P2-7 | 任务板 conformance 计数 1056/证据 SKIP | 修正 1057；`runner-session-context.txt` 带 DSN 重跑 PASS | 证据文件 |

## 4. 前端 P3/P5 断链（B-P2-3 的扩展）

复核发现后端五条代理路由已齐，但 `web/src/surfaces/mcp-app/bridge.ts` 对
`tools/call`、`resources/read`、`ui/message`、`ui/open-link`、`ui/update-model-context`
一律回 `-32601`。已接线：

- bridge 异步回调（不阻塞后续帧；失败保留 host 的 JSON-RPC code）。
- 能力声明：`serverTools/serverResources/message/openLinks/updateModelContext`
  只在 `can()` 为真时声明；`initialize` 时冻结，未声明请求一律 `-32601`。
- store/HTTP：同源 cookie，`sessionId` 取当前会话；202（确认未完成）不是成功。
- `ui/message` edit 分支复用 `prefillComposer`；`ui/open-link` 仅打开
  http(s)，web 用 `noopener,noreferrer`，原生用 `openUrl`。
- 测试：bridge 30 / mcp-apps 14 / capabilities 5；TUI 全量 164。

## 5. 明确不改 / 残余

- `<mcp_app_context>` 仍在最后一条 user 消息前：design §3.6.3 明确要求，改序属设计变更，本轮不做。
- `KEEL_MCP_SAMPLING=on` 无人值守采样无 per-server 窗口预算：opt-in 成本风险，注释已诚实化；默认 `session` 不可达。
- NSH WebView 无 per-permission `allow` 控制、单 origin 沙箱无 `allow-same-origin`：P3.6 的 camera/mic/geolocation/clipboardWrite 授予无法真实生效，登记为上游依赖，不假装声明。
- P3.5 代码（request-display-mode / size-changed / resource-teardown）已在 bridge 与 view；浏览器全屏切换 E2E 留 P6.2。
- P3.6 的破坏性工具逐次确认后端门已在 P3.1；权限授予 UI/偏好记忆待 NSH allow 面。

## 6. 修复后门禁

见 `merged-verification.txt` §5/§6（Go 55 ok、conformance 1073/0/1、TUI 164/164、
E2E PASS）。修复后 A/B 复审均 0 P0/P1（各 9/10）；A 残余的溢出内联 Send 与
能力注释漂移已在最终快照 `7004b11` 收口（后台队列 32 + 单 worker），B 指出的
typed McpError→wire 测试缝已补。

本文件不含 DSN 或密钥。
