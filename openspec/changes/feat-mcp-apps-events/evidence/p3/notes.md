# P3 后端切片证据（feat-mcp-apps-events）

日期：2026-11-20 · 分支 `task/keel-mcp-p3`（基于 `28e49f5`）· 范围：P3.1–P3.4、P3.7、P3.8，
以及 P3.6 的「破坏性工具逐次确认」后端门（P3.6 整项不勾，前端权限 UI 属前端 lane）。

## 交付面

| 任务 | 落点 | 关键行为 |
|---|---|---|
| P3.1 | `internal/agentapi/mcp_app_actions.go`、`cmd/keel/mcp_app_bridge.go` | server 由 catalogue 派生（`MCPAppsSurface.App` → `AppSpec.Server`）；`MCPAppRuntime.AppTool` 要求工具存在且 `visibility` 含 `"app"`；越权/别服务器/模型专用工具 403 + `tools/call=refused` 审计，上游零调用；破坏性工具（`annotations.destructiveHint` 或变更族词名）每次调用开 decision，拒绝无上游；返回标准 `CallToolResult`，不透传服务器响应头 |
| P3.2 | 同上 | `resources/read` 只打 catalogue 的 server；URI 仅 `ui://`/`http(s)://`（缺 scheme/host 也拒）；结果逐条 ≤2 MiB（text 或解码后 blob），不落盘；拒绝写 `resources/read=refused`（只记 scheme，不记 URI query） |
| P3.3 | 同上 + `httpapi.CommitClientInput` | `role` 仅 `"user"`，否则 JSON-RPC `-32602` 且零账本；`delivery` 仅 `queue`；decision `mcp-app.message` 三选项；send 经既有输入写路径（`user.message`，`requestId=mcp-app:<appId>:<frame>`，重放幂等），edit 只回预填、discard 零账本 |
| P3.4 | 同上 | `ui/open-link` 仅绝对 `http(s)`；decision `mcp-app.link`；确认后回 `{opened:true,url}` 供前端以 `noopener,noreferrer` 打开；拒绝/取消 `opened:false` + 审计 `refused`；不导航宿主、不写账本；审计只记 host |
| P3.7 | `internal/store/schema.sql`、`internal/store/mcp_app_audit.go`、`internal/agentapi/mcp_app_actions.go`、`cmd/keel/mcp_app_bridge.go` | `mcp_app_audit` + `rlsTenantTables`（FORCE RLS/tenant_isolation）；append/list（detail 512B 上限、去控制符）；`GET /v1/mcp/apps/audit` 分页；`POST /v1/mcp/apps/audit` 为宿主动作（`permission.granted/denied`、`protocol.violation`）的租户鉴权内部入口，不能伪造桥动作；registry 的文档取回/校验/缓存失败经同一表 |
| P3.8 | 同上 | HTTP 代理面：64 KiB body（413）、每 app 200 req/s 令牌桶（假时钟可测）、单 app 并发 4（429）；三类超限均写 `bridge.size\|rate\|concurrency=refused`；浏览器桥帧同数值，映射写在 `mcp_app_actions.go` 头注释 |

## Decision 复用

- 没有新增第二套确认机制：全部走 `agentext.DecisionBroker` 的 `Open`/`Wait`，复用现有 approve UI 与 decision 持久化。
- 「edit」分支用既有 `DecisionOption` 表达，不需要扩展 broker 状态；`AllowFreeText=false`，编辑内容由前端 composer 预填。
- 每次确认使用新 decision（requestID 含随机 UUID），因此同一 View frame 重放不会复用上一次的允许；`ui/message` 的账本 `requestId` 仍是幂等键。
- 等待有界（默认 120s，最大 10min）；超时回 202 + open decision id，并审计 refused（`reason=confirmation_timeout`）；客户端断开不写响应、审计 `client_disconnected`。

## 审计边界（无正文）

- 审计行字段：`ts/tenant_id/session_id/server/app_id/action/result/detail`；`detail` 只放工具名、scheme、host、分支、计数与固定 reason 码。
- 明确不写入：消息正文、attachments、凭据/token、完整 URL query、上游错误原文（只留协议 code）。
- 真库测试 `TestMCPAppAuditNoBodyAgainstStore` 用唯一 marker 扫描 `mcp_app_audit` 全表所有自由文本列，断言 0 命中；`TestMCPAppMessageBranches`/`TestMCPAppOpenLink` 在内存 fake 上做同断言。

## 偏离与未决（诚实登记）

1. **P3.6 未整项完成**：破坏性工具逐次确认的后端门已交付；camera/mic/geolocation/clipboardWrite 的 iframe `allow` 授予 UI、偏好记忆与「拒绝后不静默重给」的前端路径属前端 lane。
2. **P3.5 未做**：display mode / size-changed / resource-teardown 的前端状态机不在本切片。
3. **`ui/open-link` 的实际打开动作**：后端只负责 scheme 白名单与确认，返回 URL；`window.open(url,"_blank","noopener,noreferrer")` 由宿主前端执行（前端 lane）。spec 的 MUST 由前后端合起来满足。
4. **确认等待形态**：HTTP 请求在 decision 期间阻塞（有界）。超时返回 202；同一逻辑调用重试会重新确认，但账本 requestId 保持幂等。若前端希望两段式（先拿 decisionId 再执行），后续可在不改 decision 原语的前提下加执行端点。
5. **进程级 registry 的审计行**：单租户部署（registry TenantID=Nil）写 `tenant_id=NULL` 的行；租户读取沿用 plugin_audit 的口径（NULL 行对所有租户可见），多租户部署写各自 tenant。
6. **P3.8 的浏览器帧**：帧在浏览器侧聚合，服务端只对 HTTP 代理面计数；两侧常量相同，代码注释已写明映射。

## 证据文件

| 门禁 | 命令 | 结果 | 文件 |
|---|---|---|---|
| build | `go build ./...` | exit 0 | `go-build.txt` |
| vet | `go vet`（改动包） | exit 0 | `go-vet.txt` |
| race | `go test -race -p 1`（agentapi/httpapi/mcp/mcphost/servicehost/store/cmd/keel） | 7 包全 ok，exit 0 | `go-test-race.txt` |
| 全量 | `KEEL_TEST_DSN=... go test -p 1 ./...` | 55 ok / 0 FAIL，exit 0 | `go-test-all.txt` |
| spec | `pnpm exec openspec validate feat-mcp-apps-events --strict` | valid，exit 0 | `openspec-validate.txt` |
| conformance | `./scripts/conformance.sh` | 1020 pass / 0 fail / 1 skip，exit 0 | `conformance.txt`、`conformance.json` |
| runtime E2E | `./scripts/agent-runtime-e2e.sh` | PASS，exit 0 | `agent-runtime-e2e.txt`、`agent-runtime-e2e/` |
| residue | 端口/进程/临时库检查 | 两端口释放、无孤儿进程、无残留 DB | `residue.txt` |

测试 DB：独立数据库（本 worktree `.env.local` 的派生 DSN，未入库）；证据与提交不含 DSN/密码/token（凭据形状扫描 0 命中）。
