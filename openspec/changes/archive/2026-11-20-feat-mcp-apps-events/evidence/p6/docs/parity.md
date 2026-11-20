# P6 文档线：G0/G4 对表（feat-mcp-apps-events）

- 日期：2026-11-20
- 冻结基线：`3ab7996`（`docs(mcp-p5): 复核修复处置 + 修复后合并树门禁`）
- 范围：`openspec/changes/feat-mcp-apps-events/specs/mcp-apps/spec.md`（13 条 requirement）
  与 `specs/mcp-events/spec.md`（15 条 requirement），共 **28 条**。
- 结论：**28/28 条已映射**；其中已实现 **25** 条、偏离 **3** 条（A5 第二 origin/NSH flags、
  A7 teardown 等待已接受、A12 permissions 上游依赖）。A1/A13 已由 P6E 收口。
  G0 通过（规格/接口/线格式可对表；偏离显式登记）。G4 通过（PI conformance
  1071 pass / 0 fail / 1 skip；legacy 断言未改），但 G4 的「真实 ext-apps example
  server」证据有边界，见 §3。
- 证据来源（本文件不复制正文，只引用）：
  - `openspec/changes/feat-mcp-apps-events/evidence/p5/review-disposition.md`（P5 A/B 复核处置）
  - `openspec/changes/feat-mcp-apps-events/evidence/p5/merged-verification.txt`（修复后合并树门禁）
  - `openspec/changes/feat-mcp-apps-events/evidence/p2/frontend/gates.md`（P2 前端真实浏览器 E2E 与偏离）
  - `openspec/changes/feat-mcp-apps-events/evidence/p2/frontend/e2e/report.json`（`pass:true`，连续 3 次）
  - `openspec/changes/feat-mcp-apps-events/evidence/p4/architecture-isolation.txt`
  - `openspec/changes/feat-mcp-apps-events/evidence/domain-provision.md`（双 origin 域名开通；注意代码未接双 iframe）

## 1. mcp-apps：13 条逐条对表

| # | Spec requirement | 结论 | 实现（文件/符号） | 测试 / 证据 |
|---|---|---|---|---|
| A1 | UI 资源的声明与读取 | **已实现**：声明/读取/类型/大小/`_meta.ui.domain` 均解析并随目录下发（P6E 收口） | `internal/mcp/ui_meta.go` `parseToolUI`；`internal/mcp/resources.go` `ReadAppResource`/`ValidateAppResource`；`internal/mcphost/meta.go` `parseResourceMeta`；`internal/mcphost/registry.go` `resolve`/`AppSpec.Domain`；`web/src/data/mcp-apps.ts` `domainOf` | `internal/mcp/ui_meta_test.go`、`resources_test.go`；`TestRegistryPublishesValidatedApps`（catalogue JSON 含 domain）、`TestMCPAppsCatalogIsTenantScoped`；`web/src/data/mcp-apps.test.ts` domain 三态 |
| A2 | app 身份、缓存身份与目录 | 已实现 | `internal/mcp/ui_meta.go` `AppIDFor`；`internal/mcphost/registry.go` `contentHash`/`documentURL`/`dedupeApps`；`internal/mcphost/surface.go` `AppSpec` | `TestAppIDForIsDeterministicAndOpaque`；`TestRegistryPublishesValidatedApps`、`TestRegistryContentChangeReclaimsCache`；`internal/agentapi/mcp_apps_test.go` |
| A3 | app 文档托管路由与安全头 | 已实现 | `internal/agentapi/mcp_apps.go` `mcpAppView`/`validAppID`/`validContentHash` | `TestMCPAppViewSecurityHeaders`、`TestMCPAppViewUnknownIdentity`、`TestMCPAppViewRefusesIncompleteDocument`；P2 `http-contract-tests.txt` |
| A4 | CSP 构造：只收紧不放宽 | 已实现 | `internal/mcphost/csp.go` `BuildCSP`/`sanitizeCSPDomains`；`internal/mcphost/registry.go` 调用点 | `internal/mcphost/csp_test.go`（严格缺省/逐指令/负向/通配/私网/端口/上限）；P2 浏览器 E2E 的 CSP 头与 `window.open` 阻断 |
| A5 | View 沙箱与跨端承载 | **偏离**：web 有效 opaque origin 已实现，但 NSH 0.4 固定附加 `allow-forms/popups/popups-to-escape-sandbox/modals/downloads`，字面值不是 `allow-scripts`；spec 的第二 origin 双 iframe 未实现，`KEEL_MCP_APP_ORIGIN` 只作原生 WebView 基址 | `web/src/surfaces/mcp-app/view.tsx`（WebView/全屏/TUI 分支）；`web/src/data/mcp-apps.ts` `appDocumentSource`；`web/src/surfaces/mcp-app/bridge.ts` `handleMessage` | `bridge.test.ts`（sandbox origin/opaque/限速）；P2 `gates.md` §偏离 1/2；`e2e/report.json`（sandbox 无 `allow-same-origin`、cookie/DOM/localStorage 隔离） |
| A6 | 握手与 hostContext | 已实现 | `web/src/surfaces/mcp-app/bridge.ts` `handleInitialize`/`handleInitialized`/`armHandshake`；`view.tsx` `hostContextFor` | `bridge.test.ts`「握手/初始化前不发通知/hostContext 全开/全关」；P2 E2E 断言 `protocolVersion`/`hostInfo`/`hostContext` |
| A7 | 数据流与生命周期通知 | **已接受偏离（部分）**：`tool-input`/`tool-result`/`tool-cancelled`/`size-changed`/`host-context-changed` 已实现；`ui/resource-teardown` 在 web 卸载路径发送后立即销毁（NSH 0.4 组件卸载模型不允许延迟移除；无数据/安全影响） | `bridge.ts` `sendInput`/`sendResult`/`cancel`/`teardown`；`view.tsx` `disposeSession`；`frame-load.ts` | `bridge.test.ts`「数据流/取消/teardown 1s」；P6E `evidence/p6/fix/notes.md` §3 |
| A8 | 协议违规、超时与实例销毁 | 已实现 | `bridge.ts` `parseFrame`/`finish`/`armHandshake`/限速；`view.tsx` 回退 UI | `bridge.test.ts`「超时/未知方法/64 KiB/200 帧/s/非沙箱来源」；P2 E2E 导航重载与文本兜底 |
| A9 | tools/call 与 resources/read 由宿主代理 | 已实现 | `internal/agentapi/mcp_app_actions.go` `mcpAppToolCall`/`mcpAppResourceRead`；`MCPAppRuntime`；`cmd/keel/mcp_app_bridge_test.go` 真 stdio | `internal/agentapi/mcp_app_actions_test.go` `TestMCPAppToolCallOwnership`、`TestMCPAppResourceSchemeWhitelist`；`TestMCPAppRuntimeAdapterOverLiveManager` |
| A10 | ui/message 落点与确认 | 已实现 | `internal/agentapi/mcp_app_actions.go` `mcpAppMessage`/`confirmMCPApp`；`MCPAppInputCommit` | `TestMCPAppMessageBranches`、`TestMCPAppMessageAttachments`、`TestMCPAppMessageProtocolAudits`；`TestMCPAppInputCommitWritesUserMessage` |
| A11 | 链接、模型上下文与显示模式 | 已实现（SDK-only 面见备注） | `mcpAppOpenLink`；`internal/agentapi/mcp_app_context.go` `mcpAppUpdateContext`；`bridge.ts` `handleDisplayMode`/`ui/download-file` 分支 | `TestMCPAppOpenLink`、`TestMCPAppOpenLinkRejectsUserinfoAndNonASCII`、`mcp_app_context_test.go`；`bridge.test.ts` 显示模式/错误路径 |
| A12 | 安全、同意与审计 | **偏离（部分）**：破坏性调用逐次确认、审计表与无正文扫描已实现；camera/microphone/geolocation/clipboardWrite **不授予**（NSH 0.4 无 per-permission allow，P3.6 上游依赖） | `mcp_app_actions.go`（确认/审计/限速）；`internal/store/mcp_app_audit.go`；`web/src/surfaces/mcp-app/view.tsx` `hostCapabilitiesFor`（不声明 permissions） | `TestMCPAppDestructiveConfirmation`、`TestMCPAppAuditNoBodyAgainstStore`、`TestMCPAppRejectionAuditSampling`；P5 `review-disposition.md` §5；P2 `gates.md` §偏离 5 |
| A13 | 降级路径与能力协商 | **已实现**：文本/目录/握手/缓存降级；`initialize` 的 `extensions` 与 `experimental` 双位置声明 `io.modelcontextprotocol/ui` + `mimeTypes`（P6E 收口）；另修复 pre-load initialize 首载误判 | `internal/mcp/protocol.go` `DeclareMCPApps`；`internal/servicehost/services.go` `mcpClientCapabilities`；`web/src/surfaces/mcp-app/frame-load.ts` | `TestInitializeDeclaresMCPAppsInBothPositions`、`TestInitializeOmitsMCPAppsWithoutDeclaration`、`TestMCPClientCapabilitiesDeclareAppsOnlyWhenRegistryWired`；`frame-load.test.ts` 5/5 |

> A11 备注：`ui/download-file` 明确回 `-32601`；`ui/notifications/request-teardown` 是通知、
> 无 id，走未知通知 → 协议违规销毁；`ui/notifications/tool-input-partial` 不发；`pip`
> 回当前实际模式。与 spec「明确拒绝而非静默吞掉」一致，但不是 `-32601` 响应（通知无 id）。

## 2. mcp-events：15 条逐条对表

| # | Spec requirement | 结论 | 实现（文件/符号） | 测试 / 证据 |
|---|---|---|---|---|
| E1 | 瞬态通知：进度与日志 | 已实现 | `internal/mcp/client.go` progress 关联/`CallOptions.OnProgress`；`internal/mcphost/events.go` `Notification`/`logServerMessage`；`cmd/keel/serve.go` `MCPProgress` | `internal/mcp/events_test.go` `TestClientProgressCorrelatesPerCall`；`internal/mcphost/events_test.go` `TestEventsTransientNotificationsNeverPublish`、`TestEventsServerLogLevelWhitelistAndBounds` |
| E2 | 目录通知与取消通知的处置 | 已实现 | `mcphost/events.go` `MarkDirty`/`InvalidateResource`/`Notification`；`internal/mcp/client.go` `abortCancelled`（client 侧） | `TestEventsToolsListChangedDebouncesAndRepublishes`、`TestEventsResourcesUpdatedInvalidatesTTLCache`；`mcp/events_test.go` `TestClientCancellationAbortsPendingCall` |
| E3 | 账本零新增 kind | 已实现 | 无 kernel kind 变更；`mcphost/events.go` 文件头与 `internal/mcphost` 不写 ledger | `internal/mcphost/events_test.go` `TestNotificationStormLeavesLedgerKindSetUnchanged`；`internal/agentext/mcp_events_test.go` |
| E4 | 订阅主体与 modern listen 契约 | 已实现 | `internal/mcp/events.go` `Listen`/`listener`/`ackFilterOf`/`subscriptionIDOf`；`mcphost/events.go` `supervise`；`servicehost/services.go` 每服务连接 | `TestClientListenAckFilterAndTagging`、`TestClientListenIgnoresFramesBeforeAck`、`TestClientListenRoutesMultipleSubscriptionsByID`；`TestEventsListenSupervisorResubscribesAndRecomputes` |
| E5 | 断线重建与全量重算 | 已实现 | `mcphost/events.go` `supervise`（1s→30s 退避、短命流升级）、`RefreshNow`、`NoteReconnect`；`internal/mcp/streamable_http.go` legacy GET 重连 | `TestEventsListenRejectedIsAuditedAndRetried`、`TestEventsShortLivedListenStreamsBackOff`、`TestHTTPTransportSignalsLegacyStreamReconnect` |
| E6 | 与产品 SSE 的隔离 | 已实现 | `internal/mcphost` 不 import `internal/httpapi`；`internal/mcp` 不 import 宿主 dispatcher | `evidence/p4/architecture-isolation.txt`（`TestMCPHostDoesNotImportProductSSE`、`TestMCPClientDoesNotImportHostDispatcher` PASS） |
| E7 | 目录变更驱动全量 republish 与 generation | 已实现 | `mcphost/events.go` `refresh`/`publish`；`mcphost/registry.go` `Refresh`/`Generation`；`cmd/keel/dynamic_surface.go` 合面发布 | `TestEventsToolsListChangedDebouncesAndRepublishes`；`cmd/keel/mcp_surface_test.go` 双向组合发布；P5 `mcp-tests.txt` |
| E8 | 热更的 attempt 边界与失败保面 | 已实现 | `internal/mcp/manager.go` `RefreshServer` 只装成功列表；`mcphost/registry.go` `keepPrevious`；`events.go` `catalogueError` | `TestEventsRefreshFailureKeepsOldSurface`；`TestRegistryKeepsPreviousVersionOnRefetchFailure`；P4 `notes.txt` |
| E9 | MRTR：resultType 与 inputRequests 执行 | 已实现 | `internal/mcp/input_required.go`；`internal/mcp/client.go` `CallToolWithOptions`；`internal/mcphost/mrtr.go` `HandleInputRequests`/`elicit`/`sample` | `internal/mcp/p5_test.go` `TestResultTypeThreeStates`、`TestInputRequiredRequestsAreBounded`；`internal/mcphost/mrtr_test.go`（elicitation/sampling/roots） |
| E10 | MRTR 重试、requestState 与预算 | 已实现 | `internal/mcp/client.go` 循环（新 id、`requestState` 原样、`defaultMRTRMaxRounds=5`）；`InputRequestState` | `TestMRTRRoundTripEchoesStateAndUsesNewIDs`、`TestMRTRRoundBudget`、`TestMRTRExecutorMustAnswerEveryKey` |
| E11 | legacy 主动请求与能力声明一致 | 已实现 | `internal/mcp/client.go` `answerServerRequest`/`registerActiveInput`；`internal/mcphost/mrtr.go` `ClientCapabilities`；`manager.go` 无执行器时剔除声明 | `server_requests_test.go` `TestLegacyServerRequestDoesNotBlockTheReceiveLoop`；`TestMRTRClientCapabilitiesMatchTheConfiguration`、`TestLegacyServerRequestUsesExecutingCallScope`；P5 `review-disposition.md` B-P1-1/B-P1-2 |
| E12 | era 支持与 auto 探测 | 已实现 | `internal/mcp/era.go` `EraFromEnvironment`/`ParseProtocolEra`；`internal/mcp/client.go` `connectModern`/`connectLegacy`；`manager.go` 默认 era | `TestClientModernEraSkipsInitialize`、`TestClientAutoProbeFallsBackToLegacy`、`TestClientAutoProbeSelectsModern`、`TestEraParsingAndInvalidEnvironmentFailsClosed`；`conformance.sh` 1071/0/1 |
| E13 | modern 传输契约 | 已实现 | `internal/mcp/streamable_http.go` `Mcp-Method`/`Mcp-Name`/`_meta`/无 session/无 Last-Event-ID；`protocol.go` `ttlMs`/`cacheScope` | `TestHTTPModernHeadersAndNoSession`、`TestHTTPMcpNameEncodesNonASCII`、`TestHTTPLegacyKeepsSessionAndGetBehaviour`；`resources_test.go` cache hints |
| E14 | era 可观测 | 已实现 | `internal/mcphost/surface.go` `ServerState`（Era/ProtocolVersion/Capabilities 8 KiB 有界）；`internal/agentapi/mcp_apps.go` `listMCPServers` | `internal/mcphost/serverstate_test.go`；`internal/agentapi/mcp_servers_test.go`；P5 `agentapi-tests.txt` |
| E15 | 降级、诊断与可观测 | 已实现 | `internal/mcphost/metrics.go`（固定系列）；`events.go` 错误/诊断；`registry.go` 诊断；审计 | `internal/mcphost/metrics_test.go`（基数与会话无关）；P4/P5 metrics evidence；`TestMCPMetricsSeriesDoNotGrowWithSessions` |

## 3. G0 / G4 证据与边界

### G0（规格）

- 两份 spec 冻结了 28 条 requirement 与场景；`openspec validate feat-mcp-apps-events --strict`
  在 P5 门禁中通过（`evidence/p5/openspec-validate.txt`、`evidence/p5/merged-verification.txt`）。
- 公开线格式/HTTP schema 已对表：`docs/HOST.md` §5（端点、鉴权、能力位、错误码、限额）、
  `docs/PI-PARITY.md` §4.2/§4.3（偏离与未实现）、`docs/MCP-APPS.md`（服务端细节）。
- 结论：**G0 通过**；偏离不隐藏，逐条列在 §1 与 `docs/PI-PARITY.md` §4.3。

### G4（PI 对齐）

- `./scripts/conformance.sh`：**1071 pass / 0 fail / 1 skip**（`evidence/p5/merged-verification.txt`
  §5；P5 lane 单分支 1057，合流后 1071）。
- `go build ./...`、`go vet`（mcp/mcphost/agentcore/agentapi/servicehost/store）、
  全量 `go test -p 1 -count=1 ./...`：55 包 ok、零 FAIL（同文件 §5）。
- legacy 行为未改断言：`internal/mcp/client_test.go`（initialize/分页/超时/服务端请求
  method-not-found）、`TestHTTPLegacyKeepsSessionAndGetBehaviour`、
  `TestClientWithoutCallbacksKeepsPreP4Behaviour`、`TestClientCallWithoutTokenKeepsWireShape`。
- apps/events/MRTR 的真值夹具：`internal/mcp` 的 stdio/HTTP fake server 与
  `cmd/keel/mcp_app_bridge_test.go`；P1–P5 单测见 §1/§2。
- **边界（诚实登记）**：本仓 CI 没有 vendored 的官方 `ext-apps` example server（需要 Node
  运行时/网络安装），因此 G4 的「真实 MCP server 零改配置可用」由 fake server 夹具覆盖，
  按 `design.md` §5.2 的 G4 条款记录替代原因；**不声称真实 ext-apps example server 已验收**。
  `evidence/domain-provision.md` 证明 `mcp-app.fde2e.com` 域已开通，但代码未实现双 iframe
  （§1 A5），两者不能互相替代。
- 结论：**G4 通过（PI 一致性）/ 真实 apps server 证据有边界**；P6.2/P6.3 的浏览器/降级 E2E
  仍是 P6 未完成项，不在本文件冒充完成。

## 4. 本文件对表时发现的文档/代码不一致（已在本 lane 修复文档侧）

| # | 位置 | 不一致 | 处置 |
|---|---|---|---|
| 1 | `docs/MCP-APPS.md`（基线 `3ab7996` 第 3 行） | 状态写「P2 后端已交付；P3/P4 不在本阶段」，实际 P3/P4/P5 已交付 | 本 lane 更新状态与端点/审计/环境变量；指向 `docs/HOST.md` §5 |
| 2 | `docs/HOST.md`（基线 `3ab7996` 第 104–106 行） | MCP 环境变量表只有 CONFIG/PROJECT/APP_ORIGIN，缺 `KEEL_MCP_PROTOCOL_ERA`/`KEEL_MCP_SAMPLING`；APP_ORIGIN 被写得像已启用第二 origin | 本 lane 新增 §5.2/§5.7，明确 era/sampling 与实际 origin 行为 |
| 3 | `docs/PI-PARITY.md`（基线 `3ab7996` 第 111 行） | 写「未接线能力（events、第二 origin、MRTR）保持 fail-closed」，实际 events/MRTR 已交付 | 本 lane 重写 §4.3 偏离表与「已知未实现」清单 |
| 4 | `docs/RESIDUAL-PLAN.md`（基线 `3ab7996` 第 24 行） | 行仍写「P6 验收下一波」，未反映 P6 进行中/P6.5 已交付 | 本 lane 更新 37/50 与 Wave 3.5 状态；未完成项不勾 |
| 5 | `internal/mcp/protocol.go` `ClientCapabilities` | 缺 `extensions`，也没有代码写 `io.modelcontextprotocol/ui`/`mimeTypes` | **已由 P6E 修复**（双位置声明 + 条件注入 + 测试） |
| 6 | `internal/mcphost/surface.go` `AppSpec` / `registry.go` `resourceMetaFor` | `_meta.ui.domain` 解析后未投影到目录 | **已由 P6E 修复**（AppSpec.Domain + 前端透传 + 测试） |
| 7 | `design.md` §4.6 | 提到 `KEEL_MCP_MRTR_MAX_ROUNDS`，代码固定 5 轮且无该 env | 文档写明不可配（§3 与 `docs/PI-PARITY.md` §4.3）；不把设计稿当已实现 |

## 5. 复跑命令（不依赖网络/真模型）

```bash
# G0
pnpm exec openspec validate feat-mcp-apps-events --strict

# G1 / G4 的核心单测（按包）
go test ./internal/mcp/... ./internal/mcphost/... ./internal/agentapi/... ./internal/store/...

# G4 PI 一致性（需要脚本夹具；以 evidence 里的 1071/0/1 为准）
./scripts/conformance.sh

# 前端桥/目录/能力门（需要 nsh 工具链）
cd web && nsh test --target tui src/surfaces/mcp-app/bridge.test.ts
cd web && nsh test --target tui src/data/mcp-apps.test.ts
cd web && nsh test --target tui src/data/capabilities.test.ts
```

## 6. 站点检查（2026-11-20，P6.5 文档面）

- `cd site && pnpm install --frozen-lockfile`：ok（vitepress 1.6.4）。
- `cd site && pnpm build`：
  - VitePress build：**ok**（无 dead link；30 个 EN / 30 个 ZH 页面）；
  - bilingual parity：**ok**（30/30，新增 `/harness/mcp-apps` 与 `/zh/harness/mcp-apps` 成对）；
  - link gate：**ok**（2462 条内部链接/片段）；
  - citation gate：**fail（基线既有，非本 lane 引入）**：24 条 stale `path:line` 指向
    `internal/agentapi/ext.go:18/39/128`、`internal/agentext/subagent.go:175`、
    `internal/httpapi/server.go:156`、`internal/agentext/turns.go:373` 等已移动/空行。
    这些页面在 `3ab7996` 基线就存在（`git show HEAD:internal/agentapi/ext.go` 对应行是空行）；
    本 lane 新增的两个 MCP Apps 页面**没有 `path:line` 引用**，因此新增 citation 失败为 0。
- 结论：P6.5 的 MCP Apps 页面已通过 build/parity/link 三段；仓库级 citation gate 仍是基线
  红项，需站点维护线单独修，不计入本 change 的文档完成度。

本文件不含 DSN、密钥或消息正文。
