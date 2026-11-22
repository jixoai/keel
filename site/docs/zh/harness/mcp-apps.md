# MCP Apps（交互式工具结果）

> **已实现**（P1–P5；P6 验收进行中）。keel 在 MCP 里是 **host/client**：它连接服务器、
> 取回服务器声明的 `ui://` HTML、在沙箱里渲染，并把 app 的动作代理回该服务器。
> 本文面向使用者和 MCP 服务器作者；运行时细节与 HTTP 契约见
> [`docs/HOST.md`](https://github.com/jixoai/keel/blob/main/docs/HOST.md) 的 MCP 段。

MCP Apps 是官方 MCP 扩展（SEP-1865，扩展标识 `io.modelcontextprotocol/ui`，Stable
2026-01-26）。没有它，一个工具只能返回文本；有了它，工具结果可以是一块可交互的界面：
表格、表单、图表、确认按钮。界面仍然是**服务器提供的 HTML**，keel 只负责托管、沙箱、
桥接与审计。

## 1. 它能做什么

一次工具调用可以带一个 app：

```
模型调用 show_dashboard
  → 服务器返回普通 CallToolResult（文本仍在）
  → 结果 details 里带 app 绑定
  → 客户端在工具结果卡内挂载 iframe
  → iframe 通过 postMessage 桥调用 app 自己的工具、发消息、开链接
```

三个约束决定了它的形态：

1. **文本永远在**。app 只是结果卡的一种渲染，失败/不支持时回退到可读文本；
2. **app 不能直连服务器**。所有出站动作经宿主代理，宿主做归属校验、确认与审计；
3. **app 跑在不透明来源**。web 的 iframe 没有 `allow-same-origin`，读不到宿主 DOM、
   cookie 与 storage；文档响应还带严格 CSP。

## 2. 服务器如何声明一个 app

### 2.1 工具声明界面

工具用 `_meta.ui.resourceUri` 指向一个 `ui://` 资源：

```json
{
  "name": "show_dashboard",
  "description": "Show the account dashboard",
  "_meta": {
    "ui": {
      "resourceUri": "ui://dashboard/main",
      "visibility": ["model", "app"]
    }
  }
}
```

- `visibility` 缺省是 `["model","app"]`。不含 `"model"` 的工具**不会**出现在模型工具面；
  不含 `"app"` 的工具不能被 app 调用。
- 旧服务器可能用平铺键 `_meta["ui/resourceUri"]`；keel 仍可读取，但结构化 `_meta.ui`
  优先。两种写法并存只建一个 app。
- 一个 `ui://` 资源可以被多个工具引用；目录里只出现一次，并列出全部引用者。

### 2.2 资源声明 CSP 与权限

`resources/read` 返回的 app 文档内容带 `_meta.ui`：

```json
{
  "uri": "ui://dashboard/main",
  "mimeType": "text/html;profile=mcp-app",
  "_meta": {
    "ui": {
      "csp": {
        "connectDomains": ["https://api.example.com"],
        "resourceDomains": ["https://cdn.example.com"],
        "frameDomains": [],
        "baseUriDomains": []
      },
      "permissions": {
        "clipboardWrite": true
      },
      "prefersBorder": true
    }
  }
}
```

规则：

- `mimeType` 必须是 `text/html;profile=mcp-app`；其它类型不会成为可渲染 app。
- 单文档 ≤ 2 MiB；单条 `_meta` ≤ 64 KiB；只允许 `text` 或 base64 `blob` 之一。
- CSP 由 keel 构造，服务器只能提供**域列表**，不能引入指令或 `*`/`data:` 之类的宽源。
  没有 `csp` 时使用严格缺省：不连接任何外部域、不嵌 frame。
- `permissions` 当前会被解析并随目录发布，但**不会授予**（见 §8）。
- `prefersBorder` 是展示偏好；`domain` 目前只解析、不随目录下发（见 §8）。

## 3. 宿主如何渲染

### 3.1 目录与文档

宿主在连接后取回并校验每个声明的文档，生成目录：

```bash
# 机器客户端用绑定到 Bot 的 key；浏览器用账号会话 cookie
curl -sS -H "Authorization: Bearer $KEEL_AGENT_TOKEN" https://<host>/v1/mcp/apps
```

每个 app 条目包含 `appId`、`server`、`title`、`resourceUri`、`tools`、`visibility`、
`csp`、`permissions`、`prefersBorder`、`hash`、`documentUrl` 与诊断。`appId` 是宿主
从 `(server, resourceUri)` 派生的，不是服务器可控字符串；文档 URL 带内容 hash，
内容变化即换 URL。

客户端**不要**自己从工具结果拼文档地址：目录是绑定的唯一机器可读来源。

### 3.2 内联与全屏

- 内联：工具结果卡内，弹性高度（80–480 px），app 可用 `ui/notifications/size-changed`
  调整；
- 全屏：kit 的 Dialog；进入/退出由 `ui/request-display-mode` 协商，宿主只切到 View
  声明过的模式；
- `pip` 不是宿主支持的模式：请求 `pip` 会得到当前实际模式。

### 3.3 握手与数据流

```
View                                   Host
  │  ui/initialize ───────────────────▶ │
  │ ◀───────────────── result ───────── │  protocolVersion / hostInfo / hostContext
  │  ui/notifications/initialized ────▶ │
  │ ◀──────────── tool-input ────────── │  恰好一次
  │ ◀──────────── tool-result ───────── │  结果可用时恰好一次
```

- 宿主在收到 `initialized` 之前**不会**发任何 `ui/notifications/*`；
- 握手超时默认 5 秒；超时销毁 iframe，回文本兜底；
- `hostContext` 含 theme、`styles.variables`、displayMode、availableDisplayModes、
  `containerDimensions`、locale、timeZone、platform；没有的值不塞空字段；
- 主题变化发部分 `host-context-changed`，不重载文档；
- 卸载前发 `ui/resource-teardown`，等待最多 1 秒（当前 web 卸载路径发送后立即销毁，
  见 §8）。

### 3.4 主题

宿主把 `web/theme.json` 的令牌映射成 apps 标准 CSS 变量（`--color-background-primary`
等），随 `hostContext.styles.variables` 下发。View 没声明变量时使用自己的缺省；
宿主不会注入 `@font-face`。

## 4. 动作、确认与审计

View 的所有出站动作都经宿主 HTTP 面代理，`appId` 是唯一由客户端提供的身份；服务器名
由目录派生，body 里写别的服务器名无效。

| 动作 | 宿主行为 | 用户确认 |
| --- | --- | --- |
| `tools/call` | 只允许该 app 所属服务器、`visibility` 含 `"app"` 的工具；返回标准 `CallToolResult` | 破坏性工具（`destructiveHint` 或变更族名字）逐次确认 |
| `resources/read` | 只允许该服务器；URI 只允许 `ui://` 与 `http(s)://` | 否 |
| `ui/message` | `role` 只能是 `user`，`delivery` 只能是 `queue`；send 落账本，edit 只预填 composer，discard 不写 | 默认 send/edit/discard 三选一 |
| `ui/open-link` | 只允许 http(s)，拒绝凭据与非 ASCII 主机；返回目标 URL 给前端以 `noopener,noreferrer` 打开 | 默认打开/取消 |
| `ui/update-model-context` | 覆盖式写入该 app 的会话上下文，下一次 attempt 以 `<mcp_app_context>` 并入最后一条 user 消息；不写账本 | 否 |

- 确认等待默认 120 秒（上限 10 分钟）；单会话未答 app 确认上限 16，超过回 `-32000`；
- 破坏性工具被拒绝时不产生上游调用；
- 审计表 `mcp_app_audit` 记录动作、结果与有界摘要，**不含消息正文、凭据、token 或完整
  URL query**。

## 5. 降级与故障

| 情况 | 用户看到什么 |
| --- | --- |
| 服务器没有 `_meta.ui` | 纯文本结果，和没有 apps 时完全一样 |
| 客户端没有 `mcp.apps` 能力，或运行在 TUI | app 标题 + 文档路径 + 文本结果；不挂 WebView |
| 文档取回/类型/超 2 MiB/缓存失败 | 文本结果 + 失败原因 + 重试；该 app 不进目录 |
| 重取失败但已有旧版本 | 继续用旧版本，不出现空白 |
| 握手 5 秒超时 | 销毁 iframe，回文本 + 原因 + 重试 |
| 未知方法、超 64 KiB 帧、超 200 帧/秒 | 协议违规：销毁实例、回文本、写审计 |
| CSP 拒绝外部域 | app 仍在，但请求被浏览器挡住；宿主不会放宽策略 |
| app 请求非 app 可见的工具 | `403` + 审计 `refused`，没有上游调用 |
| 用户拒绝破坏性调用/消息/链接 | 不执行；审计可见 |

任何一格都不出现空白区、假 UI 或“假成功”。

## 6. 能力位与配置速查

### 6.1 能力位

`GET /v1/capabilities` 的 `features`：

| 位 | 含义 |
| --- | --- |
| `mcp.apps` | 宿主有 app 目录/文档面；为 false 时客户端不挂 iframe |
| `mcp.apps.tools` / `mcp.apps.resources` | 桥可以代理工具/资源 |
| `mcp.apps.message` | 桥可以走确认后的消息提交 |
| `mcp.apps.links` | 桥可以走确认后的链接 |
| `mcp.apps.context` | `ui/update-model-context` 真正落库 |
| `mcp.apps.sandboxOrigin` | 配置了桌面/移动 WebView 的独立文档基址；**不表示双 iframe 沙箱** |
| `mcp.events` | 通知/订阅面已接线（`subscriptions/listen` 等） |

### 6.2 环境变量

| 变量 | 作用 |
| --- | --- |
| `KEEL_MCP_CONFIG` / `--mcp` | 全局 `mcp.json` |
| `KEEL_MCP_PROJECT` / `--mcp-project` | 项目 `mcp.json`（同名条目可覆盖全局） |
| `KEEL_MCP_PROTOCOL_ERA` | `auto`（默认）/`legacy`/`modern`；非法值回 legacy |
| `KEEL_MCP_SAMPLING` | `off`/`session`（默认）/`on`；MRTR sampling 开关 |
| `KEEL_MCP_APP_ORIGIN` | 可选绝对 http(s) origin；只作桌面/移动 WebView 基址，web 不用 |

`mcp.json` 使用共享 `mcpServers` 形状：

```json
{
  "mcpServers": {
    "dashboard": {
      "command": "npx",
      "args": ["-y", "@example/mcp-dashboard"],
      "enabled": true
    }
  }
}
```

修改 `mcp.json` 后重启 `keel serve` 生效；多租户部署每进程一个租户，MCP 连接配置是
进程级的。

### 6.3 关键端点

| 端点 | 用途 |
| --- | --- |
| `GET /v1/mcp/servers` | 连接状态、`era`、`protocolVersion`、`catalogueError`、工具/app 计数 |
| `GET /v1/mcp/apps` | app 目录、主题、limits |
| `GET /v1/mcp/apps/{appId}/view?v=<hash>` | app 文档（CSP/nosniff/no-referrer/immutable） |
| `POST /v1/mcp/apps/{appId}/tools/call` | 代理工具调用 |
| `POST /v1/mcp/apps/{appId}/resources/read` | 代理资源读取 |
| `POST /v1/mcp/apps/{appId}/ui/message` | 确认后的消息 |
| `POST /v1/mcp/apps/{appId}/ui/open-link` | 确认后的链接 |
| `POST /v1/mcp/apps/{appId}/context` | 记录 app 上下文 |
| `GET /v1/mcp/apps/audit` | 审计读 |

所有 `/v1` 路由都在鉴权中间件之后：账号 cookie 或 Bot key 决定租户，`X-Tenant-ID` 由
`keel serve` 注入；客户端不手工发送，自发头会被剥离。

### 6.4 限额

| 项 | 值 |
| --- | --- |
| 单文档 | 2 MiB |
| 文档缓存 | 64 MiB（字节 LRU） |
| `_meta` | 64 KiB |
| 单 app 诊断 | 32 条 |
| `structuredContent` 落 details | 256 KiB（超限标记截断） |
| 桥单帧/请求体 | 64 KiB |
| 桥速率/并发 | 200 请求/秒、单 app 并发 4 |
| 握手超时 | 5 秒 |
| teardown 等待 | 最多 1 秒 |
| 导航重载预算 | 每实例 10 次 |
| MRTR 往返 | 5 轮 |
| sampling | 每调用 3 次、`maxTokens` 上限 4096 |

## 7. 排障

| 现象 | 先检查 |
| --- | --- |
| 看不到 app | `/v1/capabilities` 的 `mcp.apps`；`/v1/mcp/servers` 的 `connected` 与 `catalogueError`；`/v1/mcp/apps` 是否为空 |
| app 在目录里但结果卡不挂载 | 工具结果的 `details.app` 是否存在；客户端是否 TUI；能力位是否被部署关掉 |
| `/view` 404 | appId 形状、hash 是否过期、租户是否匹配；未知/跨租户统一 404 是预期 |
| app 内 `fetch` 被 CSP 挡 | 服务器是否在 `_meta.ui.csp.connectDomains` 声明了该域；宿主不会放宽 |
| 5 秒后界面消失 | View 是否发了 `ui/initialize` 与 `initialized`；是否有未知方法/超大帧 |
| `tools/call` 403 | 工具是否属于该服务器、`visibility` 是否含 `"app"` |
| 请求一直 202 | 破坏性调用/消息/链接在等用户 decision；检查审批队列与未答上限 16 |
| 429 | 单 app 200 请求/秒或并发 4 已满；拒绝审计按秒采样 |
| 终端里只有文字 | 预期：TUI 不挂 WebView，显示标题/文档路径/文本 |

指标：`mcp_servers{state}`、`mcp_surface_generation`、`mcp_app_fetch_total{result}`、
`mcp_app_bridge_frames_total{method,direction}`、`mcp_listen_reconnects_total`、
`mcp_mrtr_rounds`。日志关键字：`mcp app audit`、`mcp catalogue audit`、`mcp server log`、
`mcp listen`。

## 8. 当前限制（诚实清单）

1. **浏览器权限不授予**：`camera`/`microphone`/`geolocation`/`clipboardWrite` 需要 NSH
   的 per-permission `allow` 控制面；当前 `hostCapabilities` 不声明、iframe 无 `allow`，
   app 必须特性检测并按无权限降级。这是 P3.6 的上游依赖。
2. **第二 origin 双 iframe 暂缓**：`KEEL_MCP_APP_ORIGIN` 只作为桌面/移动 WebView 的绝对
   文档基址；web 始终同源 opaque frame。spec 的 sandbox 代理页 + 内层 View 形态未实现。
3. **客户端能力声明未实现**：`initialize` 的 `ClientCapabilities` 未写
   `io.modelcontextprotocol/ui`（`extensions`/`experimental` 双位置），也没有
   `mimeTypes`。服务器不能据能力协商判断宿主支持 apps；请按无条件声明 `_meta.ui` 工作。
4. **`_meta.ui.domain` 未随目录下发**：当前只解析/校验，`/v1/mcp/apps` 条目没有该字段。
5. **teardown 等待不阻塞**：web 卸载路径发送 `ui/resource-teardown` 后立即销毁；1 秒
   有界等待只在桥单测覆盖。
6. **SDK 2.x 独有面**：`ui/download-file` 回 `-32601`；
   `ui/notifications/request-teardown` 按未知通知销毁；`tool-input-partial` 不发；
   `pip` 不支持。

这些限制不影响文本结果的可读性；它们影响的是“更丰富的交互面”，不是工具调用本身。
