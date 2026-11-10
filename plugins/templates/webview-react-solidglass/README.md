# {{title}}（`{{id}}`）

一个 **webview + React + solid-glass** 插件包：宿主在 `/plugins/{{id}}/` 托管本包
的 UI，UI 只通过宿主的 `keel.plugin.*` 桥接拿数据；工具由 `backend/server.mjs`
（stdio 行 JSON-RPC 的 `tool_provider`）提供，模型与桥接调的是同一个实现。

## 包里有什么

| 文件 | 作用 |
|---|---|
| `plugin.yaml` | 清单：id / version / kind / provides（工具 + UI）/ permissions / entry |
| `ui/index.html` | UI 入口（`provides.ui.entry` 指向它） |
| `ui/app.js` | 已构建的前端产物（React 已打包进来，宿主 CSP 只允许同源脚本） |
| `ui/app.jsx` | 前端源码（改这里，再重建 `ui/app.js`） |
| `ui/theme.css` | 版式；颜色/圆角/字体全部消费宿主的主题令牌，不自造设计系统 |
| `ui/package.json` | 只用于重建 UI（`cd ui && npm install && npm run build`） |
| `backend/server.mjs` | 后端：`initialize` / `tools/list` / `tools/call` / `close` |

## 怎么改

- **改工具**：改 `backend/server.mjs` 的 `tools` 与 `runTool`；工具名必须和
  `plugin.yaml` 的 `provides.tools` 一致（未声明的工具会被宿主拒绝并写审计）。
- **改界面**：改 `ui/app.jsx`，然后在 `ui/` 里 `npm install && npm run build`。
  只用 `theme.css` 里那套 CSS 变量（由宿主 `bridge/state` 的 `theme.cssVars` 写入），
  不要写死颜色——宿主切换明暗主题时插件跟着变。
- **改元信息**：`plugin.yaml` 的 `description` / `provides.ui.title` / `version`。

## 怎么装

```bash
# 安装（走 durable 审批：先 202 拿 decisionId，答完带同一个 requestId + decisionId 重发）
curl -sS -X POST "$KEEL/v1/plugins/install" -H "content-type: application/json" \
  -H "x-tenant-id: $TENANT" -d '{"path":"<本目录>"}'
curl -sS -X POST "$KEEL/v1/plugins/{{id}}/enable" -H "content-type: application/json" \
  -H "x-tenant-id: $TENANT" -d '{}'
# 打开
open "$KEEL/plugins/{{id}}/"
```

也可以让 `keel.creation` 的 `creation_install` 工具代劳：它会替你走审批并返回深链。

## 怎么调

- **UI 调自己的工具**：页面上的「调用 {{tool}}」按钮 → `POST /v1/plugins/{{id}}/bridge/call`
  → 宿主注册表 → 本包 `backend/server.mjs` 的 `tools/call`。
- **模型调**：工具以 `{{publishedTool}}` 出现在模型面（宿主把 `<plugin>.<tool>` 归一为
  wire 安全的名字），调用同样落到 `backend/server.mjs`。
- **排障**：`GET /v1/plugins/{{id}}/bridge/state` 看宿主下发的状态与主题；
  `GET /v1/plugins/audit?plugin={{id}}` 看生命周期与越权审计。

## 边界

后端只拿到宿主白名单内的环境变量（`KEEL_PLUGIN_*` 等），拿不到宿主密钥；
网络/文件/密钥能力必须在 `plugin.yaml` 的 `permissions` 声明并经宿主中介。
