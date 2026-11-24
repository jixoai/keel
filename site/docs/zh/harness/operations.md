# 运维

本页是 Harness 部署的运行手册：如何启动、如何观察、产物在哪里、如何在不丢会话的前提下升级。
本页点名的每个面都已在本仓库交付；凡比契约更窄的部分（TUI 没有可分发产物、`permissions.fs`
尚未中介），页面会写明。

## 1. `keel serve`——Harness 宿主

```bash
export KEEL_DSN='postgres://postgres:<password>@127.0.0.1:13741/runtime?sslmode=disable'
export KEEL_SERVE_TENANT=<tenant-uuid>
export KEEL_SECRET_KEY='<32-byte base64/hex>'   # 给 session/CSRF cookie 签名
keel admin bootstrap --tenant "$KEEL_SERVE_TENANT"   # 空库首次
keel serve --dsn "$KEEL_DSN" --addr 127.0.0.1:13750
```

| 变量 / flag | 含义 | 默认 |
| --- | --- | --- |
| `KEEL_DSN` / `--dsn` | PostgreSQL DSN（必需） | — |
| `KEEL_SERVE_ADDR` / `--addr` | 监听地址 | `127.0.0.1:13750` |
| `KEEL_SERVE_TENANT` / `--tenant` | 本进程代表的单一租户（开启认证时必需） | — |
| `KEEL_SECRET_KEY` | 32 字节 base64/hex，签 cookie + CSRF；同时启用 AES-256-GCM 租户密钥存储 | `auth=required` 时必需 |
| `KEEL_AUTH` / `--auth` | `required`、`local-only` 或 `off` | `required` |
| `KEEL_BOOTSTRAP_TOKEN` | 容器式首次启动的一次性 setup token | — |
| `KEEL_SERVE_WEB_DIR` / `--web-dir` | 静态 web 构建目录 | `web/dist` |
| `KEEL_MODEL` / `KEEL_PROVIDER` / `KEEL_BASE_URL` / `KEEL_PROTOCOL` / `KEEL_API_KEY` | 模型选择（未设 = fake provider） | — |
| `KEEL_MODELS_CONFIG` / `--models-config` | 租户模型文档（providers/rules/selection） | — |
| `--config-file` / `KEEL_CONFIG_FILE` | `config.toml` 路径；文件缺失 = 空配置 | `$KEEL_HOME/config.toml` |
| `--plugins-root` / `KEEL_PLUGINS_ROOT` | 受管插件包根目录（每个插件 id 一个目录；首次安装时创建） | `$KEEL_HOME/plugins` |
| `KEEL_HOME` | 上述默认值的家目录 | `~/.keel` |
| `KEEL_SKILLS_ROOTS` | 技能根目录的 OS 路径列表（PI 兼容） | — |
| `KEEL_RUNTIME_ID` | 写入 claim/审计的运行时身份（handoff） | hostname-pid |

（出处：`cmd/keel/main.go:170` 为 config/plugins flag 及其默认值，`cmd/keel/main.go:246` 为
plugins-root 的解析顺序；另见 `cmd/keel/auth.go`、`docs/HANDOFF.md`。）

暴露端口前值得知道的运维事实：

- **认证是网络中间件，不是 API 的事**：人类用账号 + 密码换 HMAC 签名 HttpOnly cookie，
  写请求带 `X-Keel-CSRF`；机器客户端用绑定到 Bot 的 `keel_bot_...` key。中间件注入租户并
  剥离客户端提供的 `X-Tenant-ID`（`internal/harnessapi/api.go`、`cmd/keel/auth.go`）。
- **空库没有超管。** `keel admin bootstrap` 生成一次性 setup token；浏览器完成设置即完成并直接进入。CLI/管理员重置出的临时密码仍需先改密。
  不存在“第一个访问者自动成为超管”。
- `local-only` 只应答回环并映射内置 local-admin；`off` 还需显式 `--insecure`，公网绑定拒绝启动
  （`cmd/keel/auth.go`）。
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
# 机器客户端：绑定到 Bot 的 key；人类走账号会话
keel tui --url http://127.0.0.1:13750 --token "$KEEL_AGENT_TOKEN"
# 只看解析出的命令而不执行：
keel tui --print
```

TUI 是同一个客户端（同一份 TSX、同一个 Harness API）在终端宿主中的渲染；它不是第二套实现
（`docs/WEBUI.md`）。本次构建**没有可分发的 TUI 产物**：`nsh package` 没有 `tui` target
（`web/README.md:167`），因此 TUI 只能走开发宿主（`nsh run --target tui`）或嵌入
`nsh-platform-tui`。`KEEL_TUI_BIN` 指向用户自备的 TUI 二进制，`KEEL_AGENT_URL` 指向运行中
的宿主（`cmd/keel/tui.go:22`）。它刻意不渲染插件 UI——插件行改为打开 web URL
（[插件 UI](/zh/harness/plugins/ui)）。

## 3. 插件目录与生命周期 API

插件根是**受管目录** `<plugins-root>/<id>/`：安装时把包拷入、做内容哈希、绝不就地引用
（[清单](/zh/harness/plugins/manifest)）。像备份其他状态目录一样备份它；账本里没有插件二进制。

| 方面 | 交付了什么 |
| --- | --- |
| 插件从哪来 | 二进制内嵌的内置清单（`internal/pluginpkg/builtin/`，共十二个）**加上**启动时在插件根下发现的包 |
| 安装 / 升级 | `POST /v1/plugins/install` `{path}`；`POST /v1/plugins/{id}/update` `{path?}`——两阶段：`202` 打开 durable 审批（由 `POST /v1/plugin-approvals/{decisionId}` 作答），带 `decisionId` 重试回答 `201`；拒绝为 `409` |
| 启用 / 停用 / 卸载 | `POST /v1/plugins/{id}/enable`（同理 `disable`、`uninstall`）；卸载要求先 `disabled` 且无启用依赖方；内置插件只能停用，不能卸载 |
| 状态 | 持久化在 `plugins` 表（`internal/store/schema.sql:582`）：`installed`/`enabled`/`disabled`/`failed`/`invalid`；包消失时行保留状态，让原因可见 |
| 审计 | `GET /v1/plugins/audit`（`internal/store/schema.sql:603`）：每次生命周期动作、每次被拒审批或权限拒绝，按 `after` 做 keyset 分页 |
| 在哪可见 | `GET /v1/plugins`（`internal/harnessapi/plugins.go:56`）+ web 插件页；已启用插件的 UI 在 `/plugins/<id>/` |
| 持久化规则 | 持久化的停用状态跨重启保留；只有首次启动才应用内置插件的默认启用值 |

根目录为空会禁用安装（无处可拷），但内置插件照常工作——该部署仍能以装配结果回答
`GET /v1/plugins`。

## 4. 诊断

| 面 | 回答什么 |
| --- | --- |
| `keel doctor` | 构建身份、配置配对（secret/tenant/auth mode）、web 产物、TUI 产物；加 `--db` 则测数据库可达与 schema 存在。只读——不迁移、不写入、不启动 worker（`cmd/keel/doctor.go`） |
| `keel version` | 模块版本、VCS revision（含 dirty）、Go 工具链（`cmd/keel/main.go:80`） |
| `GET /v1/health` | 进程存活 |
| `GET /v1/ready` | store 就绪（迁移已应用） |
| `GET /v1/metrics`（JSON）与 `GET /metrics`（Prometheus） | 流/调度/准入计数、run 指标 |
| `GET /v1/plugins` | 清单：root、revision、generation、逐插件 state/source/path/content hash、工具、prompt 分节、hooks 摘要、诊断 |
| `GET /v1/plugins/audit` | 生命周期轨迹，外加被拒审批与权限拒绝 |
| `GET /plugins/<id>/…` | 已启用插件的托管 UI（未安装 404，未启用 403） |
| `GET /v1/triggers` | 触发器清单 + 审计窗口（`internal/harnessapi/ext.go:39`） |
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
# 2) 让旧进程排水（人类账号 cookie + CSRF，或 Bot key）：
curl -XPOST $old/v1/runtime/drain -b "$JAR" -H "X-Keel-CSRF: $CSRF" \
  -d '{"mode":"handoff","deadlineMs":30000}'
# 3) 观察它安静下来：
curl -s -b "$JAR" $old/v1/runtime/status     # claiming=false, inFlightRuns=0
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
