# Keel · Harness

> **Keel（龙骨）**——船体最承重的结构。Keel 是智能体集群的龙骨：
> 一个**通用、无状态、多租户**的运行时底座：契约内核（Core）+ 产品层（**Harness**：
> Bot / TeamBot / Account / Role / Profile）。
>
> **Keel 是 [PI](https://github.com/earendil-works/pi) 的 Go 重构**：接口、事件语义与
> 生态产物（SKILL.md / MCP / 压缩 / 遥测）以 PI 为唯一参照；只替换运行时
> （PostgreSQL / SSE / 多租户 / 2GB）。见 [docs/PI-PARITY.md](docs/PI-PARITY.md)。

Keel 解决一个具体问题：当你要同时跑很多持续会话（每个都可能带图片、工具调用、长上下文），
又不想让进程内存随会话数增长，也不想在重启时丢会话，底层的执行、存储、流式与反压应该长什么样。

## 下载与一键开始（推荐）

每个平台一个**单体二进制**：内嵌 PostgreSQL 18、网页客户端与图标，不需要 Docker、
不需要 `KEEL_DSN`、不需要手动建库。

```bash
# 从 Releases 下载对应平台的 keel（macOS/Linux/Windows）
chmod +x keel

# macOS：生成 ~/Applications/Keel.app（本机 ad-hoc 签名，不公证），双击即用
./keel install

# 或者不安装，直接启动（所有平台）
./keel start
```

`keel install` 还会写一个 `~/.local/bin/keel` 启动器（只覆盖自己生成的文件），
装了 `.app` 也能直接 `keel update` / `keel db`；该目录不在 PATH 时会打印提示。
`keel start` 首次会把内嵌运行时解到 `$KEEL_HOME/runtime/<version>`，自动初始化本地库、
以 local-only 模式起服务并打开浏览器。`keel uninstall` 移除应用（默认保留数据，
`--purge` 才删）。Docker 用户仍可用 compose（PG 容器在 compose 里，二进制不内嵌）。

## 升级（无需签名）

```bash
./keel update --check   # 只检查是否有新版本
./keel update           # 下载 → SHA-256 校验 → 原子替换
```

更新读取 GitHub Release 发布页（不走有 60 次/时限制的 API），按
`keel-<version>-<os>-<arch>` 选资产，用同版本 `SHA256SUMS.txt` 校验后替换。
不需要 Apple 公证或 Windows 代码签名：macOS `.app` 内替换后重新 ad-hoc 签名，
Windows 由退出后的小助手换掉运行中的 exe 再启动。`--force` 重装同版本，
`--manifest URL` 指向自建发布；`KEEL_UPDATE_MANIFEST` 可设默认值。
Windows 安装版未进 PATH 时用 `%LOCALAPPDATA%\Keel\keel.exe update`。

> v0.3.1 及更早的二进制没有 `update` 子命令，第一次升级需要手动下载；从 0.4.0 起可用。

## 设计目标

- **2GB 内存**装下 **100–200 个逻辑隔离租户**、每租户 5–10 并发、**1000 个持续会话**；
- **极致流式**：图片从存储 → base64 → HTTP 全程流式，绝不整图进内存；
- **无状态**：应用进程可任意 `kill -9`，重启后会话继续，不丢不重；
- **全链路反压**：网络带宽是首要上限，按请求尺寸分池，慢客户端可取消、可续传；
- **逻辑隔离**：单 PostgreSQL 库 + `tenant_id` + 复合索引（RLS 可选），不是每租户一个进程/库。

## 已实测

1000 个持续会话：每会话一条 SSE + 周期文本，单轮 1MiB 图片下载，客户端每流限速 50KB/s，
进程限制在 2GB cgroup（Linux/arm64）：

| 指标 | 结果 |
|---|---|
| 并发会话 | **1000**（1000 条 SSE 同时在线） |
| 图片 | 一轮内 **800 张 1MiB 图完成**（1.12GB base64 出站；另 200 张按尺寸分池返回 429，客户端重试） |
| SSE 稳定性 | **0 次硬错误**（429 准入反压由客户端按 `Retry-After` 重试，不计硬错误） |
| 吞吐 | 18.6 MB/s（客户端限速主导，非服务端上限） |
| 延迟 | p50 28.9s / p95 31.4s（= 1.4MB ÷ 50KB/s，服务端附加≈0） |
| 2GB cgroup 峰值 | **640MB（29.8%）**，未 OOM（`memory.peak` 口径，`GOMEMLIMIT=1.5GiB`；双进程 handoff 峰值 656MB/32.0%） |
| 反压 | 1761 次 429（Medium 档 40% 容量上限；快速拒绝并按 `Retry-After` 重试） |
| 无状态 E2E | **8/8**（`cmd/e2e/main.go:86-95`）：requestId 幂等、SSE 游标、blob 续传、重试追加、agent 回复、模型/工具执行中 `SIGKILL` 恢复、100 会话 2000 事件序列连续 |
| 服务图验收 | 全仓 `-race` 31 包 0 失败 · PI conformance **674**（0 失败/1 跳过）· R1 差分 102 路径/12 条规则全部命中 · SIGTERM 排水 8/8 与 in-flight 探针 4/4 · chaos：kill 10 次故障 + handoff 6 轮换代（接班由 `runtime_instances`/`handoffs`/`owner_epoch` 证明）——见 [docs/SERVICE-GRAPH.md](docs/SERVICE-GRAPH.md) |

## 架构

```
                    ┌──────────────────────────────────────┐
   HTTP/SSE  ──────▶│  Keel (Go, 1 process)                 │
   (1000 conn)      │  ┌─────────┐ ┌─────────┐ ┌────────┐  │
                    │  │ API/SSE │ │ Harness │ │ Blob   │  │
                    │  │  lane   │ │ loop    │ │ stream │  │
                    │  └─────────┘ └─────────┘ └────────┘  │
                    └───────┬───────────────┬──────────────┘
                            │               │
                   PostgreSQL (single DB)    local NVMe CAS
                   events/checkpoints/blobs  originals/large blobs
```

- **事件日志是唯一真值**：每次状态变化一行 append-only 事件，checkpoint 只是投影；
- **SSE 续传**：`Last-Event-ID`/`after` 游标 + `LISTEN/NOTIFY` 唤醒 + 心跳；
- **64KiB 分块 blob**：`base64.NewEncoder`/整块 `Encode` + `io.Copy`，4 个 base64 字符 ≡ 3 个源字节，
  所以任意 wire offset 都能无损续传；
- **反压三层**：阻塞 `Write` 提供天然背压，全局 token bucket 塑形总带宽，per-write deadline 杀慢客户端。

详细设计：[PI 对齐基线](docs/PI-PARITY.md) · [官方测试对齐矩阵](docs/PI-CONFORMANCE.md) · [架构与决策](docs/ARCHITECTURE.md) · [反压设计](docs/BACKPRESSURE.md) ·
[存储调研](docs/BLOB-STORAGE-RESEARCH.md) · [文件协议](docs/FILE-PROTOCOL.md) · [运行手册](docs/RUNBOOK.md) ·
[Harness 产品层](docs/HARNESS.md)（对外文档：<https://keel.jixoai.com>）

## 技术栈

| 层 | 选择 |
|---|---|
| 语言 | **Go 1.27.3** |
| 智能体引擎 | 从 [pigo](https://github.com/smallnest/pigo)（MIT）搬运最小必要代码后改造；不 fork |
| 状态存储 | **PostgreSQL 18**（单库 + `tenant_id`） |
| 接入 | `net/http`（SSE 与图片流）、pgx/v5 |
| 图片 | PG 64KiB 分块 `bytea`（≤1MiB）＋ 本地内容寻址存储（>1MiB） |
| 反压 | 全局带宽桶 + 三类分池 + 有界 worker + 慢客户端取消 + Range/offset 续传 |

## 从源码运行

```bash
git clone https://github.com/jixoai/keel.git && cd keel

# 1. 构建 CLI
go build -o keel ./cmd/keel

# 2. 启动产品壳（首次自动初始化本机 PostgreSQL 18，无 Docker）
./keel serve --addr 127.0.0.1:13750 --auth local-only \
  --tenant 11111111-2222-3333-4444-555555555555
```

首次启动会自动初始化本机 PostgreSQL 18 集群（默认 `$KEEL_HOME/dev-pg`，未设置时
`~/.keel/dev-pg`；仅回环、随机 0600 密码），不需要 Docker，也不需要先执行建库命令。
要用已有数据库，设 `KEEL_DSN`；要关闭自动初始化，设 `KEEL_NO_AUTO_DB=1`。
`keel db status|stop|reset --yes` 用于管理本地集群，`keel db start` 仍可显式启动。
多租户 API 进程 `cmd/runtime` 仍要求显式 DSN。

**已有 PG 或偏好容器**：`keel` 只认一个 PostgreSQL 18 DSN，任选其一——

```bash
# 已有本机实例
createdb keel && export KEEL_DSN='postgres://<user>@127.0.0.1:5432/keel?sslmode=disable'

# 或一次性容器（与 dev-pg 同为 13741，不要同时开）
docker run -d --name keel-pg -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 --memory=2g postgres:18
export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
```

少数验收脚本默认找名为 `keel-pg` 的容器，但都可用 `KEEL_DSN`/`KEEL_TEST_DSN` 指向本机库；
只有 `scripts/acceptance-perf.sh` 的 2GB cgroup 电池确实需要 Docker/Linux——这是 E0 容器电池
标「平台受限」的原因，原生电池（conformance/mcp-apps/external/core）不依赖它。

完整命令（测试、E2E、压测、2GB cgroup 复跑）见 [docs/RUNBOOK.md](docs/RUNBOOK.md)。

### 本地评审服务器（账号登录 / Bot key）

`scripts/serve-local.sh` 从**个人 env 文件**读取 `KEEL_DSN` / `KEEL_SERVE_TENANT` /
`KEEL_SECRET_KEY` / `KEEL_SERVE_ADDR`，再启动 `keel serve`。cookie/CSRF 签名密钥与 DSN 因此
不出现在命令行、shell 历史或 `ps` 输出里。

```bash
scripts/serve-local.sh --build   # 编译并前台启动
scripts/serve-local.sh --stop    # 停止
```

- **人类**用账号用户名 + 密码登录（`KEEL_ACCOUNT_USERNAME` / `KEEL_ACCOUNT_PASSWORD`，或
  `--password-file`）；第一次用 CLI/TUI 前先登录，写请求带 `X-Keel-CSRF`。
- **机器客户端**用绑定到具体 Bot 的 key（`KEEL_AGENT_TOKEN`，`keel_bot_...`），不走浏览器登录。
- 空库首次启动：运行 `keel admin bootstrap`（或设置 `KEEL_BOOTSTRAP_TOKEN`），在浏览器用一次性
  setup token 设置首个超管；密码设置即完成，直接进入应用。脚本只提示，不打印令牌。
- 旧的 `KEEL_SERVE_PASSWORD` 已删除且不被脚本读取；旧 cookie、旧 API key、
  旧 `agents`/`agent_teams` schema 均不兼容，升级即重新初始化账号（alpha 零兼容）。

env 文件路径：`FDE2E_ENV_PERSONAL`，默认取主仓根目录的 `.env.personal`（gitignored）；
本 worktree 会自动向上解析三层。需要新增变量时只改那个文件，脚本本身不保存任何凭据。
术语与旧词对表见 [docs/PRODUCT-TERMS.md](docs/PRODUCT-TERMS.md)。

### Board（keel-board v0.1.1）

`KEEL_BOARD_ENABLED=1` 时，`keel serve` 挂载 `github.com/jixoai/keel-board@v0.1.1`（commit
`70b503f0cdbd8f860ac1a373a6d1576175018ff0`，Apache-2.0；见 `NOTICE`）：

- **API**：`/v1/board/*`（频道/消息/线程/任务/claim/needs/SSE/审计/sync 管理）；沿用 keel
  登录与 Bot key，不新增认证方式。
- **权限**：`board.read` / `board.write` / `board.manage` 三个权限键 + 频道
  `resource_relations`（`resource_type=board_channel`，viewer/editor/owner，最近祖先继承）。
  镜像任务动作按频道关系校验；默认运营组 `tenant-admin`/`bot-operator` 获得 mirrored
  channel 的 owner（`bot-operator` 预设含 `board.read/write`，不含 `board.manage`）。
  创建频道者自动成为 owner；不可读频道返回 404，可读但无写权限返回 403。
- **存储**：board 自己的迁移（`board_*`）在启动时幂等执行；RLS 读 `keel.tenant_id`，与 keel
  既有的 `app.tenant_id` 策略互不覆盖。默认复用 `KEEL_DSN`；`KEEL_BOARD_DSN` 可指向专用
  数据库/非超级用户角色，`KEEL_BOARD_SCHEMA` 可隔离 schema。
- **实时**：`GET /v1/board/events`（Last-Event-ID、频道/标签过滤、15s 心跳、55s 重连）。
- **观测**：board 指标并入 `GET /metrics`（`board_*`）；拒绝写 `auth_events`
  （`permission.denied`）；sync 运维审计写 `board_audit`，均不含正文/secret。
- **开关**：三层默认关闭。部署级 `KEEL_BOARD_ENABLED=1` 是前置开关；tenant 配置
  `board.enabled` 与 `board.sync.enabled` 再逐租户开启（`{"board":{"enabled":true,
  "sync":{"enabled":true}}}`）。关闭时 `/v1/board/*` 返回 404 `board_disabled`，
  sync/source/webhook 返回 404 `sync_disabled`，webhook 在任何读取/写入前拒绝。
- **平台 webhook**：`POST /webhooks/{key}` 不走 board 会话认证，由对应 provider 验签；
  未配置 entry 时 fail closed。
  Chat SDK bridge `POST /v1/board/sync/bridge/{key}/events` 用 HMAC v1 + nonce，不经会话。
- **Sync**：tenant 配置只存 `secret_ref`（tenant secret 名称），凭据在 keel 的
  `tenant_secrets`（`KEEL_SECRET_KEY`）加密存储。`POST/PATCH /v1/board/sync/entries`
  （`board.manage` + `secret.write`）创建/启停/轮换/路由；`routes` 目标频道必须已存在且
  至少有一条 `board_channel` relation，否则 400（先 `POST /v1/board/channels` 创建/认领）；
  启停即时生效，创建/轮换/路由变更在重启后装载（变更期间 entry 在 board sync store 中停用，
  旧凭据不会继续服务）。
  inbox/outbox、board 事件订阅、watcher/schedule worker 随 `keel serve` 启动；
  每 5s 跟随 tenant flags、durable entry 与凭据指纹，关闭/停用/轮换即暂停，多副本由同一
  durable row 收敛。board 附件的字节走 keel blob store（真实读写，不再 501）。
- **部署入口**：Dockerfile/`cmd/runtime` 不含 board；board 只在 `keel serve` 中启用。
- **superagent 镜像/命令桥**：tenant 文档 `board.superagent.{mirror,bridge,channel}`（默认
  `false/false/"superagent"`）。`mirror=true` 把 Ticket/Assignment/Attention/report 单向投影为
  board 任务/claim/blocked/回复（`board_links` 幂等、`KEEL_SECRET_KEY` 派生确定性 claim token，
  缺失即 fail closed）；`bridge=true` 让 board 上的 claim/complete/fail/block/unblock/assign/
  cancel 先调 supervisor 既有 API，成功后才写 board。superagent 的 `collab_*` 仍是真值，
  board 不反写；旧 `/superagent` 看板已冻结，Phase 2 15.7 删除。

运行细节（DSN 角色、迁移、tenant 开关、entry/secret 配置、验收与 E2E 命令）见
[docs/RUNBOOK.md](docs/RUNBOOK.md#boardkeel-board-v011)。

## 状态与路线

**可运行底座 + PI 对齐**（2026-11-06）。已实现：PG schema、64KiB 分块 blob、流式 base64、
SSE 续传、三类分池与全局带宽；**Harness 执行**——事件溯源上下文、持久 run（租约 +
`FOR UPDATE SKIP LOCKED`）、工具执行、崩溃恢复（模型调用中 / 工具执行中 kill 均可续跑）；
以及 **PI 能力的宿主装配**——system prompt sections、Skills（`SKILL.md` + 按需加载）、
Hooks（扩展事件 + 命令钩子）、Compaction（PI policy + checkpoint 事件）、MCP（stdio /
Streamable HTTP / `mcp.json` / OAuth provider 接口）、Durable tasks（提交/生成/嵌套工具/
结构化输出）、Telemetry（span/metric/store）。
正在补：MCP 交互式 OAuth 登录（discovery/loopback/PKCE）、durable 存储投影（PG 只落
tasks/submissions，inbox/live/nested docs 仍由宿主存储承担）、租户配额/RLS、缩略图与 TTL 回收、CI/部署。

路线与「做到什么程度算好」以 **[docs/MATURITY.md](docs/MATURITY.md)** 为准：
每个能力按 G0（规格）→ G1（单机）→ G2（崩溃一致）→ G3（可观测）→ G4（生态兼容）→
G5（规模与运维）六级验收，**GA = 六项能力全部 G5**。规划见
[openspec/changes/keel-ecosystem](openspec/changes/keel-ecosystem/proposal.md) 与
[openspec/changes/keel-ga](openspec/changes/keel-ga/proposal.md)。

## 与 pigo 的关系

pigo 是 MIT 许可的 Go 版 pi Agent。Keel **不 fork**，只搬运并改造以下模块（保留原始版权头）：

- `provider`（OpenAI/Anthropic/OpenRouter/Ollama 协议与流式）
- `runtime` 的 agent loop（两层循环、工具轮、停止原因）
- `agenttool` 的工具执行器（并行/流式/结果裁剪）
- `compaction`（上下文压缩）
- `agentcore` 的内容类型（text/thinking/toolCall/toolResult/image）

不搬：CLI/TUI/REPL、session JSONL、trust、plugin/pkgmgr、pigo 的 selfupdate（Keel 的更新器另行实现）、内置 coding 工具。
pigo 的 `Session` 是有状态、非并发安全的，且 SDK 没有历史注入/图片输入——这些由 Keel 重做。
来源与 pin commit 见 [docs/UPSTREAM.md](docs/UPSTREAM.md)。

## License

[Apache-2.0](LICENSE)。从 pigo 搬运的文件保留 MIT 版权声明，全文见
[third_party/pigo-LICENSE](third_party/pigo-LICENSE)；归属说明见 [NOTICE](NOTICE)。
