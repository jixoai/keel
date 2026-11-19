# P2 前端证据（feat-mcp-apps-events，P2.6/P2.7 + P2.8 前端）

日期：2026-11-20 · 基线：`1f2418c` · 分支：`task/keel-mcp-p2fe` · worktree：
`/Users/kzf/Development/GitHub/jixoai/fde2e/.worktrees/keel-task-mcp-p2fe/web`

## 门禁

| # | 命令（cwd `web/`） | exit | 结果 |
|---|---|---|---|
| G1 | `nsh check` | **0** | 类型检查通过（生成 `.nsh/types`） |
| G2 | `nsh test --target tui` | **0** | **136 tests: 136 passed, 0 failed**（2m52s） |
| G3 | `nsh test --target tui src/surfaces/mcp-app/bridge.test.ts` | **0** | **17 tests: 17 passed** |
| G4 | `nsh test --target tui src/data/mcp-apps.test.ts` | **0** | **8 tests: 8 passed** |
| G5 | `nsh test --target tui --filter "MCP app" App.tui.test.tsx` | **0** | 1 test passed（TUI 降级用例） |
| G6 | `nsh build` | **0** | `App.tsx → .nsh/out`（930.3 KB js / 3.0 MB bytecode） |
| G7 | `nsh package --target web -o dist/web/keel` | **0** | `dist/web/keel/Keel`，10.3 MB（11.8s） |
| G8 | `node scripts/kit-imports.mjs --web web` | **1** | 仅 `web/src/ui/kit/components/styles.tsx:30` 的 raw-color；在基线 `1f2418c` 上同样存在（非本切片引入），本切片的 `screens/messages.tsx` 与新增 `surfaces/mcp-app/**` 无违规 |

### 附：非门禁的 headless `nsh test` 观察（诚实记录）

仓库文档的跑法是 `nsh test --target tui`（README「构建与运行」）。一次 headless `nsh test`
（不带 target）得到 `134 tests: 128 passed, 6 failed`：

- 5 条在基线 `1f2418c` 上同样失败（在 `../keel-p2fe-base` 临时 worktree 逐一复跑确认）：
  `本会话模型选择器`、`团队模式`、`命令面板-Mod+K`、`settings-capabilities` 的 503 用例、
  `container.test.tsx` 的 Table 滚动；
- 1 条是本切片新增的 TUI 用例在非 terminal 引擎上的必然差异：headless 引擎是桌面 idiom，
  `can("mcp.apps")` 为真，会走 WebView/目录路径（该引擎没有可加载的 web view，显示回退），
  而该用例断言的是 TUI 的「不挂 iframe」文案。TUI target 下 136/136 全绿（G2）。

## NSH API 选择

| 需求 | 选择 | 依据 |
|---|---|---|
| 承载 View | `WebView`（`nsh`），web target 运行时渲染为 `<iframe>`，桌面为平台 WebView | `website/docs/web.md`「Web views of other sites: WebView is an iframe」；`web/nsh.json` 已声明 `webview.hosts:["*"]` |
| View→Host | `WebView onMessage`（`{data:string}`） | 0.4.0 nightly `op_web`/`webLoaded` 的 `MessageChannel` 桥；页面侧 `window.nsh.postMessage` 仅在 `html` 形态由 NSH 注入，`src` 形态不注入任何宿主脚本 |
| Host→View | `WebViewRef.postMessage(string)` | 同上：`frame.contentWindow.postMessage(str,"*")` |
| 加载事件 | `onLoad` 启动 5s 握手计时；`onError` → 文本兜底 | `plugins.tsx` 既有 `WebView onError` 先例 |
| 全屏 | kit `Dialog`（solid-glass 令牌 + `nsh/base` Dialog），View 请求 `ui/request-display-mode` 后重挂载 | design §3.4「全屏用 kit 的 Dialog/Sheet」 |
| TUI | 不渲染 `WebView`：`setClientCapabilities({"mcp.apps": platform.idiom !== "terminal"})`，`can("mcp.apps")` 为假 → 文本兜底 | spec「TUI MUST NOT 渲染 app」；`plugins.tsx` 的 terminal 分支先例 |

文档 URL：web 用目录给的**同源相对路径** `/v1/mcp/apps/{appId}/view?v=<hash>`（NSH web 按页面
URL 解析）；桌面/移动 NSH 的 WebView 拒绝相对 URL（实测 `ERR_ACCESS_DENIED: ...names no host
a grant could add`），因此只在这些 target 上使用 `GET /v1/mcp/apps` 的 `sandboxOrigin` 拼绝对
地址；未配置则按指令「沙箱域为空 → 不渲染 iframe」回文本并在行内说明。`appDocumentUrl()` /
`appDocumentSource()` 对 appId、64 位 hash、路径、协议、凭据逐项校验，任何不一致都拒绝。

## 握手与 origin 校验设计

- `ui/initialize`（**含 appInfo.name 校验**）→ 回 `McpUiInitializeResult{protocolVersion:"2026-01-26",
  hostInfo, hostCapabilities, hostContext}`；收到 `ui/notifications/initialized` 之前**不发任何**
  `ui/notifications/*`；`initialized` 之后 `tool-input` **恰好一次** → 结果可用时 `tool-result`
  **恰好一次**（结果先到/后到两条路径都覆盖；重复 initialized 不重发）。
- 5s 握手超时（`HANDSHAKE_TIMEOUT_MS`）、单帧 64 KiB、200 帧/s、JSON-RPC 形状/`params` 对象、
  未知方法 `-32601` 后销毁；P3 面方法（`tools/call`/`resources/read`/`ui/message`/`ui/open-link`/
  `ui/update-model-context`/`ui/download-file`）明确回 `-32601` **但不销毁**（hostCapabilities 不声明它们，
  不假装成功）。
- `ui/notifications/size-changed` → 内联高度弹性（80–480），全屏时忽略；
  `ui/request-display-mode` 只接受 `inline|fullscreen` 且必须在该 View 的 `appCapabilities.
  availableDisplayModes` 内，`pip`/未声明 → 返回当前实际模式；`ui/resource-teardown` 用独立
  `host-<n>` id 段，1s 有界。
- origin：NSH 的 `WebView onMessage` 不携带 `event.origin`（0.4.0 nightly 实测），桥因此把
  「该 WebView 自身的消息通道」作为唯一可信来源；`handleMessage(data, origin?)` 在 transport
  能提供 origin 时执行校验（只接受 `"null"`/`about:srcdoc` 或配置的 sandbox origin）。Host→View
  用 NSH 固定的 `"*"` targetOrigin（运行时决定，app 代码无法改），帧内**不含任何宿主 cookie/
  令牌**，且 frame 为 opaque origin，只有该 frame 能收到。
- 不把 `details`/`structuredContent` 拼进宿主 DOM：只经 `Text` 渲染有界文本，`structuredContent`
  原样进 `postMessage`，从不 `innerHTML`。

## 已知偏离 / 未决（诚实清单）

1. **sandbox 属性无法精确为 `allow-scripts`**：NSH 0.4.0（bba76ef nightly）的 web 运行时在
   `op_web` 里固定写 `allow-forms allow-popups allow-popups-to-escape-sandbox allow-modals
   allow-downloads`（`html` 再加 `allow-scripts`），**没有** `sandbox` prop，app 代码无法收窄。
   同源 `src` 时它确实**不**加 `allow-same-origin`（opaque origin 成立），但
   `allow-popups-to-escape-sandbox` 是 spec MUST NOT 列表之外、且更危险的一项。本切片按现状交付并
   在此登记；建议向 NSH 提交「WebView sandbox 可配置 / 去掉 escape token」。CSP（`default-src
   'none'` 等）由文档响应头提供，不受此影响。
2. **第二 origin 的 web 双 iframe 未实现**（design 已知暂缓项 c）：web 端始终用同源文档（直接
   跨源 `src` 会被后端 `Content-Security-Policy: frame-ancestors 'self'` 阻断）。`sandboxOrigin`
   只在桌面/移动 target 作为绝对地址基座使用；web 不声称自己是第二 origin 沙箱。
3. **浏览器 E2E 未跑**：P2.6 验收里的「E2E 断言 sandbox 属性与 CSP 头」归 P6.2/P6.3；本切片给到
   `nsh check` + 17 条桥单测 + TUI 行为用例。P6 的浏览器用例应直接断言第 1 条的 sandbox 现状
   （并据此决定是否升级 NSH）。
4. **`structuredContent` 透传 256 KiB 上限**是服务端 P1.4 的账本截断；前端按收到的内容原样转发，
   不二次截断（桥的 64 KiB 单帧限制覆盖入站帧，出站 `tool-result` 可能超过 64 KiB——P3.8 的
   「单帧 64 KiB」双向限速尚未实现，属 P3）。
5. **权限（camera/mic/geolocation/clipboardWrite）未授予**：`hostCapabilities.sandbox` 只带
   `csp`，不带 `permissions`；`allow` 属性未写。确认门属 P3.6。
6. `ui/resource-teardown` 在组件卸载时发送后立即销毁（不阻塞 UI）；1s 有界的「等待响应」路径
   已在桥单测覆盖，但真实卸载不等待——P3.5 重做该语义时接管。
