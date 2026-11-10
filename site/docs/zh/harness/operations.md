# 运维

本页是 Harness 部署的运行手册：如何启动、如何观察、产物在哪里、如何在不丢会话的前提下升级。
覆盖已实现的面（附出处）与规划中的插件目录（已标注）。

## 1. `keel serve`——Harness 宿主

```bash
export KEEL_DSN='postgres://postgres:<password>@127.0.0.1:13741/runtime?sslmode=disable'
export KEEL_SERVE_TENANT=<tenant-uuid>
export KEEL_SERVE_PASSWORD='<passphrase>'      # 或 ""（仅回环）
keel serve --dsn "$KEEL_DSN" --addr 127.0.0.1:13750
```

| 变量 / flag | 含义 | 默认 |
| --- | --- | --- |
| `KEEL_DSN` / `--dsn` | PostgreSQL DSN（必需） | — |
| `KEEL_SERVE_ADDR` / `--addr` | 监听地址 | `127.0.0.1:13750` |
| `KEEL_SERVE_TENANT` / `--tenant` | 本进程代表的单一租户（开启认证时必需） | — |
| `KEEL_SERVE_PASSWORD` / `--password` | 本地口令；置空关闭认证（仅回环） | — |
| `KEEL_SERVE_WEB_DIR` / `--web-dir` | 静态 web 构建目录 | `web/dist` |
| `KEEL_MODEL` / `KEEL_PROVIDER` / `KEEL_BASE_URL` / `KEEL_PROTOCOL` / `KEEL_API_KEY` | 模型选择（未设 = fake provider） | — |
| `KEEL_MODELS_CONFIG` / `--models-config` | 租户模型文档（providers/rules/selection） | — |
| `KEEL_SKILLS_ROOTS` | 技能根目录的 OS 路径列表（PI 兼容） | — |
| `KEEL_SECRET_KEY` | 启用 AES-256-GCM 租户密钥存储；未设则显式禁用 | — |
| `KEEL_RUNTIME_ID` | 写入 claim/审计的运行时身份（handoff） | hostname-pid |

（出处：`cmd/keel/main.go:53`、`cmd/keel/serve.go:37`、`docs/HANDOFF.md`。）

暴露端口前值得知道的运维事实：

- **认证是网络中间件，不是 API 的事**：web 用 HMAC 签名的 HttpOnly cookie，CLI/TUI 用
  `Authorization: Bearer`；中间件注入租户并剥离客户端提供的 `X-Tenant-ID`
  （`internal/agentapi/api.go:18`、`cmd/keel/auth.go:63`）。
- `--password ""` **只应答回环 Host**——关闭认证的监听地址若绑定通配，会拒绝非回环请求而不是
  暴露租户（`cmd/keel/auth.go:100`）。
- `keel serve` 是**单租户**（`KEEL_SERVE_TENANT`）；多租户复用需要多租户前置层（那是产品决策，
  不是 flag）。
- 进程无状态：一切事实在 PostgreSQL（大 blob 另加 CAS），因此 `kill -9` 是被支持的失败模式，
  而不是事故（`docs/ARCHITECTURE.md` §3）。
- 仓库约定：开发端口位于 **13610–13799**；`keel serve` 默认 13750，TUI 默认连同一 origin
  （`cmd/keel/serve.go:66`）。

### 静态 UI

`keel serve` 从配置目录托管 NSH web 构建；NSH 打成嵌套根
（`…/web/<app>/<App>/index.html`），handler 在启动时解析一次而不是逐请求猜测
（`cmd/keel/static.go:16`）。构建缺失时，非 API 路径返回 JSON 404，指明缺失产物与构建命令——
绝不返回空白页（`cmd/keel/static.go:13`）。

## 2. `keel tui`

```bash
keel tui --url http://127.0.0.1:13750 --token "$KEEL_SERVE_PASSWORD"
# 只看解析出的命令而不执行：
keel tui --print
```

TUI 是同一个客户端（同一份 TSX、同一个 AgentAPI）在终端宿主中的渲染；它不是第二套实现
（`docs/WEBUI.md`）。`KEEL_TUI_BIN` 指向打包的 TUI 二进制
（`nsh package --target tui`），`KEEL_AGENT_URL` 指向运行中的宿主（`cmd/keel/tui.go:22`）。
它刻意不渲染插件 UI——插件行改为打开 web URL
（[插件 UI](/zh/harness/plugins/ui)）。

## 3. 插件目录

| 方面 | 今天 | 规划 |
| --- | --- | --- |
| 插件从哪来 | 编译进二进制（两个内置：`keel.example`、`keel.subagents`——`internal/agentext/builtin.go:38`、`:256`） | 启动时发现 `<plugins-root>/<id>/` 包 |
| 启用/停用 | 进程装配（`agentext.Runtime.Apply`） | 生命周期 API + 审批（[生命周期](/zh/harness/plugins/lifecycle)） |
| 状态 | 逐插件 `active`/`pending`/`failed`/`stopped`、generation 计数、诊断 | 加上 `installed`/`disabled`，以及安装/卸载/更新审计 |
| 在哪可见 | `GET /v1/plugins` + web 设置 › 插件（`internal/agentapi/ext.go:18`、`web/src/screens/settings.tsx:1148`） | 同一端点扩展，而非替换 |

规划中的插件根目录是**受管目录**：包被拷入其中、安装时做内容 hash、绝不就地引用
（[清单](/zh/harness/plugins/manifest)）。像备份其他状态目录一样备份它；账本里没有插件二进制。

## 4. 诊断

| 面 | 回答什么 |
| --- | --- |
| `keel doctor` | 构建身份、配置配对（password/tenant）、web 产物、TUI 产物；加 `--db` 则测数据库可达与 schema 存在。只读——不迁移、不写入、不启动 worker（`cmd/keel/doctor.go:16`、`:26`） |
| `keel version` | 模块版本、VCS revision（含 dirty）、Go 工具链（`cmd/keel/main.go:80`） |
| `GET /v1/health` | 进程存活 |
| `GET /v1/ready` | store 就绪（迁移已应用） |
| `GET /v1/metrics`（JSON）与 `GET /metrics`（Prometheus） | 流/调度/准入计数、run 指标 |
| `GET /v1/plugins` | 装配结果：generation、插件状态、工具、hooks 摘要、诊断 |
| `GET /v1/triggers` | 触发器清单 + 审计窗口（`internal/agentapi/ext.go:39`） |
| `GET /v1/sessions/{id}/run` 与 `/run/loadout` | run 行（status/attempts/cursor/lastError）与钉住的 loadout（`docs/LOADOUT.md` §5） |
| `POST /v1/runtime/drain`、`GET /v1/runtime/status` | 排水控制与可观测性（`docs/HANDOFF.md`） |
| `GET /v1/sessions/{id}/decisions` | 未决/已答/超时审批 |

日志是结构化的（`log/slog`）；内核拒绝被投影为结构化审计字段并限频，「这次提交为什么失败」
因此是一次日志查询，而不是一场排障（`internal/servicehost/audit.go`、`docs/LOADOUT.md` §6）。

## 5. 升级与回滚

升级是 **handoff**，不是重启（`docs/HANDOFF.md`；完整描述见
[恢复与压缩](/zh/harness/recovery-and-compaction) §3）：

```bash
# 1) 启动新二进制（同一 DSN，不同的 KEEL_RUNTIME_ID）
# 2) 让旧进程排水：
curl -XPOST $old/v1/runtime/drain -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
  -d '{"mode":"handoff","deadlineMs":30000}'
# 3) 观察它安静下来：
curl -s $old/v1/runtime/status     # claiming=false, inFlightRuns=0
# 4) SSE 客户端收到 `event: drain` 后重连，并从 Last-Event-ID 续传
```

强制纪律：

- **绝不两个写者并存**——旧二进制不过围栏；无论升级*还是*回滚，都必须先排空旧进程再让新进程
  接班（`docs/HANDOFF.md`、`internal/kernel/cutover_test.go`）；
- `handoff` 让在途 run 跑到提交边界结算并清除租约；`shutdown`（SIGTERM）放弃它们，新进程需等
  一个有界租约 TTL；
- 回滚 = 排水 → 停新 → 起旧；**不要回滚数据**——账本只追加，旧二进制读事件时跳过未知 kind
  （`docs/HANDOFF.md`「回滚手册」）。

## 6. 容量备注

Harness 继承 Core 的预算（2GB、~1000 并发会话），流路径通常是第一个要查看的地方：新流经受
token bucket 准入（429 + `Retry-After`），调度器把控制帧与批量 blob 分离，慢客户端被写/进度
截止切断（[流式与 partial](/zh/harness/streaming-and-partial) §3）。容量证据与 1000 会话实测
见 `README.md` 与 `docs/ARCHITECTURE.md` §8。
