# P6E 修复注记：A13 / A1 修复，A7 已接受偏离

- 日期：2026-11-20
- lane：P6E（`keel-mcp-p6-fix`），基线 `b8ca79b`（P5 最终快照）
- 范围：`feat-mcp-apps-events` spec 的 A13（能力协商）、A1（`_meta.ui.domain` 随目录）、A7（teardown 有界等待）

## 1. A13：initialize 的双位置 MCP Apps 能力声明（已修复）

- `internal/mcp/protocol.go`：`ClientCapabilities` 新增 `Extensions map[string]json.RawMessage`（2026-07-28 合法字段）；自定义 `MarshalJSON` 同时输出 `extensions` 与 `experimental`，空非 nil map 保留为 `{}`（与 sampling/elicitation 的声明语义一致）。
- 新增 `(*ClientCapabilities).DeclareMCPApps()`：向 `extensions["io.modelcontextprotocol/ui"]` 与 `experimental["io.modelcontextprotocol/ui"]` 写入**同一 payload** `{"mimeTypes":["text/html;profile=mcp-app"]}`；扩展标识常量 `mcp.AppExtensionID` 在 `internal/mcp/ui_meta.go`。
- 只在部署真正接线 apps 时声明：`internal/servicehost/services.go` 的 `mcpClientCapabilities(cfg)` 在 `cfg.MCP.Registry != nil` 时调用 `DeclareMCPApps()`；未接线（如 `cmd/runtime` 无 registry）不声明。`cmd/keel/serve.go` 的 registry 装配不变。
- 测试：
  - `internal/mcp/apps_capability_test.go` `TestInitializeDeclaresMCPAppsInBothPositions`：MemoryTransport 抓 wire 级 initialize 请求，断言两位置都存在、payload 深相等（值 + 字节）、`mimeTypes` 正确。
  - 同文件 `TestInitializeOmitsMCPAppsWithoutDeclaration`：未声明时两位置都不存在。
  - 同文件 `TestClientCapabilitiesPreservesEmptyExtensionsMap`：空非 nil `extensions` 输出 `{}`，零值输出 `{}` 且不含字段。
  - `internal/servicehost/mcp_capabilities_test.go` `TestMCPClientCapabilitiesDeclareAppsOnlyWhenRegistryWired`：nil registry 不声明；非 nil registry 双位置声明且字节相等；MRTR 的 sampling/elicitation 不丢失。

## 2. A1：`_meta.ui.domain` 随目录下发（已修复）

- `internal/mcphost/surface.go`：`AppSpec` 新增 `Domain string \`json:"domain,omitempty"\``。
- `internal/mcphost/registry.go` `resolve`：把 `parseResourceMeta` 已校验/小写化的 `meta.Domain` 投影到 `AppSpec.Domain`。
- `internal/agentapi` 的 `GET /v1/mcp/apps` 直接序列化 `AppSpec`，因此目录 JSON 自动含 `domain`；`internal/agentapi/mcp_apps_test.go` 的 `testSurface` 与目录断言覆盖。
- 前端：`web/src/data/contract.ts` `McpAppView.domain?: string`；`web/src/data/mcp-apps.ts` `parseMcpApp` 经 `domainOf` 做**有界透传**（trim，>253 字符丢弃；字符集/长度由后端负责，前端不重算 URL）。`web/src/data/mcp-apps.test.ts`「目录：domain 有则透传，缺省/超长不发明」覆盖有/缺/超长三态。
- 后端测试：`internal/mcphost/registry_test.go` `TestRegistryPublishesValidatedApps` 的资源 meta 增加 `"domain":"Weather.Example"`，断言 `spec.Domain == "weather.example"` 且序列化后的目录 JSON 含该键。

## 3. A7：卸载 teardown 的 1s 等待（已接受偏离，非假装修复）

### 当前行为

- `web/src/surfaces/mcp-app/view.tsx:396-401` `disposeSession`：
  1. `session.bridge.teardown()` — 若已初始化，`bridge.ts:664-679` 发 `ui/resource-teardown`（id `host-<n>`）并设置 1s 定时器，响应到达（`bridge.ts:640-648`）或超时后调用 `onTeardown`；
  2. 紧接着 `session.bridge.destroy({kind:"unmounted"})` 立即销毁实例。
- 结果：teardown 帧已发出，但调用方不等待响应；桥内的 1s 有界等待只在桥单测中可观察，卸载路径上被绕过。

### 为什么在 NSH 组件卸载模型下不能真实等待（架构原因）

- `cleanup(f)` 是**通知**，不是可延迟的卸载钩子：`nsh` 文档（`docs/dev/components.md:281/323`）明确 cleanup 在 owner 消失时立即运行，没有“保留组件直到异步完成”的形态；应用代码也没有 DOM/重挂载 API。
- Web 目标的 View 是 `<iframe>`（`nsh-platform-web/web/nsh.js:1750-1761`）；组件移除时 `apply()` 在**同一同步批**内调用 `finishRemovals()`（`nsh.js:1717`），后者对未参与退场动画的节点直接 `el.remove()`（`nsh.js:1724-1740`）。
- iframe 从 DOM 移除即关闭其 browsing context；`postMessage` 走 `el._frameEl?.contentWindow?.postMessage(...)`（`nsh.js:4512`），此时 `contentWindow` 已不可用。teardown 响应是异步的 postMessage 回帧，必然晚于同一批次的 DOM 移除，因此**无法在同一实例上观察到响应**。
- 桌面/移动目标的 WebView 同样随宿主节点移除而销毁（`nsh-core` 的原生 WebView 元素由平台视图承载，`nsh-platform-macos/src/native.rs:1729+`），没有跨平台的“先等回帧再移除”接缝。
- 仅延迟 `destroy()` 不会让 iframe 多活：帧已被移除，1s 后触发的只会是超时分支；把 bridge 留在 map 里 1s 属于假等待，不采用。
- 保留 iframe 的唯一形态是把 View 提升到应用根部的常驻隐藏容器（不受行卸载影响），这会把工具结果渲染器改成全局 portal、重建布局/主题/滚动语义，代价与风险远超一条生命周期礼貌通知；且它仍是“另一个挂载点里的同一文档”，不是规范的“卸载前等待”，故不做。

### 影响与风险（仅生命周期通知）

- 丢失的是 View 在 `ui/resource-teardown` 中释放自身资源/回执的机会；宿主侧实例、桥、iframe 都按原路径销毁。
- 无数据、安全、账本、审计影响：卸载路径不写账本；teardown 帧不含凭据或消息正文；宿主不依赖其响应做任何状态转换。
- 规范条款「MUST 等待响应（有界 1s）」在本宿主上不可满足；按 P6 任务书登记为**已接受偏离**，供 P6.4 复核裁决。若要真正满足，需要 NSH 上游提供可延迟的元素移除/卸载钩子（例如 cleanup 返回 Promise 或保留槽位），届时再实现。

## 4. P6A1-login-cap：登录后刷新部署能力（追加 P1，已修复）

- 现象（P6A1 真实浏览器 E2E `login-capability-stale`）：未登录首屏 `detect()` 因 401 不读 `/v1/capabilities`；`login()` 成功只 `refreshSessions()`，能力位仍是登录前的空快照，已有会话里带 app 的 `tool.result` 继续显示「当前客户端以文本显示」，必须整页刷新才挂载 View。
- 修复：`web/src/data/store.ts` `login()` 成功分支在 `auth.set("ok")` 与 `refreshSessions()` 之前调用 `web/src/data/capabilities.ts` 新增的 `refreshDeploymentCapabilities(http)`：
  - 成功：`setDeploymentCapabilities` 记录真实文档，能力 signal 在 auth gate 重渲染前已更新；
  - 失败/超时（默认 3s 有界）：保留既有快照（不 `setDeploymentCapabilities(null)`，不假装），`console.warn` 记录原因并 `flash` 一条 warn（交互界面暂以文本显示）。
- `detect()/reconnect()/refreshSessions()`：`detect()` 在 `probe()==="ok"` 分支本来就读 capabilities；`reconnect()` 与列表重试走 `detect()`，因此只有登录分支缺口，未重复加逻辑。
- 测试：`web/src/data/capabilities.test.ts`「登录后刷新：成功即记录部署能力；失败/超时保留旧快照并报告」覆盖登录前 off → 登录后 on、失败保留、超时保留三态。

## 5. 门禁（本 lane）

见同目录 `gates.txt` 与提交说明；本文件不复制原始输出。
