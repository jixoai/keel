# P2 前端证据（feat-mcp-apps-events，P2.6/P2.7 + P2.8 前端）

日期：2026-11-20（复核修复批次） · 基线：`1f2418c` · 分支：`task/keel-mcp-p2fe` · worktree：
`/Users/kzf/Development/GitHub/jixoai/fde2e/.worktrees/keel-task-mcp-p2fe/web`

本文件记录**真实浏览器 E2E**（P0 复核修复）与全部门禁。上一版「web 用相对路径 + NSH onMessage」
的结论已被复核推翻，现按实测重写：

- NSH 0.4.0 nightly 的 web 运行时**拒绝相对 `src`**（`it names no host a grant could add`）；
- URL 形态 WebView **不建 MessageChannel**（`op_web`: `_token = html != null && js ? … : null`），
  页面 `parent.postMessage` 不会到达 NSH `onMessage`。

因此传输层改为下面的「绝对 URL + 页面 window 监听」方案，并以真实 Chrome 验收。

## 传输方案（web target）

| 方向 | 机制 |
|---|---|
| 文档地址 | **绝对同源 URL** `origin + /v1/mcp/apps/{appId}/view?v=<hash>&instance=<nonce>&reload=<n>`；`origin` 来自官方 `host.call("keel.info")`（1.5 s 竞争超时，失败时回退到同源 `window.top.location.origin`） |
| Host → View | `WebViewRef.postMessage(string)` → NSH URL 形态的 `frame.contentWindow.postMessage` |
| View → Host | app 文档 `parent.postMessage` 到页面 window；app 的 JS realm 与页面同源，在 `window.top` 上注册一个监听器，用 `event.source` 匹配 `top.document.querySelectorAll("iframe")` 中 `src` 带同一 `instance` nonce 的那一帧，再交给该实例的桥 |
| origin 校验 | 只接受 `event.origin === "null"`（sandbox 的不透明来源）；具体 origin 的帧一律不路由 |
| 导航防线（web） | **只有 load 事件防线真实生效**：`onLoad` 在 bridge 已 `initialized`（或已开始握手）后再次触发 → 销毁实例、`reload` 计数 +1、重新加载**宿主自己的**文档 URL。NSH web 的 `onNavigationStateChange` 恒报挂载 URL（自导航后仍报原 URL），`onFrameNavigation` 在 web 上显式 `return`，注释写明它是原生防线。|
| 导航防线（原生） | `onLoad` 重载 + `onNavigationStateChange` 的 URL（origin+pathname）与期望文档不一致 → 销毁重载；两条都真实。重复 `ui/initialize` → `-32602` + 协议销毁。|
| 重载预算 | 每实例 10 次自动导航重载；第 11 次拒绝：销毁实例、定格文本兜底、诊断 `protocol`，并尽力 `POST /v1/mcp/apps/audit` 写 `protocol.violation/refused`（detail 无 URL query；路由 P3.7 前 404 被吞）。手动重试重置预算。|
| cookie invariant | `cmd/keel/auth.go`：会话 cookie 恒 `SameSite=Lax`（注释写明绝不 None——opaque 帧的带凭据 fetch 是跨站，sandbox 不过滤网络层 cookie）。 |

**为什么不用 srcdoc wrapper**（复核建议的形态）：NSH 的 `html` 形态强制 sandbox 无
`allow-same-origin`，wrapper 是不透明来源；内层 iframe 指向宿主的 app 文档时，文档响应的
`Content-Security-Policy: frame-ancestors 'self'` 会拒绝不透明祖先。真实 Chrome 实测：
`Framing 'http://127.0.0.1:13677/' violates the following Content Security Policy directive:
"frame-ancestors 'self'". The request has been blocked.`。所以服务器 HTML 必须留在它自己的 URL
（CSP/hash 头随响应到达），改用页面 window 监听。

**残留（诚实登记）**：`onLoad` 是浏览器能可靠观察到的导航信号；第三方页面在自己的 `load`
事件之前发来的帧存在一个理论竞态窗口（它要拿到 tool-input/result 需先完成 initialize+
initialized，而重复 initialize 会被销毁）。E2E 的 `/evil.html` 尝试在加载时 `ping`，宿主在
其 load 后立即重载（见 `e2e/server-headers.txt` 的 `EVIL GET` → `APP-DOC …&reload=1`），
第三方页未获得任何 `ui/notifications/tool-*` 帧。彻底消除该窗口需要 NSH 上游给 URL 形态提供
文档绑定的 MessageChannel（或 sandbox 可配置），已作为上游建议记录。

## 真实浏览器 E2E（P0 验收）

- 产物：`nsh package --target web -o dist/web/keel`（10.3 MB）
- 服务器：`e2e/server.mjs`（端口 **13679**），静态站点 + 假 app 文档
  `/v1/mcp/apps/mcpapp_0123456789abcdef/view`；文档响应带
  `X-MCP-App-Mime`、`X-MCP-App-Hash`、CSP、`nosniff`、`Referrer-Policy`、immutable 缓存，
  并 `Set-Cookie: keel_host_secret=1`。
- 浏览器：Chrome（playwright-core，headless，1280×900）。
- 断言（`e2e/report.json` 的 `pass: true`，连续 3 次独立运行全过）：
  1. iframe `sandbox` 含 `allow-scripts`、**不含** `allow-same-origin`（现状还含 NSH 固定的
     `allow-forms/popups/popups-to-escape-sandbox/modals/downloads`，见偏离 1）；
  2. `ui/initialize` 应答含 `protocolVersion:"2026-01-26"`、`hostInfo.name:"Keel"`、完整的
     `hostContext`（theme/styles/displayMode/containerDimensions/locale/timeZone/platform）；
  3. 顺序：initialize 应答 → `tool-input` → `tool-result`（各一次）；
  4. `ui/notifications/size-changed {height:360}` 生效：iframe 实际盒高 **360px**；
  5. app 内脚本 `document.cookie`、`parent.document`、`localStorage` 均 `SecurityError`
     （不透明来源）；`fetch("/cookie-check",{credentials:"include"})` 到达服务器时
     `cookie=""`（网络日志实证，不发送宿主 cookie）；
  6. 自导航到 `/evil.html` 后：宿主加载 `…&reload=1` 的 app 文档并重新握手
     （`tool-input`/`tool-result` 再次到达），`/evil.html` 未获得任何 `ui/notifications/tool-*`；
  7. 文档 CSP 含 `sandbox allow-scripts`（与后端 `internal/mcphost/csp.go` 一致），与 iframe
     sandbox 属性取交集：app 内 `window.open` 返回 null（popup 被阻断）；
  8. 文档响应头逐项断言（CSP 含 `frame-ancestors 'self'` / `sandbox allow-scripts`、`nosniff`、`no-referrer`、
     `text/html;profile=mcp-app`、hash、immutable）。
- 证据文件：`e2e/report.json`、`e2e/console.txt`（导航重载的 `warning` 与 popup 被 CSP sandbox
  阻断的浏览器告警 = 预期）、
  `e2e/server-headers.txt`、`e2e/after.png`、`e2e/fake-app.html`、`e2e/evil.html`、
  `e2e/server.mjs`、`e2e/e2e.mjs`。
- 复跑：
  ```bash
  cd web && nsh package --target web -o dist/web/keel
  cd ../openspec/changes/feat-mcp-apps-events/evidence/p2/frontend/e2e
  # server.mjs 里的 ROOT 指向该 worktree 的 dist/web/keel/Keel
  node server.mjs &            # 13679
  node e2e.mjs                 # exit 0 = pass
  ```

## 门禁（复核修复批次）

| # | 命令（cwd `web/`） | exit | 结果 |
|---|---|---|---|
| G1 | `nsh check` | **0** | 类型检查通过 |
| G2 | `nsh test --target tui` | **0** | **145 tests: 145 passed, 0 failed** |
| G3 | `nsh test --target tui src/surfaces/mcp-app/bridge.test.ts` | **0** | **18 tests**（含 200 帧/s 限速） |
| G4 | `nsh test --target tui src/data/mcp-apps.test.ts` | **0** | **8 tests**（绝对 URL/instance/reload/凭据拒绝） |
| G5 | `nsh test --target tui src/data/capabilities.test.ts` | **0** | **4 tests**（未注册特性 = false、部署半门） |
| G5b | `nsh test --target tui src/surfaces/mcp-app/reload-budget.test.ts` | **0** | **4 tests**（10 次允许/第 11 次拒绝、上限可配、循环停止、审计 detail 无 query） |
| G6 | `nsh build` | **0** | `App.tsx → .nsh/out`（945.2 KB js / 3.1 MB bytecode） |
| G7 | `nsh package --target web -o dist/web/keel` | **0** | 10.3 MB |
| G8 | `PLAYWRIGHT_CORE=<.../playwright-core/index.mjs> node e2e/e2e.mjs` | **0** | `pass: true`（显式 env；含 popup 阻断与 CSP sandbox） |
| G9 | `node scripts/kit-imports.mjs --web web` | **1** | 仅 `web/src/ui/kit/components/styles.tsx:30` 的基线已有 raw-color；本切片无新增 |
| G10 | `openspec validate feat-mcp-apps-events --strict` | **0** | valid |

### 附：headless `nsh test`（非门禁）

仓库文档的跑法是 `nsh test --target tui`。headless 引擎是桌面 idiom，会走 WebView/目录路径；
5 条既有失败在基线 `1f2418c` 上同样存在（本会话已逐一复跑确认），新增的 TUI 用例在 headless
下的差异是「没有 terminal idiom」的必然结果，不改变 `--target tui` 的 145/145。

## NSH API 选择（复核后）

| 需求 | 选择 |
|---|---|
| 承载 View | `<WebView src>`（web 为 `<iframe>`；桌面为平台 WebView） |
| Host→View | `WebViewRef.postMessage` |
| View→Host（web） | `window.top` message 监听 + `event.source`↔iframe `src` 的 `instance` nonce 匹配 |
| View→Host（原生） | NSH `onMessage`（每个 WebView 的独立通道） |
| 导航/重载 | `onLoad` + `onNavigationStateChange` → 销毁实例、`reload` 参数重载 |
| 全屏 | kit `Dialog`；TUI 不挂 `WebView`（`can("mcp.apps")` 为假） |

## 降级面（本批补齐）

- `can()`：**客户端未注册的特性一律 false**（undefined 不再回落到部署位）；新增
  `deploymentCan()` 供 TUI 读取目录。
- TUI：`can` false 时仍加载目录，显示 **app 标题 + 文档路径 + 文本结果**（spec「标题、链接与
  文本兜底」）。
- `catalogue.ts`：`force` 重试不再被 in-flight 吞掉（request token，旧应答被忽略）。
- 限速：每实例 200 帧/s；实例重建（display-mode/导航）开始新窗口，Host 级跨实例预算属 P3.8。
- fixture：app 条目带诊断「文档路径由浏览器 E2E 服务器提供；普通 dev server 没有该路由」，
  浏览器回退按真实加载/授权/超时原因呈现，不伪造「没有文档」。

## 复核 P2 卫生项（本批）

1. **web 导航空转已标注**：`view.tsx` 的 `onFrameNavigation` 在 `platform.os === "web"` 时
   直接返回并写明原因（NSH web 恒报挂载 URL；web 真正生效的是 load 事件防线）。原生 target
   两条防线都真实。E2E 的 `/evil.html` 重载由 load 防线触发（服务器日志实证）。
2. **重载预算 + 审计**：`reload-budget.ts`（默认 10 次，单测 2 条）；第 11 次销毁实例、定格
   文本兜底、诊断 `protocol`，并 `POST /v1/mcp/apps/audit` 写
   `{action:"protocol.violation", result:"refused", detail:"reload budget exhausted (10)"}`
   （detail 无 URL query；P3.7 前路由 404，失败被吞、不影响 UI）。手动重试重置预算。
3. **cookie invariant**：`cmd/keel/auth.go` 注释写明会话 cookie 恒 `SameSite=Lax`/`Strict`，
   绝不 `None`；行为未改。
4. **陈旧证据**：`merged-verification.txt` 头部加
   `Superseded by fix-merged-verification.txt（web 传输 P0 修复，f55d290）`，原文保留作历史。
5. **E2E 可复跑**：`e2e.mjs` 改为
   `PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs node e2e.mjs`（未设置时回落本机 npx
   路径）；本批用显式 env 重跑，`pass:true`。

## 已知偏离 / 未决（诚实清单）

1. **sandbox 属性的字面值无法精确为 `allow-scripts`（但有效权限已被 CSP 收紧）**：
   NSH 0.4.0（bba76ef nightly）的 web 运行时固定加 `allow-forms allow-popups
   allow-popups-to-escape-sandbox allow-modals allow-downloads`（`html` 再加 `allow-scripts`），
   没有 `sandbox` prop；同源 `src` 确实**不加** `allow-same-origin`（E2E 断言 opaque origin：
   cookie/DOM/localStorage 全部 SecurityError）。后端文档 CSP 自带 `sandbox allow-scripts`
   （`internal/mcphost/csp.go`），与 iframe 属性取**限制交集**，E2E 断言 `window.open` 返回
   null。因此有效权限 = allow-scripts；仍建议 NSH 上游提供 `sandbox` prop（属性字面值可审计、
   直接打开文档也一致），这是 defense-in-depth 而非当前越权。
2. **第二 origin 双 iframe 未做**（design 暂缓 c）：web 始终用同源文档（跨源直接 `src` 会被
   `frame-ancestors 'self'` 阻断；srcdoc wrapper 实测同样被阻断）。`sandboxOrigin` 只在桌面/
   移动作为绝对地址基座。
3. **导航前的理论竞态**：见「传输方案」残留段；`onLoad` 后的销毁/重载已由 E2E 证明。彻底修复
   依赖 NSH 上游的文档绑定通道。
4. **出站帧未限 64 KiB**：入站帧限制在；`tool-result` 可能超 64 KiB，双向限速属 P3.8。
5. **权限未授予**：`hostCapabilities.sandbox` 不带 `permissions`，iframe 无 `allow` 属性；P3.6。
6. **卸载 teardown 不阻塞**：发送后立即销毁；1 s 有界等待路径有单测，真实语义留 P3.5。
7. P2.8 仍未勾：浏览器 E2E 一格已在本批完成，但审计表 `mcp_app_audit` 属 P3.7；CSP 头断言已在
   P2.6 E2E 覆盖。
