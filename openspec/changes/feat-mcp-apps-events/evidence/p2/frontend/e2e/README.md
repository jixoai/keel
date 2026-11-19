# Web 传输桥 · 真实浏览器 E2E（P2.6 / 复核 P0）

本目录是复核修复批次的可复跑证据。它证明 keel 的 web 构建在真实 Chrome 里完成
`initialize → initialized → tool-input → tool-result → size-changed`，并且 app 文档
（opaque origin）读不到宿主 cookie/DOM，自导航到第三方后宿主销毁并重载自己的文档。

## 文件

| 文件 | 用途 |
|---|---|
| `server.mjs` | 静态站点 + 假 app 文档（`/v1/mcp/apps/mcpapp_0123456789abcdef/view`，带 CSP/hash/`Set-Cookie`）+ `/cookie-check` + `/evil.html`；端口 13679；请求与响应头写入 `server-headers.txt` |
| `fake-app.html` | View 侧假 app：发 `ui/initialize`，应答后发 `initialized` 与 `size-changed`；记录收到的 `tool-input`/`tool-result`；检查 cookie/parent DOM/localStorage；`fetch("/cookie-check",{credentials:"include"})` |
| `evil.html` | 第三方页：加载时发 `ping`（尝试继承已授权通道），并尝试向 `/evil-report` 上报它收到的帧 |
| `e2e.mjs` | playwright-core 驱动：开 `?fixture=1` → 会话「天气面板」→ 展开工具行 → 断言全链路、尺寸、隔离、导航防线、响应头 |
| `report.json` | 最后一次通过的完整断言数据（`pass: true`） |
| `console.txt` | 浏览器控制台（空 = 无错误） |
| `server-headers.txt` | 文档响应头逐项 + `COOKIE-CHECK cookie=""` + `EVIL GET` → `APP-DOC …&reload=1` 的时序 |
| `after.png` | 断言完成时的整页截图 |

## 复跑

```bash
# 1) 构建被测 web 产物（worktree 根）
cd web
nsh package --target web -o dist/web/keel

# 2) 证据目录：server.mjs 的 ROOT 默认指向本 worktree 的产物；
#    换机器/换 worktree 时改 ROOT 这一个常量。
cd ../openspec/changes/feat-mcp-apps-events/evidence/p2/frontend/e2e
node server.mjs &                       # 127.0.0.1:13679
node e2e.mjs                            # exit 0 = pass；playwright-core 路径在文件首行

# 3) 清理
kill %1
```

`e2e.mjs` 顶部用绝对路径引用 npx 缓存里的 `playwright-core`（本机
`/Users/kzf/.npm/_npx/e41f203b7505b1fb/node_modules/playwright-core`）；换成
`NODE_PATH` 或 `npm i playwright-core` 亦可。浏览器用系统 Chrome
（`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`）。

## 关键实测结论

- NSH web 运行时**拒绝相对 `src`**（`it names no host a grant could add`）；必须
  `origin + path`，origin 从 `keel.info` 取。
- URL 形态 WebView 不注入 `window.nsh` / 不建 MessageChannel；页面→宿主只能走
  `window.top` 的 message 监听（`event.origin === "null"` + `event.source` 对应带
  `instance` nonce 的 iframe）。
- srcdoc wrapper 方案被 `frame-ancestors 'self'` 阻断（不透明 wrapper 祖先）；见
  `../gates.md` 的实测记录。
- iframe sandbox 现状：`allow-forms allow-popups allow-popups-to-escape-sandbox
  allow-modals allow-downloads allow-scripts`，**无** `allow-same-origin`；
  `document.cookie`/`parent.document`/`localStorage` 均 `SecurityError`，
  `cookie-check` 到达服务器时 `cookie=""`。
- 自导航 `/evil.html` 后：`EVIL GET` 之后立即 `APP-DOC …&reload=1`，新实例重新完成
  `tool-input`/`tool-result`；`evilReports` 为空（第三方页在重载前没来得及上报；
  它也未收到任何 `ui/notifications/tool-*`）。
