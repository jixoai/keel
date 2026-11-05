# Proposal: runtime-foundation

> 立项：Owner 2026-11-05。本仓＝**通用 Agent 无状态运行时底座**（Go 1.27.3 + PostgreSQL），
> 与 Hive 及任何具体产品无关。上游参考 pigo（MIT），**不 fork**，按模块搬运必要代码后改造。

## Why

智能体产品（Omni / Echo / Lens / Meta / Void / Moon / Browser / Hive）都需要同一套底层能力：
无状态执行、会话持久化、流式输出、多租户隔离、图片/文件协议。当前每个产品各自实现（或依赖
有状态的 Node 运行时），导致：进程内存不可控、崩溃即丢会话、图片整图进内存、无法多租户。

Owner 给出硬指标：**2GB 内存、100–200 逻辑隔离租户、每租户 5–10 并发、1000 个带图片的持续会话**，
并且要**极致流式**（存储 → base64 → HTTP 全链路）与**全链路反压**。这要求一个专门的底座，
而不是在既有产品里打补丁。

## What Changes

### 能力（New Capabilities）

- `runtime-core`：**无状态 Agent 循环**——事件日志唯一真值、checkpoint 投影、每个 await 点可重入、
  `kill -9` 后 `resume()` 续跑；模型请求是已提交消息的纯函数；请求幂等（`requestId`）。
- `runtime-stream`：**SSE 流式**——`Last-Event-ID`/offset 续传、心跳、缺口补齐、慢客户端取消；
  与 Agent 事件（文本/思考/工具调用/工具结果）解耦。
- `blob-store`：**PG 分块大对象 + 本地 CAS**——64KB chunk 流式读写、`ref_count`/TTL 回收、
  原图落 NVMe、缩略图在 PG；`POST /v1/files` 上传即返回 `file_id`。
- `runtime-backpressure`：**全链路反压**——全局带宽预算、按尺寸分池、有界 worker、慢客户端取消、
  Range 续传、重试预算。
- `tenant-isolation`：**逻辑隔离**——单库 `tenant_id` + 复合索引 + RLS（可选）、每租户并发/带宽/存储配额。
- `provider-bridge`：**模型协议适配**——OpenAI Chat/Responses、Anthropic、OpenRouter、Ollama；
  流式事件统一；图片支持 URL 与 base64 两种出站形态。

### 复用与改造（从 pigo 搬运，MIT）

| pigo 模块 | 搬运内容 | 改造 |
|---|---|---|
| `internal/agentcore` | Content/Message/Event/Tool 类型 | 新增 `FileRef`；无其它改动 |
| `internal/provider` | 协议、流式、认证解析 | 去全局单例/环境变量直读；注入 `http.Client`；接反压 |
| `internal/runtime` | Agent loop、两层循环、停止原因 | 状态外置 PG；可重入；context 取消 |
| `internal/agenttool` | 工具执行器（并行/流式/裁剪） | 幂等键、超时、重试预算 |
| `internal/compaction` | 上下文压缩 | 压缩事件追加不覆盖；token 预算 |

**不搬**：CLI/TUI/REPL、session JSONL、trust、plugin/pkgmgr、selfupdate、内置 coding 工具。

### Modified Capabilities

无（新仓，无既有规格）。

## Impact

- **新仓** `jixoai/fde2e-agent-runtime`（私有；后续按 Owner 决定是否开源）。
- **主仓**：作为 submodule 引入，`PROJECT-MAP.md` 登记。
- **下游**：所有 Agent 产品（omni/echo/lens/meta/void/moon/browser）未来可接入；
  本 change 不修改任何现有产品。
- **风险**：PG 2GB 调参在真实负载下需复验；1000 流 × 1MB 图片是极限场景，可能需要降级策略
  （低带宽客户端只发缩略图）；Go 1.27.3 较新，部分库兼容性待验证。
