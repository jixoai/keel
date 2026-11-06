# FDE2E Keel · Agent Runtime

> **Keel（龙骨）**——船体最承重的结构。FDE2E Keel 是智能体集群的龙骨：
> 给所有 Agent 产品提供执行、会话、持久化、流式与多租户能力的**通用无状态底座**。
> 与 Hive 无关，不绑定任何具体产品。

## 目标（Owner 2026-11-05）

- **2GB 内存**、100–200 个**逻辑隔离租户**、每租户 5–10 个并发、**1000 个持续会话**；
- 极致的**流式**：图片从存储 → base64 → HTTP 全程流式，绝不整图进内存；
- **无状态**：应用进程可任意 kill，重启后会话继续，不丢不重；
- 图片优先进 PostgreSQL（分块），大文件/原图按分层策略走内容寻址存储；
- 全链路**反压**：网络带宽是首要上限，按请求尺寸分池、慢客户端可恢复。

## 技术栈

| 层 | 选择 |
|---|---|
| 语言 | **Go 1.27.3**（使用最新稳定版，安全补丁） |
| Agent 引擎 | 从 [pigo](https://github.com/smallnest/pigo)（MIT）**搬运最小必要代码**后改造；不 fork、不跟随上游 |
| 状态存储 | **PostgreSQL 18**（单库 + `tenant_id` + RLS；逻辑隔离） |
| 接入 | `net/http`（SSE 与图片流）、pgx/v5、TLS 在入口层终止 |
| 图片 | PG 64KB 分块 `bytea`（缩略图/小图）＋ 内容寻址大文件（原图）＋ 100MB 进程内 LRU |
| 反压 | 全局限速 + 按尺寸分池 + 有界 worker + 慢客户端取消 + Range/offset 续传 |

## 与 pigo 的关系

pigo 是 MIT 许可的 Go 版 pi Agent。我们**不 fork**，只搬运并改造以下模块（保留版权声明）：

- `provider`（OpenAI/Anthropic/OpenRouter/Ollama 协议与流式）
- `runtime` 的 Agent loop（两层循环、工具轮、停止原因）
- `agenttool` 的工具执行器（并行/流式/结果裁剪）
- `compaction`（上下文压缩）
- `agentcore` 的内容类型（text/thinking/toolCall/toolResult/image）

不搬：CLI/TUI/REPL、session JSONL、trust、plugin/pkgmgr、selfupdate、内置 coding 工具。
pigo 的 `Session` 是有状态、非并发安全的，且 SDK 没有历史注入/图片输入——这些由本仓重做。

## 文档

- [架构与决策](docs/ARCHITECTURE.md)
- [OpenSpec change: runtime-foundation](openspec/changes/runtime-foundation/proposal.md)
- 上游参考：pigo（MIT，pin commit 记录于 `docs/UPSTREAM.md`）

## 已实测（2026-11-05，本机 Go 1.27.3 + PG 18）

| 指标 | 结果 |
|---|---|
| 并发会话 | **1000**（每会话一条 SSE + 周期图片） |
| 图片 | 1MiB/张，120s 内 **2867 张**完成（约 4.0GB base64 出站） |
| SSE 稳定性 | **0 次异常断开**（120s） |
| 吞吐 | 33.4 MB/s 平均（客户端限速 100KB/s/流，非服务端上限） |
| 延迟 | p50 14.1s / p95 16.0s / p99 22.6s（= 1.4MB ÷ 100KB/s，服务端附加≈0） |
| 进程 RSS | p50 **300MB**，p95 431MB，**max 437MB**（`GOMEMLIMIT=1.6GiB`） |
| 反压 | 588 次 429（容量满时快速拒绝，客户端按 Retry-After 重试） |
| 无状态 E2E | **4/4**：请求幂等、SSE 游标、blob 续传、100 会话 2000 事件，全部跨 `SIGKILL` |

结论：**437MB 就装下了 1000 个带图片的持续会话**，2GB 预算有 4.5× 余量。
细节与设计见 `docs/BACKPRESSURE.md`、`docs/BLOB-STORAGE-RESEARCH.md`、`docs/FILE-PROTOCOL.md`。

## 状态

**可运行骨架 + 实测通过**（2026-11-05）。已搬运 pigo 的 agentcore/provider/compaction/
agenttool/loop；已实现 PG schema、64KiB 分块 blob、流式 base64、SSE 续传、三类分池与全局带宽。
下一步：把 Agent loop 接到事件日志（工具执行/压缩的持久任务）、provider 文件协议 adapter。
