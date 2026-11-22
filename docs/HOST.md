# 宿主层（Host）设计

> PI 的三层里，**内核**（loop/durable）和**协议**（MCP/telemetry）已分别复刻；
> 本文件定义把它们装配成「能跑一个真实 Harness 会话」的**宿主层**。只做装配，不重写内核语义。

## 1. 职责

宿主层把「租户配置 + 事件日志 + 工具 + 技能 + 钩子 + 协议」组装成一次 run：

```
Tenant config
  ├─ system prompt sections (base + skills index + tenant)
  ├─ tools      (builtin + skill loader + MCP namespaced)
  ├─ hooks      (PI extension event names)
  ├─ compaction (PI policy + checkpoint callback)
  └─ telemetry  (PI TelemetryContext/Span)
        │
        ▼
  loop.RunConfig  →  agent.Runner  →  events (PG)  →  SSE
```

**内核不感知** skills/MCP/租户；宿主只做装配与配置解析。

## 2. 配置 schema（v1）

```yaml
agent:
  name: "tenant-agent"
  provider: "openai"        # 空 = fake provider
  model: "gpt-5"
  systemPrompt: "You are ..."
skills:
  enabled: true
  roots: ["~/.agents/skills", ".agents/skills"]
mcp:
  servers:
    filesystem:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
      timeout: 60
hooks:
  commands:
    - event: tool_execution_start
      command: ["./hooks/deny-rm.sh"]
      timeoutMs: 5000
compaction:
  enabled: true
  threshold: 0.8
  keepRecentTokens: 20000
telemetry:
  adapter: "memory"         # memory | prometheus | otlp（后两者后续）
tools:
  # 省略 = 不过滤（推荐）；显式 [] = 不保留任何工具；尾部 * 为前缀通配（如 "keel_*"）
  allow: ["bash", "read", "write", "edit", "mcp", "skills", "tasks", "agent", "approve", "notify", "creation"]
quota:
  runConcurrency: 8       # per-tenant concurrent runs in this runtime (0 = unlimited)
  eventsPerMinute: 600    # per-tenant admitted-event rate (0 = unlimited)
```

配置来源：运行时可继续从环境变量读取（见 §4）；租户配置文档持久化在
`tenants.config jsonb`，由管理员 API（见 §4.2）写入，下一轮 run 生效。
密钥只存引用：`agent.apiKeyRef` 与 MCP server 的 `env` / `headers` 值只接受
`${NAME}` / `env:NAME` / `secret:NAME` 三种形态；明文（包括 `Bearer <token>`）一律拒绝，
不落 schema。

## 3. 装配顺序（与 PI 对齐）

1. **Skills**：`skills.Discover(roots)` → `skills.FormatForPrompt(skills, "read")`；
   作为 system prompt 的 `<skills>` section；`disable-model-invocation` 不进入索引。
2. **Tools**：恒定 11 个内建聚合工具（`bash`/`read`/`write`/`edit`/`mcp`/`skills`/`tasks`/
   `agent`/`approve`/`notify`/`creation`）∪ 已审批插件的 `provides.tools`。MCP 只经
   `mcp {action:"list"|"describe"|"call"}` 暴露，不生成 `mcp__server__tool`；skills 正文
   经 `skills {action:"view"}` 按需加载。工具集按 `tools.allow` 过滤（host 装配统一执行：
   对内置与插件/外部工具同时生效；省略不过滤，显式 `[]` 不保留任何工具，尾部 `*` 为前缀
   通配，如 `keel_*`）；skill 的 `allowed-tools` 与实际集合取交集。
3. **Hooks**：把 `internal/hooks` 的事件接到 loop 的 `PrepareRequest` / `OnStop` /
   `ShouldStopAfterTurn` / tool executor 的 before/after；命令钩子按配置加载。
4. **Compaction**：`loop.LoopConfig.Compaction` 使用 PI `DEFAULT_COMPACTION_POLICY`
   （threshold/keepRecentTokens/backgroundTokens）。`agent.Runner` 在有序事件消费者里
   提交 `compaction` 事件（摘要/保留段/token 数），随后写 `checkpoints`
   （`upto_seq`=事件 seq、`kept_from_seq`=保留段首个事件 seq；投影，不覆盖事件）。
   保存失败只记日志、不回滚事件；新 run 以最新 checkpoint 的摘要 + `seq >= kept_from_seq`
   重建，未锚定或无 checkpoint 时回退全量事件。
5. **Telemetry**：run = span，turn/tool = 子 span；`run.finished` 作为业务汇总事件追加。
6. **System prompt**：按 PI 的 section 形态拼装（`<preamble>`、`<skills>`、`<tenant>`），
   不再用单一字符串硬编码。

## 4. 与现有运行时的接线

- `internal/agent.Runner` 通过 `Assembly` 接缝（`SystemPrompt()`, `Tools()`）使用宿主；
  默认实现是 `internal/host.Host`，`Runner` 仍只负责事件↔消息、run 状态、租约。
- `/v1/sessions/{id}/input` 提交后入队 run；worker 领取后由 Runner 用装配好的 prompt/tools
  调用 loop。`Runner.Telemetry` 为 run 级 span + `keel.agent.runs` / `keel.agent.run.duration`。
- MCP server 生命周期由 `mcp.Manager` 管理（并发连接/列工具/部分失败报告/关闭）；
  run 结束不关闭共享连接；`cmd/runtime` 在退出时 `Close()`。
- MCP Apps 宿主（目录/文档托管/CSP/降级/租户语义）见 [`MCP-APPS.md`](MCP-APPS.md)。
- 配置热更新：下一轮 run 生效；运行中的 run 用快照。

### 当前环境变量（`cmd/runtime`）

| 变量 | 作用 |
|---|---|
| `KEEL_SYSTEM_PROMPT` | preamble（默认一句通用身份说明） |
| `KEEL_SYSTEM_SECTIONS` | 额外 section，JSON 数组 `[{"name":"tenant","content":"..."}]` |
| `KEEL_SKILLS_ROOTS` | 技能根目录，OS path list（如 `~/.agents/skills:/srv/skills`） |
| `KEEL_SKILLS_ROOT` | **仅 `keel serve`**：租户技能库根（默认 `$KEEL_HOME/skills`）。`<root>/<tenant>/active` 自动加入发现；安装/更新/固定经 `/v1/settings/skills`，版本不可变、按内容哈希去重 |
| `KEEL_SKILLS_SOURCE_ROOT` | **仅 `keel serve`**：技能订阅源根（默认 `<KEEL_SKILLS_ROOT>/sources`）。订阅源 URL 只能解析到这棵根内；`/etc`、`..`、指向根外的 symlink 一律拒绝。模型工具面只能建 untrusted 源 |
| `KEEL_FILE_READ_TOOL` | skills 索引里的读文件工具名：`read`（默认）/`bash`/`indirect` |
| `KEEL_MCP_CONFIG` | 全局 `mcp.json` 路径（空 = 不启用 MCP） |
| `KEEL_MCP_PROJECT` | 项目 `mcp.json` 路径（仅可信项目） |
| `KEEL_MCP_APP_ORIGIN` | **仅 `keel serve`**：可选原生 WebView 文档基址（`/v1/capabilities` 的 `mcp.apps.sandboxOrigin`）；非法值保持关闭。`cmd/runtime` 没有 app 面，不读它 |
| `KEEL_TELEMETRY` | `memory` 时启用内存 telemetry recorder |
| `KEEL_ADMIN_KEY` | 管理员 Bearer key（租户 provisioning、租户配置/用量 API、MCP OAuth sign-in）；空 = 管理员 API 关闭（503） |
| `KEEL_MODEL` / `KEEL_BASE_URL` / `KEEL_PROTOCOL` / `KEEL_PROVIDER` / `KEEL_API_KEY` | provider 选择（空 = fake provider） |
| `KEEL_LOADOUT_ID` / `KEEL_LOADOUT_REVISION` | 默认 loadout 快照 id 与 revision（handoff identity 与 run 元数据共用；默认 `default` / 1） |
| `KEEL_QUOTA_RUNS` / `KEEL_QUOTA_EVENTS_PER_MINUTE` | 默认租户文档的 per-tenant 配额（并发 run / 每分钟事件）；0 = 不限，租户存储文档可覆盖 |

### 管理员 API（租户 provisioning / 配置 / 用量）

管理员接口要求 `Authorization: Bearer <KEEL_ADMIN_KEY>`（scheme 大小写不敏感，密钥比较
常量时间）；未配置 `KEEL_ADMIN_KEY` 时返回 503，不存在匿名或半开放路径。

| 方法/路径 | 作用 |
|---|---|
| `POST /v1/tenants` | 创建租户（`X-Tenant-ID` 指定 id，body `{"name":"..."}`）；未鉴权 401，未配置 key 503 |
| `PUT /v1/tenants/{id}/config` | 替换租户配置。body 即 §2 的 JSON 文档（非信封）；未知字段/非法值 400，body 超限 413，租户不存在 404 |
| `GET /v1/tenants/{id}/config` | 返回 `{tenantId, revision, config}` |
| `GET /v1/tenants/{id}/usage` | 返回按 session/provider/model 分组的 token 计数与总计（含 `unaccountedEntries`）；只有计数，无金额 |

租户 provisioning 也是管理员操作：`X-Tenant-ID` 只是写入的行 id，不是身份凭证。

`config_revision` 每次写入原子 +1，是变更标识。`usage_ledger` 以 Runner 在发请求前生成的
`request_id` 为幂等键：同一 request_id 重放只记一次，一个事件 seq 派生多个请求（如
compaction summary）也不会丢账；provider 已计费但进程崩溃导致 usage 丢失的请求以
`unaccounted` 行保留，恢复后在同一行升级为 `settled`。

## 5. MCP 接入（apps / events / era）

> 本节描述 **`keel serve`（Harness 产品宿主）**。`cmd/runtime` 只装配 MCP 工具与通知，
> 没有 app 目录、MRTR 执行器或 `/v1/mcp/*` 路由；那些内容不适用于它。
> 设计基线：`openspec/changes/archive/2026-11-20-feat-mcp-apps-events/` 的 `design.md` 与两份 spec；
> G0/G4 逐条对表见
> `openspec/changes/archive/2026-11-20-feat-mcp-apps-events/evidence/p6/docs/parity.md`。

Keel 在 MCP 里是 **host/client**，不实现 server 侧。MCP 面在宿主侧投影为：工具、提示段、
app 目录与 events；资源面当前只投影已发布 app 的文档（不是完整的 `resources/list` 目录）。
模型面仍只有恒定 11 个内建工具；MCP 工具只经聚合 `mcp` 工具（`list`/`describe`/`call`）
暴露，**不生成** `mcp__<server>__<tool>`。这一恒定工具面是本 change 的前置契约
（`docs/PI-PARITY.md` §4.2）。

### 5.1 装配与发布

```
mcp.Manager（进程级连接）
  ├─ tools / tools/list_changed ──▶ 聚合 mcp 工具（模型面唯一入口）
  ├─ PromptSection() ────────────▶ mcp_servers 段
  ├─ AppsPromptSection() ────────▶ mcp_apps 段（只含已发布 app）
  ├─ ModelTools()/AppTools() ────▶ visibility 过滤与 app 绑定目录
  └─ mcphost.Registry ───────────▶ app 目录 / 文档缓存 / /v1/mcp/apps

mcphost.Events ──▶ 通知分类、listen 监督、去抖 republish、generation
mcphost.MRTR   ──▶ input_required / legacy 服务端请求执行器
```

- `mcpService.Start()` 先做一次 registry 刷新（取回并校验每个 app 文档），再启动 events
  监督；`cmd/keel/dynamic_surface.go` 把 MCP 半面与插件半面**合并后一次性**
  `PublishMCPSurface`，失败保留上一版面，绝不发布空面/半面。
- 发布语义是**全量 republish**：工具、资源、app、提示段在同一次 generation 里更新；
  进行中的 attempt 使用它开始时冻结的工具集，热更新从**下一次 attempt** 生效。
- `generation` 单调递增，`/v1/mcp/servers` 与 `/v1/capabilities.mcpApps.generation` 都携带。

### 5.2 配置：注册 MCP 服务器

| 项 | 作用 |
|---|---|
| `--mcp` / `KEEL_MCP_CONFIG` | 全局 `mcp.json` 路径；空 = 不启用 MCP |
| `--mcp-project` / `KEEL_MCP_PROJECT` | 项目 `mcp.json` 路径；项目条目可覆盖全局同名条目 |
| `KEEL_MCP_PROTOCOL_ERA` | `auto`（默认）/`legacy`/`modern`；非法值 fail-closed 到 `legacy` |
| `KEEL_MCP_SAMPLING` | `off`/`session`（默认）/`on`；见 §5.5 |
| `KEEL_MCP_APP_ORIGIN` | 可选绝对 http(s) origin；只作为**桌面/移动 WebView 的文档基址**，见 §5.7 |
| `KEEL_SERVE_TENANT` | serve 进程的租户；`TenantID=uuid.Nil` 表示单租户/进程级目录 |

`mcp.json` 使用跨 MCP 客户端的 `mcpServers` 形状，stdio 与 Streamable HTTP 二选一：

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
      "enabled": true,
      "timeout": 60
    },
    "billing": {
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "${BILLING_TOKEN}" },
      "enabled": true
    }
  }
}
```

- stdio 字段：`command`/`args`/`env`/`cwd`；HTTP 字段：`url`/`headers`/`oauth`；
  两者共有 `enabled`/`description`/`timeout`。`type:"sse"` 明确拒绝（只支持 Streamable HTTP）。
- 密钥只接受 `${NAME}` / `env:NAME` / `secret:NAME` 引用；明文值会被配置校验拒绝。
- 项目条目与全局同名时替换；项目-only 条目只能设置 `enabled`/`exposure`/`toolExposure`。
- **租户覆盖（当前不支持）**：MCP 连接源是进程级 `mcp.json`；`keel serve` 不会在运行时
  按租户重读 `PUT /v1/tenants/{id}/config` 的 MCP 段。修改 `mcp.json` 后重启进程生效；
  OAuth 登录成功会走既有 `Reconnect`，不需要重启。多租户部署必须**每租户一个进程**
  （或保持 `TenantID=uuid.Nil` 的单租户进程），不能在一个 serve 进程里假装按租户隔离。
- `TenantID=uuid.Nil` 时目录是进程级：任何已鉴权租户都能读到同一份目录；registry/audit
  的 NULL-tenant 行也会被所有租户看到。多租户部署请显式设置租户。

### 5.3 HTTP 面

所有 `/v1` 路由都在鉴权中间件之后：人类账号 cookie 与机器 Bot key 都由中间件解析出身份并
注入 `X-Tenant-ID`；客户端自带的 `X-Tenant-ID` 会被删除，不手工发送（`cmd/runtime` 是例外，
它仍以该头作为运行时租户接口）。租户判定先于任何目录/文档字节读取。
未接线 registry 的路由明确回 501，不用 404 假装「没有这个能力」。

| 方法与路径 | 语义 | 主要失败码 |
|---|---|---|
| `GET /v1/mcp/servers` | 机群投影：`generation`、连接状态、`protocolVersion`/`era`、`capabilities`（8 KiB 有界）、工具/app 计数、诊断、`catalogueError` | 401、501 |
| `GET /v1/mcp/apps` | app 目录（`AppSpec` 投影）+ `theme` 调色板 + `limits` + 可选 `sandboxOrigin` | 401、501 |
| `GET /v1/mcp/apps/{appId}/view` | app 文档本体；`?v=<hash>` 是内容身份 | 401、404、501 |
| `POST /v1/mcp/apps/{appId}/tools/call` | 宿主代理 `tools/call` | 400、403、409、413、429、502 |
| `POST /v1/mcp/apps/{appId}/resources/read` | 宿主代理 `resources/read`（仅该服务器） | 400、413、429、502 |
| `POST /v1/mcp/apps/{appId}/ui/message` | `ui/message` → decision → 既有输入写路径 | 400、404、409、413、429、501、503 |
| `POST /v1/mcp/apps/{appId}/ui/open-link` | `ui/open-link` → 确认 → 返回目标 URL | 400、404、413、429、503 |
| `POST /v1/mcp/apps/{appId}/context` | `ui/update-model-context` 覆盖式落库 | 400、404、413、429、501 |
| `GET /v1/mcp/apps/audit` | 审计读（`app`/`action`/`after`/`limit`，limit 1–200） | 400、503 |
| `POST /v1/mcp/apps/audit` | 宿主侧动作审计：只接受 `permission.granted`、`permission.denied`、`protocol.violation` | 400、404、413、429、503 |

`GET /v1/mcp/apps/{appId}/view` 的响应头：`Content-Type: text/html; charset=utf-8`、
`X-MCP-App-Mime: text/html;profile=mcp-app`、`X-MCP-App-Hash`、
`Content-Security-Policy`（含 `frame-ancestors 'self'`）、`X-Content-Type-Options: nosniff`、
`Referrer-Policy: no-referrer`、`Vary: X-Tenant-ID`；带匹配 hash 时
`Cache-Control: public, max-age=31536000, immutable`，裸 URL 为 `no-store`。

能力位（`GET /v1/capabilities` 的 `features`；`mcpApps` 块另带
`sandboxOrigin`/`generation`/`apps`）：

| 能力位 | 为 true 的条件 |
|---|---|
| `mcp.apps` | 宿主 registry 已接线（`keel serve` 恒为 true，即使没有服务器） |
| `mcp.apps.sandboxOrigin` | `KEEL_MCP_APP_ORIGIN` 是合法 http(s) origin |
| `mcp.apps.context` | `mcp_app_contexts` 存储 seam 已接线 |
| `mcp.apps.tools` / `mcp.apps.resources` | app 代理 runtime 已接线 |
| `mcp.apps.message` | 输入提交 seam 与 decision 原语都已接线 |
| `mcp.apps.links` | decision 原语已接线（前端自己用 noopener 打开） |
| `mcp.events` | events dispatcher 已接线（`keel serve` 为 true；`cmd/runtime` 不暴露该 API） |

### 5.4 Apps：声明、渲染与降级

**服务端声明。** 工具用 `_meta.ui.resourceUri` 指向 `ui://` 资源；旧式平铺键
`_meta["ui/resourceUri"]` 仍可读（结构化键优先）。`_meta.ui.visibility` 缺省为
`["model","app"]`；不含 `model` 的工具不进入模型面，不含 `app` 的工具不能被 app 调用。
资源用 `_meta.ui` 声明 `csp{connectDomains,resourceDomains,frameDomains,baseUriDomains}`、
`permissions`（camera/microphone/geolocation/clipboardWrite）、`domain` 与 `prefersBorder`。
文档必须是 `text/html;profile=mcp-app`，单文档 ≤ 2 MiB，单条 `_meta` ≤ 64 KiB。

**身份与目录。** `appId = "mcpapp_" + hex(sha256(server + "\x00" + resourceUri))[:16]`；
文档身份是内容 sha256，`documentUrl` 由宿主构造为
`/v1/mcp/apps/{appId}/view?v={hash}`。同一 `ui://` 被多个工具引用时目录只出现一次，
`tools` 列出全部引用者。目录是绑定的唯一机器可读来源，客户端不自行重算。

**宿主行为。** 工具结果卡的 `details.app` 命中且能力位允许时挂载 View；内联默认弹性高度
（80–480 px），全屏用 kit 的 Dialog。View 必须先 `ui/initialize` 再
`ui/notifications/initialized`；宿主在 `initialized` 之前不发任何 `ui/notifications/*`。
握手完成后恰好发一次 `tool-input`，结果到达后恰好发一次 `tool-result`；取消时发
`tool-cancelled`；`size-changed` 只在内联模式生效；主题变化发部分
`host-context-changed`。卸载前发 `ui/resource-teardown`，最多等 1 s（当前 web 卸载路径
发送后立即销毁，见 §5.7 缺口）。

**出站动作。** 全部经宿主 HTTP 面代理，View 不直连 MCP 服务器或产品 API：

- `tools/call`：服务器由目录派生；只允许该服务器、`visibility` 含 `"app"` 的工具；
  破坏性工具（`destructiveHint` 或变更族词名）逐次确认；拒绝不产生上游调用。
- `resources/read`：只允许该服务器；URI 只允许 `ui://`/`http(s)://`，`file:`/`data:` 拒绝。
- `ui/message`：`role` 只能是 `user`，`delivery` 只能是 `queue`；decision
  send/edit/discard；只有 send 落账本，edit 只预填 composer。
- `ui/open-link`：只允许 http(s)，拒绝凭据/非 ASCII 主机；确认后前端以
  `noopener,noreferrer` 打开。
- `ui/update-model-context`：覆盖式写入 `mcp_app_contexts`（每文档 ≤ 64 KiB），
  下一次 attempt 以 `<mcp_app_context>` 并入最后一条 user 消息；**不写事件账本**。

**降级。** 没有 `_meta.ui` 的服务器 = 纯文本，零变化；客户端无 `mcp.apps` 或 TUI =
标题 + 文档链接 + 文本；取回/类型/超限/缓存失败 = 不进目录 + 诊断 + 文本兜底；重取失败
保留旧版本；握手超时（5 s）、帧超限、超频或协议违规 = 销毁实例 + 文本 + 诊断/审计。
任何一格都不出现空白区、假 UI 或「假成功」。

**限额。** 单文档 2 MiB；文档缓存 64 MiB（字节 LRU）；单 app 诊断 32 条；目录诊断总量
200 条；`structuredContent` 落 details 上限 256 KiB（超限换成
`{"_meta":{"truncated":true,...}}` 标记）；桥单帧/请求体 64 KiB、每 app 200 请求/秒、
单 app 并发 4；确认等待默认 120 s（上限 10 min）；单会话未答 app 确认上限 16；
导航重载预算每实例 10 次。

### 5.5 Era 与 MRTR

`LatestProtocolVersion` 仍是 PI 基线的 `2025-11-25`；同一客户端同时支持两代：

| 面 | legacy（≤2025-11-25，PI 基线） | modern（2026-07-28） |
|---|---|---|
| 握手 | `initialize` + `notifications/initialized` | 无握手；`server/discover` |
| session | `Mcp-Session-Id` + DELETE | 无 session |
| 服务端→客户端流 | HTTP GET 长流 | 无 GET；`subscriptions/listen` 的 POST 响应流 |
| 恢复 | `Last-Event-ID` 续传（对端给过 id 时） | 不重投递；重订阅 + 全量重算 |
| 服务端请求 | 主动 `sampling/createMessage` 等 | `InputRequiredResult` / MRTR |
| 结果类型 | 无 | `resultType`；缺失按 `"complete"`，未知值协议错误 |
| 头 | `MCP-Protocol-Version` | 另加 `Mcp-Method`/`Mcp-Name`（非 ASCII base64 哨兵） |
| 缓存提示 | 无 | `ttlMs`/`cacheScope` 解析并使用 |

`auto`（默认）先按现代形态探测 `server/discover`；400 且 body 是可识别的现代 JSON-RPC
错误 → modern（按 advertised 版本重试），body 空/不可识别 → 回落 legacy。现代下不发
`Mcp-Session-Id`/`Last-Event-ID`、不 DELETE session；每个请求 body 带
`io.modelcontextprotocol/protocolVersion` 与 `clientCapabilities`。`/v1/mcp/servers`
逐服务器报 `era`、`protocolVersion` 与有界 `capabilities`，运维不必另开接口。

**MRTR。** `CallTool` 收到 `resultType:"input_required"` 时：`inputRequests` 逐条执行；
`elicitation/create` 走 decision（URL 模式不自动打开）；`sampling/createMessage` 走当前
会话模型服务，`KEEL_MCP_SAMPLING` 默认 `session`（只在有会话工具调用时允许），`on` 允许
任何执行器上下文（无 per-server 窗口预算，显式 opt-in 成本风险）；`roots/list` 与未知
方法一律 `-32601` 并审计。`requestState` 原样回显、重试使用新 JSON-RPC id；同一调用
MRTR 往返上限 **5 轮**（`CallOptions.MRTRMaxRounds`，无环境变量覆盖）；sampling 每次调用
最多 3 次、`maxTokens` 上限 4096（实际下发到 provider）。legacy 的服务端主动请求在
**唯一在途工具调用**内复用同一执行器；无调用或并发多个调用时 fail-closed，不猜会话。

### 5.6 Events

| 通知 | 宿主落点 |
|---|---|
| `notifications/progress` | `internal/mcp` 按进度 token 关联到 `(tenant, session, toolCallId)` → `PartialHub` 的瞬态 `mcp.progress`；不落账本 |
| `notifications/message` | 结构化日志（level 闭集 debug/info/warning/error，字段有界）；不进时间线/账本 |
| `notifications/tools/list_changed` | 去抖 250 ms → 全量 republish → generation++ → hook `mcp.surface.changed` |
| `notifications/prompts/list_changed` | 触发同一目录重算（当前 prompts 内容无独立缓存） |
| `notifications/resources/list_changed` | 触发资源/app 目录重算 |
| `notifications/resources/updated{uri}` | 标记该 app 文档 stale → 重取/换 hash；失败保留旧版本 |
| `notifications/cancelled` | 中止对应在途请求；最终以该调用的错误 `tool.result` 落账本 |
| `notifications/subscriptions/acknowledged` | listen 流消费；ack 未接受的类别记诊断 |

- **订阅主体是服务器**：每个 modern 服务器一条 `subscriptions/listen`，不是每会话/每 UI；
  filter 默认 `toolsListChanged`/`promptsListChanged`/`resourcesListChanged` 全开，
  `resourceSubscriptions` 只列已发布的 app URI。首消息必须是 ack；按
  `io.modelcontextprotocol/subscriptionId` 去复用。
- **断线**：modern 直接重发 listen（不发 `Last-Event-ID`、不期待重投递）；legacy 保持
  GET 流退避重连。订阅重建成功后**全量重算一次目录**（断线期间无回放）。
- **失败保面**：`tools/list` 重算失败时 `mcp.Manager` 不替换旧工具集；registry 保留旧
  app 版本；`/v1/mcp/servers` 的 `catalogueError` 可见并写审计。发布永不发空面/半面。
- **账本零新增 kind**：目录、日志、进度、订阅 ack 都不是会话事实，不新增 kernel kind；
  唯一可回放结果是既有 `tool.result` 与被转成 decision 的问句。
- **与产品 SSE 隔离**：MCP 订阅实现不 import `internal/httpapi` 的 handler/hub，恢复语义
  各自独立；MCP 流作用域是服务器，产品 SSE 作用域是会话/账本。

### 5.7 安全

**CSP。** `/view` 的 CSP 全部由宿主构造，服务器只贡献域列表；`default-src 'none'`，
`script-src`/`style-src` 为 `'self' 'unsafe-inline'` + `resourceDomains`，
`img-src`/`media-src` 为 `'self' data:` + `resourceDomains`，`font-src` 为 `'self'` +
`resourceDomains`，`connect-src` 为 `'self'` + `connectDomains`，`frame-src` 仅
`frameDomains`（缺省 `'none'`），`object-src 'none'`，`base-uri` 仅 `baseUriDomains`
（缺省 `'self'`），`frame-ancestors 'self'`。另加 `sandbox allow-scripts`：文档即使被
直接打开也保持 opaque origin。只收紧不放宽；缺 `csp` 用严格缺省。

- 域只接受 http(s) origin；通配至少两级标签（`https://*.example.com`），拒绝
  `*.com`/`*.localhost`/IP/全数字主机；默认拒绝 loopback/私网/链路本地；单指令 ≤16 个域、
  单域 ≤256 字节。PSL 判定未实现（`*.co.uk` 这类仍会被当作子域授予），登记为已知残留。
- web iframe 由 NSH 运行时渲染：同源文档**不带** `allow-same-origin`（opaque origin），
  但 0.4 运行时固定附加 `allow-forms/popups/popups-to-escape-sandbox/modals/downloads`；
  文档 CSP 的 `sandbox allow-scripts` 与属性取交集，有效权限仍被收紧。字面属性与 spec
  的 `sandbox="allow-scripts"` 有差异，登记在
  `evidence/p2/frontend/gates.md` §偏离 1。
- `KEEL_MCP_APP_ORIGIN` 不是双 iframe 沙箱：配置后它只作为桌面/移动 WebView 的绝对
  文档基址；web 始终用页面自身 origin。**spec 的 sandbox 代理页 + 内层 View 双 iframe
  未实现**（tasks 已知暂缓 c）。能力位 `mcp.apps.sandboxOrigin` 只表示「配置了一个
  独立 origin 供原生宿主使用」，不表示双 iframe 已启用。
- **permissions 当前不授予**：`_meta.ui.permissions` 会解析并随目录发布，但
  `hostCapabilities` 不声明 `sandbox.permissions`、iframe 没有 `allow` 属性；camera/
  microphone/geolocation/clipboardWrite 的「首次请求 + 用户确认 + 拒绝记忆」依赖 NSH
  per-permission allow 面（tasks P3.6 上游依赖），当前诚实降级为不授予，不假装成功。
- 桥：未知方法 `-32601`；`ui/download-file` `-32601`；`ui/notifications/request-teardown`
  不实现（通知无 id，按未知通知 → 协议违规销毁）；`tool-input-partial`/`pip` 不实现。
- 审计：`mcp_app_audit`（FORCE RLS + tenant_isolation）记录文档取回/校验/缓存失败、
  `tools/call`（ok/error/refused）、`resources/read`、`ui/message`、`ui/open-link`、
  `ui/update-model-context`、权限授予/拒绝、协议违规、bridge.size/rate/concurrency。
  只存标识、动作、结果与 ≤512 B 摘要，**不存消息正文、凭据、token 或完整 URL query**。
- 凭据只在宿主进程；`tools/call` 只回标准 `CallToolResult`，不透传服务器响应头。

**错误码。** JSON-RPC：`-32600` 无效请求/未知 `resultType`、`-32601` 方法不支持、
`-32602` 参数/角色/投递/URL 非法、`-32603` 内部错误、`-32000` 确认挂起或确认队列溢出、
`-32022` 现代版本不被支持。HTTP：`400` 参数、`401` 租户缺失、`403` 非 app 可见工具、
`404` 未知/过期/跨租户 app 或会话、`409` 破坏性工具被用户拒绝或外部会话、`413` 请求体
超 64 KiB、`429` 速率/并发/确认队列超限、`501` 能力未接线、`502` 上游失败、`503`
decision/audit 未接线。

### 5.8 运维

```bash
# 1) 能力位与目录（Bot-bound key；浏览器换成账号会话 cookie）
curl -sS -H "Authorization: Bearer $KEEL_AGENT_TOKEN" https://<host>/v1/capabilities | jq '.features | with_entries(select(.key|startswith("mcp.")))'
curl -sS -H "Authorization: Bearer $KEEL_AGENT_TOKEN" https://<host>/v1/mcp/servers | jq .
curl -sS -H "Authorization: Bearer $KEEL_AGENT_TOKEN" https://<host>/v1/mcp/apps | jq '.apps[] | {appId,server,tools,hash,documentUrl}'

# 2) 文档头与审计
curl -sSI "https://<host>/v1/mcp/apps/<appId>/view?v=<hash>" | grep -Ei 'content-security|nosniff|referrer|x-mcp-app|vary|cache-control'
curl -sS -H "Authorization: Bearer $KEEL_AGENT_TOKEN" 'https://<host>/v1/mcp/apps/audit?limit=50' | jq .
```

指标（固定基数，`/metrics`）：`mcp_servers{state}`、`mcp_surface_generation`、
`mcp_app_fetch_total{result}`、`mcp_app_bridge_frames_total{method,direction}`、
`mcp_listen_reconnects_total`、`mcp_mrtr_rounds`（直方图）。`mcp_app_bridge_frames_total`
只给四个动作打标签，`context`/`audit.record` 折入 `other`；系列数与会话数无关。

日志关键字：`mcp app audit`（registry 文档/目录）、`mcp catalogue audit`（events/刷新）、
`mcp server log`（服务器 `notifications/message`）、`mcp listen`（ack 缺项/重连诊断）。

| 现象 | 先看 |
|---|---|
| `mcp.apps` false | 部署未接线 registry（本仓只有 `keel serve` 接线；`cmd/runtime` 没有 app 面） |
| app 不在目录 | `/v1/mcp/servers` 的 `connected`/`catalogueError`；`/v1/mcp/apps.diagnostics`；MIME 是否 `text/html;profile=mcp-app`、是否超 2 MiB |
| `/view` 404 | appId 形状、hash 是否过期、租户是否边界错误；未知/跨租户统一 404 是预期 |
| app 内 fetch 被 CSP 阻断 | 服务器没有在 `_meta.ui.csp.connectDomains` 声明该域；宿主不会放宽 |
| View 5 s 后消失 | View 未完成 `ui/initialize` + `initialized`，或发了未知方法/超 64 KiB 帧 |
| `tools/call` 403 | 工具不属于该 app 的服务器，或 `visibility` 不含 `"app"` |
| `tools/call` 202 | 破坏性工具/消息/链接在等用户 decision；单会话未答上限 16 |
| `429` | 单 app 200 请求/秒或并发 4 已满；拒绝审计按秒采样 |
| TUI 只看到文字 | 预期：终端 idiom 不挂 WebView，显示标题 + 文档路径 + 文本结果 |

**当前缺口（与 spec 的差异，不要当成已完成）：**

1. **第二 origin 双 iframe 未实现**：见 §5.7；`KEEL_MCP_APP_ORIGIN` 只服务原生基址。
2. **permissions 未授予**：见 §5.7；P3.6 依赖 NSH allow 面。
3. **teardown 等待不阻塞**：web 卸载路径发送 `resource-teardown` 后立即销毁；1 s 有界
   等待只有桥单测，未在真实卸载路径生效；NSH 0.4 组件卸载模型不允许延迟移除，
   已作为接受的偏离记录（`evidence/p6/fix/notes.md` §3），无数据/安全影响。
4. **SDK 2.x 独有面**：`ui/download-file` 回 `-32601`；`ui/notifications/request-teardown`
   按未知通知销毁；`ui/notifications/tool-input-partial` 不发；`pip` 不支持。

## 6. 验收

- 一个租户配置：1 个 skill + 1 个 MCP filesystem server + 1 个 deny hook + compaction +
  telemetry memory adapter；
- 一次对话：skill 索引出现在 system prompt、MCP 工具可调用、deny hook 阻止危险工具、
  超阈值触发 compaction、run span 有 turn/tool 子 span；
- `go test -race ./internal/host` + 运行时 E2E 全绿。

## 7. 非目标

- 不复刻 coding-agent 的 TUI/CLI/主题/键位/npm 包管理；
- 不复刻 project-trust UI 与交互式 `/mcp` 菜单；
- 不在宿主层实现新的协议或事件名——只装配 PI 已有语义。
