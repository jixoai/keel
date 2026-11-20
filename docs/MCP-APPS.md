# MCP Apps 宿主（服务端）

> 状态：P2 后端 + 前端 View；P3 桥与出站动作；P4 events；P5 MRTR/era/app context
> 均已交付（P6 验收进行中）。当前运行时/HTTP 面的完整说明以
> [`HOST.md`](HOST.md) §5 为准；本文保留 P2 建立的服务端细节。
> 设计依据：`openspec/changes/feat-mcp-apps-events/design.md` §2/§3；
> 验收证据：`openspec/changes/feat-mcp-apps-events/evidence/`（p2–p5）。

## 端点

| 路由 | 语义 |
|---|---|
| `GET /v1/mcp/servers` | MCP 机群投影：`generation`、连接状态、`protocolVersion`/`era`、能力、工具/应用计数、诊断 |
| `GET /v1/mcp/apps` | app 目录（`AppSpec` 投影：appId/server/tools/resourceUri/visibility/csp/permissions/prefersBorder/hash/documentUrl），以及 `theme` 调色板与 `limits` |
| `GET /v1/mcp/apps/{appId}/view` | app 文档本体；`?v=<hash>` 为内容身份 |
| `POST /v1/mcp/apps/{appId}/tools/call` | 宿主代理工具调用（只允许该服务器、`visibility` 含 `app` 的工具） |
| `POST /v1/mcp/apps/{appId}/resources/read` | 宿主代理资源读取（只允许该服务器与 `ui://`/`http(s)://`） |
| `POST /v1/mcp/apps/{appId}/ui/message` | `ui/message` → decision（send/edit/discard） |
| `POST /v1/mcp/apps/{appId}/ui/open-link` | `ui/open-link` → 确认后返回目标 URL |
| `POST /v1/mcp/apps/{appId}/context` | `ui/update-model-context` 覆盖式落库 |
| `GET/POST /v1/mcp/apps/audit` | 审计读 / 宿主侧动作审计（闭集） |

全部路由都要求 `X-Tenant-ID`（浏览器会话由 `keel serve` 的鉴权中间件注入），
租户判定先于任何目录/文档字节。未接线 registry 时读路径 501；桥动作按各自 seam
回 501/503，不用假成功。

## 身份与缓存

- `appId = "mcpapp_" + hex(sha256(server + "\x00" + resourceUri))[:16]`，**只**用派生身份；
  服务器字符串不进 URL/缓存键。
- 文档身份 = 内容 sha256。`documentUrl` 带 `?v=<hash>`：
  - 带匹配 hash：`Cache-Control: public, max-age=31536000, immutable`；
  - 裸 URL：`Cache-Control: no-store`（内容会变，禁止中间层缓存）；
  - 陈旧 hash / 未知 appId / 跨租户：统一 404，不泄露服务器清单。
- 同一 `ui://` 资源被多个工具引用 → 目录只出现一次，`tools` 列出全部引用者。

## CSP 策略

每个文档响应头由 `_meta.ui.csp` 构造（服务器只能贡献域列表，**不能**引入指令）：

```
sandbox allow-scripts; default-src 'none';
script-src/style-src 'self' 'unsafe-inline' <resourceDomains>;
img-src/media-src 'self' data: <resourceDomains>;
font-src 'self' <resourceDomains>;
connect-src 'self' <connectDomains>;
frame-src <frameDomains|'none'>; object-src 'none';
base-uri <baseUriDomains|'self'>; frame-ancestors 'self'
```

- `sandbox allow-scripts`：文档即使被直接打开也保持 opaque origin（iframe 属性之外的第二道约束）。
- 通配只接受**至少两级**标签的子域授予（`https://*.example.com`）；
  `https://*.com`、`https://*.net`、`https://*.localhost`、`https://*.com.` 一律拒绝。
- 默认拒绝 loopback/私网/链路本地（`127.0.0.1`、`::1`、`10/8`、`172.16/12`、`192.168/16`、
  `169.254/16`、`localhost`/`*.localhost`、全数字主机如 `2130706433`）。
  仅 dev fixture 通过 Go 选项 `mcphost.Options{AllowPrivateDomains: true}` 显式放开；
  该选项**不是**环境变量，生产部署不应打开。
- 端口限 1–65535；域去重、单指令 ≤16 个、单域 ≤256 字节。
- 公共后缀列表（PSL）判定**未实现**：当前规则拒绝单标签 TLD；`*.co.uk` 这类
  「两级以上但仍是公共后缀」的通配仍是一个已知残留面（登记在 backlog，成本 > 当前收益）。

## 租户语义（运维必读）

`mcphost.Registry` 的 `TenantID` 决定目录可见性：

- `TenantID = uuid.Nil`：**进程级目录**（无租户隔离）——只适用于单租户进程。
  HTTP 端点仍要求 `X-Tenant-ID`，任何已鉴权租户都能读到这份进程级目录。
- `TenantID = <tenant>`：目录只对该租户可见；其它租户得到空目录，文档 404。
  `keel serve` 用 serve 进程的租户（`KEEL_SERVE_TENANT`）构造 registry。

**多租户部署必须每进程绑定一个租户**：MCP 连接配置（`mcp.json`）是进程级的，
本阶段没有按租户加载的 MCP 配置。一个进程服务多个租户时，MCP 目录对所有租户相同，
不能假装按租户隔离；请拆分为每租户一个 `keel serve`（或保持 `TenantID=0` 的单租户部署）。

## 降级面与诊断

| 场景 | 行为 |
|---|---|
| 无 apps 服务器 | 目录为空；`mcp_servers` 段照常，`mcp_apps` 不出现 |
| 文档取回失败 / 类型不符 / 超 2 MiB / 缓存装不下 | app 不进目录；诊断 + 审计；工具文本结果不变 |
| 重取失败（已有旧版本） | 保留旧版本继续服务（不出现空白），诊断记录失败 |
| app 未发布 | 工具结果无 `details.app`，`mcp_apps` 无该行（半成品 app 不可挂载） |

诊断条数每 app 上限 32 + 一条 `…N more dropped`；`_meta` 总量上限 64 KiB（超界整块忽略，
文档仍可用）。审计已落持久表 `mcp_app_audit`（P3.7；FORCE RLS + tenant_isolation）：
文档取回/校验/缓存、`tools/call`、`resources/read`、`ui/message`、`ui/open-link`、
`ui/update-model-context`、权限决定与协议违规都写标识/动作/结果/≤512 B 摘要，
不含消息正文、凭据、token 或完整 URL query。

## 限额与配置

| 项 | 默认 | 说明 |
|---|---|---|
| 单文档 | 2 MiB | `mcp.AppResourceMaxBytes`；注册表再校验一次 |
| 文档缓存 | 64 MiB | 按字节 LRU；当前版本受保护，旧版本可回收 |
| `_meta` | 64 KiB | 超界整块忽略并记一条诊断 |
| 单 app 诊断 | 32 条 | 超出折叠为汇总行 |

环境变量：

| 变量 | 作用 |
|---|---|
| `KEEL_MCP_APP_ORIGIN` | 可选绝对 http(s) origin（如 `https://mcp-app.example.com`）。设置且合法时 `/v1/capabilities` 的 `mcp.apps.sandboxOrigin` 为 true。**它只作为桌面/移动 WebView 的文档基址，不启用 spec 的双 iframe 沙箱代理页；web 始终用页面自身 origin。** 非法值只告警并保持 false。生产域名不写进代码。 |
| `KEEL_MCP_PROTOCOL_ERA` | `auto`（默认）/`legacy`/`modern`；控制 §5.5 的两代线协议。非法值 fail-closed 到 legacy。 |
| `KEEL_MCP_SAMPLING` | `off`/`session`（默认）/`on`；MRTR `sampling/createMessage` 的开关。部署没有可用模型时自动降为 off。 |

其余限额（2 MiB 单文档、64 MiB 缓存、64 KiB `_meta`、32 条/app 诊断）当前是包内默认值，
不是环境变量；`KEEL_MCP_MRTR_MAX_ROUNDS` 只出现在设计稿，代码固定为 5 轮。
