# Tasks: feat-mcp-apps-events

> 口径：任务板只是索引，真值＝代码 + 证据（`docs/MATURITY.md` 的 G0–G5）。
> 每项给出**验收口径**（能跑的命令或能看的证据）；未勾选项不得声称完成。
> 阶段依赖：P1 → P2 → P3 →（P4 可与 P3 并行）→ P5 → P6；P1 不依赖任何前端改动。

## 0. 契约与前置（本 change 交付的文档）

- [x] T0.1 冻结两份 spec（`specs/mcp-apps`、`specs/mcp-events`），实现不得偏离；
      偏离走新 change 或本 change 的 MODIFIED delta（验收：`openspec validate feat-mcp-apps-events --strict` 通过）
- [x] T0.2 设计中的标准出处逐条复核（`design.md` 附录 A 的 11 项），标注抓取日与版本；
      任何方法名/字段名以 SDK `schema.json` 与 2026-01-26 spec 正文为准
      （验收：附录 A 每行可点开、结论与 `design.md` §0 一致）
      （复核 2026-11-20：11 项均可达；schema 的 17 个 `ui/*` 方法名逐一核对；SDK exports 行已按实测修正）
- [x] T0.3 `docs/PI-PARITY.md` 登记偏离：MCP Apps 与 events 是 PI 没有的面；
      legacy 的 MCP 行为逐条不变（验收：偏离表含「理由/影响面/验收方式」三列）
- [x] T0.4 双 origin 沙箱域开通（2026-11-14）：`mcp-app.fde2e.com` + `*.mcp-app.fde2e.com` ——
      DNS 泛解析已覆盖（二级通配实测）、acme.sh DNS-01 证书（两域一体，LE ec-256）、
      `conf.d/mcp-app.fde2e.com.conf`（80→443、Host 透传 → frps 13601）、frpc 片段
      `mcp-app-fde2e.toml`（pin，→ dev keel 13760）；生产由 keel 正式位接管同域
      （验收：`evidence/domain-provision.md`——服务器侧 200/301/SAN + 公网直连 + 浏览器渲染）

## P1 — 元数据、资源与 visibility（无 UI）

> **P1.2 裁决（2026-11-20，与 v2 工具面契约对齐）**：本任务原文的「`mcp__<server>__<tool>` 命名不变」
> 与已冻结的 `keel-harness-v2` 工具面冲突（v2 模型面只有聚合 `mcp` 工具，硬编码名已删除并登记为
> PI 偏离，`docs/PI-PARITY.md` §4.2）。裁决：P1.2 的「app 表」是 **MCP manager 内部的工具目录/绑定表**
> （`internal/mcp` 的 `Server.ModelTools`/`AppTools`/`Apps`，供 app 目录使用），不是模型面工具名；
> `mcp__<server>__<tool>` MUST NOT 被重新引入。模型面过滤的验收改为「聚合 `mcp` 的 list/describe/call
> 不暴露 `visibility` 不含 `model` 的工具，且内建工具清单仍为 11 个」。

- [x] P1.1 `Tool._meta.ui` 解析（`resourceUri` + 废案平铺键 `_meta["ui/resourceUri"]`；
      `visibility` 缺省 `["model","app"]`）
      （验收：单测覆盖新键/废案键/缺省/非法形状；非法形状记诊断不崩）
- [x] P1.2 模型面过滤 `visibility` 不含 `"model"` 的工具（聚合 `mcp` 的 list/describe/call）；
      app 工具单独进 manager 内部 app 目录（绑定表，不生成模型面工具名）
      （验收：单测断言模型面不变（11 内建 + 聚合 `mcp` 输出）、app 表含 app-only 工具）
- [x] P1.3 客户端方法 `ListResources` / `ListResourceTemplates` / `ReadResource` / `ListPrompts` / `GetPrompt`
      （验收：fake server 单测覆盖分页、`ttlMs`/`cacheScope` 解析、非 `text/html;profile=mcp-app` 拒绝）
- [x] P1.4 `MCPToolDetails` 增 `App{AppID,ResourceURI,Visibility}` 与 `StructuredContent`（截断有界）
      （验收：`tool.result` 的 details 里两字段随账本落库并可读回；模型上下文不含它们）
- [x] P1.5 `mcp_apps` 提示段（哪些工具带交互界面）（验收：装配测试断言系统提示含该段且 `mcp_servers` 仍在）
- [x] P1.6 回归：`go test ./internal/mcp/... ./internal/host/...` + `scripts/conformance.sh` 全绿
      （验收：命令输出；PI 移植测试未改断言）

## P2 — apps 宿主骨架与内联渲染

- [x] P2.1 新包 `internal/mcphost`：`Surface`/`Sinks`/`AppSpec`/`ResourceSpec` + `Registry`
      （appId 规则、目录、generation）（验收：单测；同资源多工具引用去重）
      （2026-11-20 P2 后端切片：`registry_test.go`——派生 appId、双工具引用去重、generation、租户 scope）
- [x] P2.2 文档取回/缓存/CSP 构造/主题映射：`ReadResource` → 校验 → hash → LRU（64 MiB）
      （验收：单测覆盖上限、LRU 淘汰、缺 `csp` 的严格缺省、只收紧不放宽）
      （2026-11-20：`cache_test.go` 字节上限/淘汰/保护；`csp_test.go` 逐指令 + 负向；
      `meta_test.go`/`theme_test.go`。CSP 额外加 `sandbox allow-scripts`（只收紧：直开文档也不落入宿主 origin））
- [x] P2.3 发布通路改造：`cmd/keel/dynamic_surface.go`（聚合 MCP 半面 + 插件半面，合并后
      `RefreshProductTools`/`RefreshProductPrompt`）；MCP 面经 sinks 首次发布
      （替换 `newPromptFactory` 的一次性读取）
      （验收：装配测试「插件刷新后 MCP 工具/段仍在」与反向各一条，治 R2）
      （2026-11-20：`cmd/keel/mcp_surface_test.go` 双向两条；Boot 首次发布 + Reconnect 重发）
- [x] P2.4 HTTP 面：`GET /v1/mcp/servers`、`GET /v1/mcp/apps`、
      `GET /v1/mcp/apps/{appId}/view`（CSP/`nosniff`/`Referrer-Policy`/immutable 缓存/`frame-ancestors`）
      （验收：`curl -I` 断言头；越权 appId 404；租户边界先于 SSE/正文）
      （2026-11-20：`agentapi/mcp_apps_test.go` httptest 级断言；跨租户/未知 appId 404 不泄露清单；
      hash URL immutable、裸 URL no-store）
- [x] P2.5 `/v1/capabilities` 能力位：`mcp.apps`、`mcp.apps.sandboxOrigin`、`mcp.events`
      （验收：capabilities 测试；未接线时为 false 且 501/404 路径明确）
      （2026-11-20：未接线 501；sandboxOrigin 来自 `KEEL_MCP_APP_ORIGIN`（无硬编码生产域）；mcp.events 默认 false）
- [x] P2.6 前端 `web/src/surfaces/mcp-app/{index,view,bridge}.*`：iframe
      （`sandbox="allow-scripts"`，无 `allow-same-origin`）+ 握手（initialize→initialized 时序）+
      `tool-input`（恰好一次）→ `tool-result` + 内联弹性尺寸 + 全屏（kit Dialog/Sheet）
      （验收：`nsh check`；`bridge.test.ts` 过；E2E 断言 sandbox 属性与 CSP 头）
      （2026-11-20 复核修复批次：**真实 Chrome E2E `pass:true` 连续 3 次**
      （`evidence/p2/frontend/e2e/report.json`）——绝对同源 URL（`keel.info` 取 origin）+
      URL 形态 WebView；View→Host 走页面 `window.top` 监听 + `event.source`↔`instance` nonce，
      Host→View 走 `WebViewRef.postMessage`；断言 initialize→tool-input→tool-result→size-changed
      （360px）、sandbox 无 `allow-same-origin`、CSP 四个头、cookie/DOM/localStorage 隔离。
      **卫生项**：web 的 `onNavigationStateChange` 空转已显式标注（web 只靠 load 防线；原生两条
      都真实）；每实例 10 次导航重载预算，超出销毁+文本兜底+诊断并尽力 POST
      `/v1/mcp/apps/audit`（`protocol.violation`，detail 无 query；P3.7 前 404 被吞）；文档 CSP
      的 `sandbox allow-scripts` 与 iframe 属性取交集，E2E 断言 `window.open` 被阻断。
      `bridge.test.ts` 18/18、`reload-budget.test.ts` 4/4、TUI 145/145。**偏离**：NSH 0.4.0 无
      sandbox prop，web 运行时属性固定加
      `allow-forms/popups/popups-to-escape-sandbox/modals/downloads`（同源 src 仍无
      `allow-same-origin` → opaque origin）；CSP `sandbox allow-scripts` 已把有效权限收紧，
      字面值偏离登记在 `evidence/p2/frontend/gates.md` §偏离 1。srcdoc wrapper 方案被文档
      `frame-ancestors 'self'` 阻断（实测），故采用 URL 形态 + 页面监听。）
- [x] P2.7 `messages.tsx` 的 `ToolRow` 挂载（`details.app` + 能力位）；保留文本兜底可展开；
      按 `toolCallId` 复用实例（防流式重渲染重建 iframe）
      （验收：组件测试或 E2E；流式期间 iframe 不重建）
      （2026-11-20：`McpAppSlot` 挂到 ToolRow 展开区；绑定读完整 `tool.result`（core 适配层按
      `toolCallId` 索引 details/content）；TUI `can("mcp.apps")` 为假时不挂 WebView，显示
      app 标题 + 文档路径 + 文本结果。实例按 `toolCallId` 持有桥状态、`src` 由目录 appId+hash+
      instance+reload 决定；E2E 证明自导航后 reload 并重新握手，流式重渲染不改变文档身份。
      浏览器「流式期间不重建」的专项慢帧 E2E 仍归 P6.2。）
- [ ] P2.8 降级面：无 apps 服务器/无能力客户端/取回失败/握手超时（5s）/协议违规 → 文本兜底 + 诊断 + 审计
      （验收：单测逐格 + E2E 一格）
      （2026-11-20 服务端部分已交付：无 apps 服务器=空目录、取回/校验/超限/缓存失败=诊断+审计+排除目录、
      重取失败保留旧版本、app 绑定按已发布目录门控（无半成品 app）；前端部分已交付：无能力/终端=
      文本+标题+链接、目录失败/握手超时/协议违规/加载失败=文本+原因+重试（`bridge.test.ts` 逐格、
      TUI 用例一格、浏览器 E2E 含导航降级一格；重载预算耗尽会尽力写
      `protocol.violation` 审计）。剩余：审计表 `mcp_app_audit` 的 durable 落库（P3.7）与
      P6.3 的独立降级 E2E，故本项仍未勾）

## P3 — 桥与出站动作

- [x] P3.1 `POST /v1/mcp/apps/{id}/tools/call`：ownership 复核（只允许该服务器、只允许 `visibility` 含 `"app"`）
      + 审计（验收：越权 403 + 审计行；正常路径返回标准 `CallToolResult`）
      （2026-11-20 P3 后端切片：`internal/agentapi/mcp_app_actions.go`——server 由 catalogue 派生、`AppTool` 复核
      `visibility`；越权/别服务器/模型专用工具 403 + `tools/call=refused` 审计且上游零调用；破坏性工具
      （`destructiveHint` 或变更族词名）逐次 decision 确认，拒绝无上游。httptest 断言 + 真 stdio fixture
      `cmd/keel/mcp_app_bridge_test.go`。复核修复：同会话未答 `mcp-app.*` decision 上限 16，超限 429
      `-32000` 且不新建行）
- [x] P3.2 `POST /v1/mcp/apps/{id}/resources/read`：URI scheme 白名单 + 该服务器限定
      （验收：`file:`/`data:` 等被拒并审计）
      （2026-11-20：只允许 `ui://`/`http(s)://`（缺 host/缺 scheme 也拒）；结果按 2 MiB 单条上限复核、不落盘；
      拒绝写 `resources/read=refused` 且不带 URI query）
- [x] P3.3 `ui/message`：decision（send/edit/discard）→ 既有输入写路径（`requestId` 可归属、`delivery` 只能 queue）
      （验收：三分支 E2E；`user.message` 落账本；`edit` 不落账本只预填；`role!="user"` 回 `-32602`）
      （2026-11-20：`mcp-app.message` decision 三选项；send 经 `httpapi.CommitClientInput`（与 `/input` 同一提交路径，
      delivery=queue，requestId=`mcp-app:<appId>:<frame>`，重放幂等）；edit 只回预填、discard/非法 role/delivery
      零账本；真库+MemKernel 断言 user.message 落账本与幂等）
- [x] P3.4 `ui/open-link`：http(s) 白名单 + 确认 + `openUrl`/`window.open`（noopener）
      （验收：非法 scheme 拒绝并审计；不开宿主页导航）
      （2026-11-20：后端只返回 `{opened,url}` 供前端以 noopener,noreferrer 打开；默认 decision `mcp-app.link`；
      非法 scheme 400 + `ui/open-link=refused`；审计只记 host，不记 query；不写账本。前端 window.open 属前端 lane）
- [ ] P3.5 `ui/request-display-mode`（inline/fullscreen；`pip` 拒绝回当前模式）、
      `ui/notifications/size-changed`、`ui/resource-teardown`（卸载前发，≤1s）
      （验收：桥单测 + E2E 全屏切换）
      （2026-11-20：桥/视图实现已在 P2 前端落地（`bridge.ts` 三方法 + `view.tsx`
      全屏 Dialog/尺寸/teardown 测试）；未勾仅因浏览器全屏切换 E2E 归 P6.2）
- [ ] P3.6 权限确认（camera/mic/geolocation/clipboardWrite）+ 偏好记忆；破坏性工具逐次确认
      （验收：拒绝后不静默重给；确认门 E2E）
      （2026-11-20 P3 后端切片：**部分**——破坏性工具逐次确认门与拒绝审计已在 P3.1 交付并测；
      camera/mic/geolocation/clipboardWrite 的 iframe `allow` 授予 UI、偏好记忆与前端拒绝路径属前端 lane，故本项不勾。
      2026-11-20 复核：NSH 0.4 WebView 无 per-permission `allow` 控制、单 origin 沙箱无
      `allow-same-origin`，权限无法真实生效；本机不假装声明，登记为上游/第二 origin 依赖，
      证据 `evidence/p5/review-disposition.md` §5）
- [x] P3.7 审计表 `mcp_app_audit` + `GET /v1/mcp/apps/audit`；正文/凭据扫描断言
      （验收：单测断言审计不含 message 正文与 token）
      （2026-11-20：`schema.sql` + `rlsTenantTables`（FORCE RLS + tenant_isolation）；`store/mcp_app_audit.go`
      append/list（detail 有界 512B、去控制符）；`GET/POST /v1/mcp/apps/audit`（读分页；POST 仅
      permission.granted/denied、protocol.violation 的宿主动作入口，租户鉴权、不能伪造桥动作）；
      真库 RLS/跨租户写拒绝 + 全行扫描断言无消息正文/URL query。复核修复：role/delivery/content/attachments
      违规写 `protocol.violation=refused`（只记字段/固定原因码）；拒绝路径审计按 app/动作/秒采样）
- [x] P3.8 桥限速限长：单帧 64 KiB、200 帧/s、单 app 并发 4；超限断连 + 审计
      （验收：单测用假计时源断言阈值）
      （2026-11-20：HTTP 代理面同阈值——64 KiB body（413）、每 app 200/s 令牌桶（假时钟测试）、
      单 app 并发 4（429）；三类超限都写 `bridge.size|rate|concurrency=refused`（复核修复：每
      app/动作/秒采样 1 行 + `suppressed=N`，防止零成本拒绝以线速灌审计表）；浏览器桥帧实现同数
      并在 `mcp_app_actions.go` 头注释写明映射）

## P4 — events 路由与订阅

- [x] P4.1 `Dispatcher` 接口与 `ClientOptions` 回调（通知/服务端请求），默认实现保持
      「method not found」（fail closed 不变）（验收：未注入时行为与今天逐字一致）
- [x] P4.2 通知分类落点实现（design §4.1 表逐行）：progress/message 瞬态；目录类 → Registry；
      cancelled → 中止调用；（验收：单测逐行；账本零新增 kind 断言）
- [x] P4.3 `subscriptions/listen`：filter、ack 子集、`subscriptionId` 去复用、断线重发（modern）、
      legacy GET 流保持；多订阅并存（验收：HTTP fake server 断言首消息必为 ack、未 opt-in 的通知不投递）
      （2026-11-20 复核修复：请求域未打标帧按来源流路由——listen 流上未打标仍按违规，
      tools/call 响应流/连接流上的 progress/cancelled/message 不被吞；日志采样、短命流退避、
      reconnect 后 listen 重评估已补。注：prompts 目录当前无内容存储，`prompts/list_changed`
      只触发注册面重算与 generation，prompt 内容缓存/刷新留 P5/P6）
- [x] P4.4 `tools/list_changed` → 去抖（250ms）→ 全量 republish → generation++ →
      hook 事件 `mcp.surface.changed`（闭集追加）；失败保留旧面
      （验收：run 边界测（design §4.4(7)）；失败面测试断言旧工具仍可用）
- [x] P4.5 `progress` → `(session, toolCallId)` 关联 → PartialHub（新 kind `mcp.progress`）；
      runner 注入 `ProgressToken`（验收：单测断言不串会话；丢帧无害）
- [x] P4.6 `notifications/message` → 结构化日志（level 白名单、字段截断）；
      **不**进时间线与账本（验收：单测 + 日志字段断言）
- [x] P4.7 指标：`mcp_servers{state}`、`mcp_surface_generation`、`mcp_app_fetch_total{result}`、
      `mcp_app_bridge_frames_total{method,direction}`、`mcp_listen_reconnects_total`、`mcp_mrtr_rounds`
      （验收：基数上界写进 design/代码注释；series 数与会话数无关的断言）

## P5 — MRTR / 版本对齐 / app 上下文

- [x] P5.1 `InputRequiredResult`/`InputRequests`/`InputResponses`/`requestState` 类型与解析；
      `resultType` 三态（缺省=complete、complete、input_required；未知=协议错误）
      （验收：单测三态 + 未知值走失败路径）
      （2026-11-20：`internal/mcp/input_required.go` + `client.go`；三态与
      `inputRequests`/`requestState` 形状校验；未知 resultType 回 `-32600`；
      证据 `evidence/p5/mcp-tests.txt`）
- [x] P5.2 MRTR 执行器：elicitation→decision；sampling→当前会话模型服务（`KEEL_MCP_SAMPLING` 默认 session、
      `maxTokens` 上限、每调用预算 3）；roots→拒绝；未知 method→拒绝并审计；轮次预算 5
      （验收：端到端 MRTR 测；`requestState` 原样回显与重试 id 不同为硬断言）
      （2026-11-20：`internal/mcphost/mrtr.go` + `internal/mcp/client.go`；轮次 5、
      sampling 3 次/调用、`maxTokens` 4096 上限、roots/未知方法 `-32601` 审计；
      requestState 逐字节回显、重试新 id、缺 key 拒绝发送；决策走
      `agentext.DecisionBroker`；sampling 走 runtime 的 llm/ModelService（非新 provider）；
      证据 `evidence/p5/notes.txt` §P5.2）
- [x] P5.3 legacy 主动请求等价路径 + 能力声明（只在真能答时声明 sampling/elicitation）
      （验收：fake server 发 `sampling/createMessage`（legacy）→ 得到结果；未声明能力时服务器不得发）
      （2026-11-20：`Client.answerServerRequest` 在唯一在途调用内把 legacy request 交给
      MRTR 执行器（调用 ctx 的 tenant/session）；无调用/多调用回落 dispatcher 拒绝；
      Manager 在无执行器时剔除 sampling/elicitation，roots 永不声明；
      证据 `evidence/p5/mcp-tests.txt` / `mrtr-executor-tests.txt`）
- [x] P5.4 era 探测与门控（`KEEL_MCP_PROTOCOL_ERA=auto|legacy|modern`）：modern 下不发
      session/`Last-Event-ID`、补齐 `_meta` 与 `Mcp-Method`/`Mcp-Name` 头、`ttlMs`/`cacheScope`；
      legacy 行为不变（验收：两个 era 的 fake server 各一组；conformance 全绿）
      （2026-11-20：`internal/mcp/era.go`；auto=先 modern `server/discover`，400 现代错误
      按 supported 重试，其余回落 initialize；modern 无 session/GET/Last-Event-ID/DELETE，
      `_meta` 与头同版本、`Mcp-Name` base64 哨兵；非法 env fail closed 到 legacy；
      conformance 1057/0 fail；同时按 P4 复核结论放行 listen 活跃时的未打标
      progress/cancelled/message（P4 lane 拥有 canonical fix，本分支带最小修复与回归测试）；
      证据 `evidence/p5/mcp-tests.txt` / `conformance.txt`）
- [x] P5.5 `ui/update-model-context`：`mcp_app_contexts` 表 + runner seam `<mcp_app_context>` 段；
      未接线时 `-32601` + 能力位 false（验收：迁移测试 + 下次 attempt 提示含上下文；显式否决「写账本」方案）
      （2026-11-20：`schema.sql` + `rlsTenantTables` + `store/mcp_app_context.go`（覆盖式、
      RLS、64 KiB 两文档上限）；`POST /v1/mcp/apps/{appId}/context`（未接线 501/-32601，
      `mcp.apps.context` false）；`Runner.SessionContexts` 在 attempt 内存内并入最后一条
      user 消息，零账本新增；app 桥调用按 sessionId 注入 ToolRunScope；写账本方案
      在代码注释/设计 §7-R5 明确否决；证据 `evidence/p5/store-tests.txt` / `notes.txt`）
- [x] P5.6 `/v1/mcp/servers` 报 `era`/`protocolVersion`/`capabilities`（验收：单测）
      （2026-11-20：ServerState 取 client 实际 era；capabilities 8 KiB 有界，
      超限 `{"truncated":true}` + `capabilitiesTruncated`；证据 `evidence/p5/agentapi-tests.txt`）
- P5 复核修复（2026-11-20）：A 0P0/1P1、B 0P0/2P1 已清零——`/context` 桥限流、
  sampling `maxTokens` 真正下发、elicitation 关闭回 `-32601`；另收口 inputRequests/
  elicitation schema 上界、legacy 请求有界异步、cancel 语义、attempt 模型 selection、
  严格版本日期；前端 View→Host 五条代理链路接线（tools/resources/message/open-link/
  context）。明细 `evidence/p5/review-disposition.md`，门禁 `evidence/p5/merged-verification.txt` §5。

## P6 — 验收与收口

- [ ] P6.1 G0–G5 证据逐条落盘（design §5.2 表），含崩溃矩阵（`cmd/e2e` 新 scenario `mcp-apps`）
      与规模/混沌（`scripts/acceptance-perf.sh` 扩展）
- [ ] P6.2 浏览器 E2E：`scripts/mcp-apps-e2e.sh` → 每步截图 + 步骤解说 → `report.html`
      （before/after 或 after+断言），落 `.agents/reports/keel/mcp-apps-events/` 并挂公网链接
- [ ] P6.3 降级面 E2E：TUI 不渲染 app（URL+文本兜底）、CSP 拒绝域名、握手超时各一格
- [ ] P6.4 子代理交叉复核（reviewer + vision 组合，本项目不使用 Codex），逐条闭合阻塞项
- [x] P6.5 文档：`docs/HOST.md`（MCP 段：apps/events/era、HTTP/鉴权、安全、运维）、
      `docs/PI-PARITY.md`（偏离与未实现登记）、`site/docs/zh/harness/mcp-apps.md`（使用页；
      站点双语门禁要求同路径英文页 `site/docs/harness/mcp-apps.md`）、`docs/RESIDUAL-PLAN.md`
      Wave 3.5 状态更新（P5 已交付 / P6 进行中，不勾销未完成项）
      （2026-11-20 P6C docs lane：G0/G4 逐条对表
      `evidence/p6/docs/parity.md`；站点检查 VitePress build + 双语 parity + 链接门禁通过，
      citation gate 在基线页面有 24 条既有 stale anchor（非本 lane 页面引入，见 parity.md §4））
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
