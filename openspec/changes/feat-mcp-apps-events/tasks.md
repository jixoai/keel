# Tasks: feat-mcp-apps-events

> 口径：任务板只是索引，真值＝代码 + 证据（`docs/MATURITY.md` 的 G0–G5）。
> 每项给出**验收口径**（能跑的命令或能看的证据）；未勾选项不得声称完成。
> 阶段依赖：P1 → P2 → P3 →（P4 可与 P3 并行）→ P5 → P6；P1 不依赖任何前端改动。

## 0. 契约与前置（本 change 交付的文档）

- [ ] T0.1 冻结两份 spec（`specs/mcp-apps`、`specs/mcp-events`），实现不得偏离；
      偏离走新 change 或本 change 的 MODIFIED delta（验收：`openspec validate feat-mcp-apps-events --strict` 通过）
- [ ] T0.2 设计中的标准出处逐条复核（`design.md` 附录 A 的 11 项），标注抓取日与版本；
      任何方法名/字段名以 SDK `schema.json` 与 2026-01-26 spec 正文为准
      （验收：附录 A 每行可点开、结论与 `design.md` §0 一致）
- [ ] T0.3 `docs/PI-PARITY.md` 登记偏离：MCP Apps 与 events 是 PI 没有的面；
      legacy 的 MCP 行为逐条不变（验收：偏离表含「理由/影响面/验收方式」三列）
- [x] T0.4 双 origin 沙箱域开通（2026-11-14）：`mcp-app.fde2e.com` + `*.mcp-app.fde2e.com` ——
      DNS 泛解析已覆盖（二级通配实测）、acme.sh DNS-01 证书（两域一体，LE ec-256）、
      `conf.d/mcp-app.fde2e.com.conf`（80→443、Host 透传 → frps 13601）、frpc 片段
      `mcp-app-fde2e.toml`（pin，→ dev keel 13760）；生产由 keel 正式位接管同域
      （验收：`evidence/domain-provision.md`——服务器侧 200/301/SAN + 公网直连 + 浏览器渲染）

## P1 — 元数据、资源与 visibility（无 UI）

- [ ] P1.1 `Tool._meta.ui` 解析（`resourceUri` + 废案平铺键 `_meta["ui/resourceUri"]`；
      `visibility` 缺省 `["model","app"]`）
      （验收：单测覆盖新键/废案键/缺省/非法形状；非法形状记诊断不崩）
- [ ] P1.2 `AgentTools` 过滤 `visibility` 不含 `"model"` 的工具；app-only 工具单独成表（app 目录用）
      （验收：单测断言模型面工具数减少、app 表含该工具；`mcp__<server>__<tool>` 命名不变）
- [ ] P1.3 客户端方法 `ListResources` / `ListResourceTemplates` / `ReadResource` / `ListPrompts` / `GetPrompt`
      （验收：fake server 单测覆盖分页、`ttlMs`/`cacheScope` 解析、非 `text/html;profile=mcp-app` 拒绝）
- [ ] P1.4 `MCPToolDetails` 增 `App{AppID,ResourceURI,Visibility}` 与 `StructuredContent`（截断有界）
      （验收：`tool.result` 的 details 里两字段随账本落库并可读回；模型上下文不含它们）
- [ ] P1.5 `mcp_apps` 提示段（哪些工具带交互界面）（验收：装配测试断言系统提示含该段且 `mcp_servers` 仍在）
- [ ] P1.6 回归：`go test ./internal/mcp/... ./internal/host/...` + `scripts/conformance.sh` 全绿
      （验收：命令输出；PI 移植测试未改断言）

## P2 — apps 宿主骨架与内联渲染

- [ ] P2.1 新包 `internal/mcphost`：`Surface`/`Sinks`/`AppSpec`/`ResourceSpec` + `Registry`
      （appId 规则、目录、generation）（验收：单测；同资源多工具引用去重）
- [ ] P2.2 文档取回/缓存/CSP 构造/主题映射：`ReadResource` → 校验 → hash → LRU（64 MiB）
      （验收：单测覆盖上限、LRU 淘汰、缺 `csp` 的严格缺省、只收紧不放宽）
- [ ] P2.3 发布通路改造：`cmd/keel/dynamic_surface.go`（聚合 MCP 半面 + 插件半面，合并后
      `RefreshProductTools`/`RefreshProductPrompt`）；MCP 面经 sinks 首次发布
      （替换 `newPromptFactory` 的一次性读取）
      （验收：装配测试「插件刷新后 MCP 工具/段仍在」与反向各一条，治 R2）
- [ ] P2.4 HTTP 面：`GET /v1/mcp/servers`、`GET /v1/mcp/apps`、
      `GET /v1/mcp/apps/{appId}/view`（CSP/`nosniff`/`Referrer-Policy`/immutable 缓存/`frame-ancestors`）
      （验收：`curl -I` 断言头；越权 appId 404；租户边界先于 SSE/正文）
- [ ] P2.5 `/v1/capabilities` 能力位：`mcp.apps`、`mcp.apps.sandboxOrigin`、`mcp.events`
      （验收：capabilities 测试；未接线时为 false 且 501/404 路径明确）
- [ ] P2.6 前端 `web/src/surfaces/mcp-app/{index,view,bridge}.*`：iframe
      （`sandbox="allow-scripts"`，无 `allow-same-origin`）+ 握手（initialize→initialized 时序）+
      `tool-input`（恰好一次）→ `tool-result` + 内联弹性尺寸 + 全屏（kit Dialog/Sheet）
      （验收：`nsh check`；`bridge.test.ts` 过；E2E 断言 sandbox 属性与 CSP 头）
- [ ] P2.7 `messages.tsx` 的 `ToolRow` 挂载（`details.app` + 能力位）；保留文本兜底可展开；
      按 `toolCallId` 复用实例（防流式重渲染重建 iframe）
      （验收：组件测试或 E2E；流式期间 iframe 不重建）
- [ ] P2.8 降级面：无 apps 服务器/无能力客户端/取回失败/握手超时（5s）/协议违规 → 文本兜底 + 诊断 + 审计
      （验收：单测逐格 + E2E 一格）

## P3 — 桥与出站动作

- [ ] P3.1 `POST /v1/mcp/apps/{id}/tools/call`：ownership 复核（只允许该服务器、只允许 `visibility` 含 `"app"`）
      + 审计（验收：越权 403 + 审计行；正常路径返回标准 `CallToolResult`）
- [ ] P3.2 `POST /v1/mcp/apps/{id}/resources/read`：URI scheme 白名单 + 该服务器限定
      （验收：`file:`/`data:` 等被拒并审计）
- [ ] P3.3 `ui/message`：decision（send/edit/discard）→ 既有输入写路径（`requestId` 可归属、`delivery` 只能 queue）
      （验收：三分支 E2E；`user.message` 落账本；`edit` 不落账本只预填；`role!="user"` 回 `-32602`）
- [ ] P3.4 `ui/open-link`：http(s) 白名单 + 确认 + `openUrl`/`window.open`（noopener）
      （验收：非法 scheme 拒绝并审计；不开宿主页导航）
- [ ] P3.5 `ui/request-display-mode`（inline/fullscreen；`pip` 拒绝回当前模式）、
      `ui/notifications/size-changed`、`ui/resource-teardown`（卸载前发，≤1s）
      （验收：桥单测 + E2E 全屏切换）
- [ ] P3.6 权限确认（camera/mic/geolocation/clipboardWrite）+ 偏好记忆；破坏性工具逐次确认
      （验收：拒绝后不静默重给；确认门 E2E）
- [ ] P3.7 审计表 `mcp_app_audit` + `GET /v1/mcp/apps/audit`；正文/凭据扫描断言
      （验收：单测断言审计不含 message 正文与 token）
- [ ] P3.8 桥限速限长：单帧 64 KiB、200 帧/s、单 app 并发 4；超限断连 + 审计
      （验收：单测用假计时源断言阈值）

## P4 — events 路由与订阅

- [ ] P4.1 `Dispatcher` 接口与 `ClientOptions` 回调（通知/服务端请求），默认实现保持
      「method not found」（fail closed 不变）（验收：未注入时行为与今天逐字一致）
- [ ] P4.2 通知分类落点实现（design §4.1 表逐行）：progress/message 瞬态；目录类 → Registry；
      cancelled → 中止调用；（验收：单测逐行；账本零新增 kind 断言）
- [ ] P4.3 `subscriptions/listen`：filter、ack 子集、`subscriptionId` 去复用、断线重发（modern）、
      legacy GET 流保持；多订阅并存（验收：HTTP fake server 断言首消息必为 ack、未 opt-in 的通知不投递）
- [ ] P4.4 `tools/list_changed` → 去抖（250ms）→ 全量 republish → generation++ →
      hook 事件 `mcp.surface.changed`（闭集追加）；失败保留旧面
      （验收：run 边界测（design §4.4(7)）；失败面测试断言旧工具仍可用）
- [ ] P4.5 `progress` → `(session, toolCallId)` 关联 → PartialHub（新 kind `mcp.progress`）；
      runner 注入 `ProgressToken`（验收：单测断言不串会话；丢帧无害）
- [ ] P4.6 `notifications/message` → 结构化日志（level 白名单、字段截断）；
      **不**进时间线与账本（验收：单测 + 日志字段断言）
- [ ] P4.7 指标：`mcp_servers{state}`、`mcp_surface_generation`、`mcp_app_fetch_total{result}`、
      `mcp_app_bridge_frames_total{method,direction}`、`mcp_listen_reconnects_total`、`mcp_mrtr_rounds`
      （验收：基数上界写进 design/代码注释；series 数与会话数无关的断言）

## P5 — MRTR / 版本对齐 / app 上下文

- [ ] P5.1 `InputRequiredResult`/`InputRequests`/`InputResponses`/`requestState` 类型与解析；
      `resultType` 三态（缺省=complete、complete、input_required；未知=协议错误）
      （验收：单测三态 + 未知值走失败路径）
- [ ] P5.2 MRTR 执行器：elicitation→decision；sampling→当前会话模型服务（`KEEL_MCP_SAMPLING` 默认 session、
      `maxTokens` 上限、每调用预算 3）；roots→拒绝；未知 method→拒绝并审计；轮次预算 5
      （验收：端到端 MRTR 测；`requestState` 原样回显与重试 id 不同为硬断言）
- [ ] P5.3 legacy 主动请求等价路径 + 能力声明（只在真能答时声明 sampling/elicitation）
      （验收：fake server 发 `sampling/createMessage`（legacy）→ 得到结果；未声明能力时服务器不得发）
- [ ] P5.4 era 探测与门控（`KEEL_MCP_PROTOCOL_ERA=auto|legacy|modern`）：modern 下不发
      session/`Last-Event-ID`、补齐 `_meta` 与 `Mcp-Method`/`Mcp-Name` 头、`ttlMs`/`cacheScope`；
      legacy 行为不变（验收：两个 era 的 fake server 各一组；conformance 全绿）
- [ ] P5.5 `ui/update-model-context`：`mcp_app_contexts` 表 + runner seam `<mcp_app_context>` 段；
      未接线时 `-32601` + 能力位 false（验收：迁移测试 + 下次 attempt 提示含上下文；显式否决「写账本」方案）
- [ ] P5.6 `/v1/mcp/servers` 报 `era`/`protocolVersion`/`capabilities`（验收：单测）

## P6 — 验收与收口

- [ ] P6.1 G0–G5 证据逐条落盘（design §5.2 表），含崩溃矩阵（`cmd/e2e` 新 scenario `mcp-apps`）
      与规模/混沌（`scripts/acceptance-perf.sh` 扩展）
- [ ] P6.2 浏览器 E2E：`scripts/mcp-apps-e2e.sh` → 每步截图 + 步骤解说 → `report.html`
      （before/after 或 after+断言），落 `.agents/reports/keel/mcp-apps-events/` 并挂公网链接
- [ ] P6.3 降级面 E2E：TUI 不渲染 app（URL+文本兜底）、CSP 拒绝域名、握手超时各一格
- [ ] P6.4 子代理交叉复核（reviewer + vision 组合，本项目不使用 Codex），逐条闭合阻塞项
- [ ] P6.5 文档：`docs/HOST.md`（MCP 段）、`docs/PI-PARITY.md`、`site/docs/zh/harness/mcp-apps.md`、
      `docs/RESIDUAL-PLAN.md` Wave 3.5 勾销
- [ ] P6.6 交付即归档：`openspec/changes/archive/` + 提交推送（含主仓指针，若在子仓工作）

## 已知暂缓项（登记，不装作已完成）

- [ ] (a) `ui/notifications/tool-input-partial`：需要 provider 层工具参数增量投影，本 change 不做；
      View 按 spec MAY 忽略；收到也不发（设计已声明）
- [ ] (b) `ui/download-file`、`ui/notifications/request-teardown`（SDK 2.x 新面）：不实现，
      回 `-32601`，`/v1/mcp/servers` 的能力说明区注明
- [ ] (c) `KEEL_MCP_APP_ORIGIN` 第二 origin 沙箱（spec 的 web host 双 iframe 形态）：
      本 change 以 opaque origin 等价约束交付；增强留 P6 之后独立项
- [ ] (d) prompts 资源面的 UI（管理页展示/调试）：目录与读接口在 P1/P4 交付，UI 展示不在本 change
- [ ] (e) `pip` 显示模式：不实现（宿主不声明）
