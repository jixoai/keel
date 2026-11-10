# Keel · Harness

> **Keel（龙骨）**——船体最承重的结构。Keel 是智能体集群的龙骨：
> 一个**通用、无状态、多租户**的运行时底座：契约内核（Core）+ 产品层（**Harness**，原 Agent）。
>
> **Keel 是 [PI](https://github.com/earendil-works/pi) 的 Go 重构**：接口、事件语义与
> 生态产物（SKILL.md / MCP / 压缩 / 遥测）以 PI 为唯一参照；只替换运行时
> （PostgreSQL / SSE / 多租户 / 2GB）。见 [docs/PI-PARITY.md](docs/PI-PARITY.md)。

Keel 解决一个具体问题：当你要同时跑很多持续会话（每个都可能带图片、工具调用、长上下文），
又不想让进程内存随会话数增长，也不想在重启时丢会话，底层的执行、存储、流式与反压应该长什么样。

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
| 无状态 E2E | **7/7**：requestId 幂等、SSE 游标、blob 续传、agent 回复、模型/工具执行中 `SIGKILL` 恢复、100 会话 2000 事件序列连续 |
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

## 快速开始

```bash
docker run -d --name keel-pg -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 --memory=2g postgres:18

export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
go run ./cmd/runtime -addr 127.0.0.1:13740 -cas /tmp/keel-cas
```

完整命令（测试、E2E、压测、2GB cgroup 复跑）见 [docs/RUNBOOK.md](docs/RUNBOOK.md)。

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

不搬：CLI/TUI/REPL、session JSONL、trust、plugin/pkgmgr、selfupdate、内置 coding 工具。
pigo 的 `Session` 是有状态、非并发安全的，且 SDK 没有历史注入/图片输入——这些由 Keel 重做。
来源与 pin commit 见 [docs/UPSTREAM.md](docs/UPSTREAM.md)。

## License

[Apache-2.0](LICENSE)。从 pigo 搬运的文件保留 MIT 版权声明，全文见
[third_party/pigo-LICENSE](third_party/pigo-LICENSE)；归属说明见 [NOTICE](NOTICE)。
