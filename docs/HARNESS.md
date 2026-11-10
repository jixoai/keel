# HARNESS — `keel serve` 产品层

`keel` 是内核（`docs/KERNEL.md`）之上的**第一个验证者**：一个 Harness 服务 + 一套产品 API +
扩展面（插件/触发器/subagent/hooks/审批），供 AI UI（`web/` 的 NSH 客户端、TUI）对接。
内核不知道它存在；依赖方向只有 `cmd/keel → agentapi/agentext → kernel`。

> 命名：产品层对外统一 **Harness**（原 Agent）；协议名 `AgentAPI` / `AgentBackend` 是契约，
> 予以保留。术语与别名说明见官网术语表（`site/docs/glossary.md`）。

## 1. 启动

```bash
# 本地（口令认证 + 同源托管 web/dist）
KEEL_SERVE_TENANT=<tenant-uuid> keel serve \
  --dsn 'postgres://…' --addr 127.0.0.1:13750 \
  --password <passphrase> [--model … --base-url … --api-key …]

keel tui                 # NSH TUI（同一份 App.tsx，见 docs/WEBUI.md）
keel doctor              # 只读诊断：配置/DB/schema/web 构建/TUI 产物
keel version
```

- 认证是**网络中间件**（`cmd/keel/auth.go`）：HMAC 签名 HttpOnly cookie，Bearer 兼容 CLI/TUI；
  服务端注入租户、剥离客户端 `X-Tenant-ID`。`--password ""` 关闭认证（仅回环开发）。
- 静态 UI 同源托管 `web/dist`（`nsh package --target web` 的产物；未构建时 JSON 404 带构建指引）。
- `--password ""` 关闭认证时仍注入配置的 tenant（自带 UI 可用），且只回答回环 Host（阻断 DNS rebinding）。
- `keel serve` 是**单租户**产品进程（`KEEL_SERVE_TENANT`）：扩展面读接口（plugins/triggers）返回该进程的
  全局装配状态，多租户复用需另立 change（按租户过滤 + 装配隔离）。
- 装配面是 `cmd/runtime` 的子集（kernel/worker/LLM/MCP/插件/触发器）；外部 AgentBackend、telemetry adapter、
  MCP OAuth、技能/压缩等仍只在 `cmd/runtime` 装配。
- 端口一律 13610–13799（`--addr` 默认 `127.0.0.1:13750`）。

## 2. API 面

### 核心 shell（`internal/httpapi`，内核直连）

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| POST | `/v1/tenants` | 建租户（admin key） |
| POST | `/v1/sessions` | 建会话 |
| POST | `/v1/sessions/{id}/input` | 提交输入；`{text, requestId?, delivery?: queue\|guide}` |
| GET | `/v1/sessions/{id}/events?after=&limit=` | 已提交事件（快照/续传） |
| GET | `/v1/sessions/{id}/events/stream?after=&partials=1` | SSE；`id:` = 事件 seq，partial 无 id |
| GET | `/v1/sessions/{id}/run` | run 行（status/attempts/cursorSeq/lastError） |
| POST | `/v1/runtime/drain`、`GET /v1/runtime/status` | 排空/交接 |

### 产品 API（`internal/agentapi`）

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| GET | `/v1/capabilities` | 能力集 + 闭集 hooks + 分页上限（前端能力门的数据源） |
| GET | `/v1/sessions?limit=&after=` | 会话列表（keyset 分页，`runStatus` overlay） |
| POST | `/v1/sessions/{id}/cancel` | 停止当前 attempt（幂等；202=cancelling，501=跨进程无 seam） |
| GET/POST | `/v1/sessions/{id}/decisions` | 审批开/列；`POST .../{decisionId}` 作答（幂等，409 冲突） |
| GET | `/v1/plugins` | 插件/generation/hooks/工具 |
| GET | `/v1/triggers`、POST `/v1/triggers/{id}/fire` | 触发器审计与手动触发（幂等键） |
| GET | `/v1/subagents`、`/v1/subagents/{id}` | 子代理列表/详情 |
| POST | `/v1/sessions/{id}/subagents` | 派生子代理（`wait:true` 同步等待；断线不再取消子代理） |

错误映射：401 未认证 / 404 不存在 / 409 冲突 / 429 限流 / 501 seam 未接线 / 502 子代理或后端失败 / 503 排空中。

取消的语义：kernel 的 run 行按失败终态结算（attempt 不自动重跑），产品的 `runStatus` overlay 记 `cancelled`；重复 cancel 在同一进程生命周期内幂等（重启后按 durable 终态回答）。

## 3. 扩展面（`internal/agentext`）

| 能力 | 契约 |
| --- | --- |
| 插件 | **进程内声明式契约**（`agentext.Plugin`，`internal/agentext/plugin.go:67`）：`ID/Version/Provides/Requires/Tools/Hooks/Triggers/Effects/Start/Stop`；依赖拓扑激活（缺依赖 `PENDING` 可见）、反序卸载（hook 订阅/触发器/effects 精确撤销，无残留）、`generation+1`、工具重名 fail-closed（第二个插件 `FAILED`）；装配结果经唯一 sink 发布给模型工具集（`cmd/keel/serve.go` 用 `rt.RefreshProductTools`）。web 设置 › 插件（`web/src/screens/settings.tsx:1148`）渲染该装配面。**规划中**（`keel-plugin-core`，勿当作已实现）：`plugin.yaml` 包与安装/启用/禁用 API、三种 kind（in-process/sidecar/webview）、运维权限与审计、webview UI 托管与 `keel.plugin.*` 桥、创造模式 |
| 触发器 | `cron` / `interval` / `webhook`；幂等键（确定性 session + kernel requestId 双保险）；有界重试；审计在 `/v1/triggers` |
| Sub-agent | `spawn_subagent` 工具：独立子会话 + 结构化结果 `{status,summary,artifacts,usage}` + 预算（turns/tokens/timeout）；父会话经 `parentSessionId` 可查 |
| Hooks | 闭集：`subagent.started\|finished`、`trigger.before\|after`、`turn.started\|finished`、`decision.opened\|answered`；有界异步队列，不阻塞账本，失败可见 |
| 审批 | decision 原语：open/wait/timeout/cancel + 答案 journal；durable 在 tasks 行 |
| Partials | token 级旁路：进程内 hub → SSE `event: partial`（无 `id:`，不进事件表，不重放） |

## 4. 与内核的接缝

- `RunControl`（`agentext`）→ `agent.RunController` → worker：取消注册在**执行 attempt 的进程**；
  跨进程只能返回 501，不伪造成功。
- `TurnStarter`（`agentext`）→ kernel edge commit：触发器/subagent/手动触发共用一条写路径。
- `WrapStream`/`WrapTelemetry`（`servicehost`）：partial 生产者与 turn 遥测，不碰内核。
- 摘要/取消语义见 `docs/KERNEL.md` §2「运行摘要」。

## 5. 验收

`internal/agentapi`、`internal/agentext`、`cmd/keel` 均有 PG 集成测试；扩展面 E2E 覆盖
插件注册→触发→hook→subagent→取消在线 attempt。全量门禁见 `scripts/acceptance-all.sh`。
对外文档的术语纪律由 `scripts/naming-scan.sh` 扫描。
