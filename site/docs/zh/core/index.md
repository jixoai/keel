# Keel 是什么

Keel 是 Go 编写的**通用、无状态、多租户智能体运行时**。本页是 **Core** 主线的入口：内核负责什么、
拒绝做什么、怎么跑起来，以及后续文档的读法。

Core 之上的产品层叫 **Harness**，它有独立的主线，从 [Harness 是什么](/zh/harness/) 开始。
协议名 `AgentAPI`、`AgentBackend` 保留原拼写，并注 Harness 别名（见[术语表](/zh/glossary)）。

## 它解决什么问题

你要同时跑很多持续会话——每个都可能带图片、工具调用与长上下文——同时保住三个属性，而朴素设计往往丢掉它们：

1. **内存不能随会话数增长**。进程内存由配置（2GB 预算）决定，而不是由存在多少会话决定。
2. **重启不能丢会话**。`kill -9` 之后每个会话都从原处继续，不重复可见的最终回复。
3. **反压必须真实**。网络带宽是第一显式上限；慢客户端可取消、可续传。

由此得到的设计目标（记录于 `README.md` 与 `docs/ARCHITECTURE.md:6`）：

| 维度 | 目标 |
|---|---|
| 进程内存上限 | **2GB**（Go RSS，含所有租户） |
| 租户数 | 100–200（逻辑隔离，不是 200 个进程） |
| 每租户并发 | 5–10 个会话 |
| 总会话 | **1000 个持续会话**，每个都可能带图片 |
| 流式 | 图片：存储 → base64 → HTTP 全程流式；单流缓冲 ≤64KB |
| 无状态 | `kill -9` 后重启，会话继续（SSE `Last-Event-ID`/offset 续传） |

## 塑造一切的一条规则

> **账本不能换，工位可以换；换工位靠换班（handoff），不靠热拔插。**

- **账本**是契约内核：事件顺序、幂等、工具配对、租约与所有权、租户边界、崩溃恢复真值。
- **工位**是服务图：agent loop、prompt、工具、LLM provider、会话投影、压缩、MCP、遥测、sandbox。
- **换班**是 runtime handoff：新 runtime 用新 loadout 启动，旧 runtime 排水并交出租约，新 runtime 从账本继续。

这就是为什么 Keel 不是「一切皆插件」：服务向内核**提交提案**，只有内核决定什么成为事实。这条规则的完整
表述、分层图与「可替换 / 不可替换」对照表在[架构](/zh/core/architecture)。

## 内核不做什么

内核边界由测试钉住，而不是靠约定：`internal/architecture/boundary_test.go:14` 用 `go list -deps`
断言内核包（loop、agentcore、agenttool、kernel、agent、store）**不得** import 产品包
（harnessapi、harnessext、servicehost、cmd/keel）。不属于内核：

- 认证（网络中间件的事）；
- 会话列表、停止与审批语义；
- 插件生命周期、触发器调度、子代理编排；
- token 级 partials；
- 任何 UI。

产品能力住在内核之上，由 `keel serve` 或服务图装配——怎么加见[扩展地图](/zh/core/extend/)。

## 快速开始

前置条件：PostgreSQL 18（`initdb`/`pg_ctl` 在 `PATH` 上，或任意 DSN）与 Go 1.27.3
（`go.mod:3`）。不需要 Docker。

```bash
# 1. 克隆并构建 CLI
git clone https://github.com/jixoai/keel.git
cd keel
go build -o keel ./cmd/keel

# 2. 启动 Harness
./keel serve --addr 127.0.0.1:13750 \
  --tenant 11111111-2222-3333-4444-555555555555 \
  --auth local-only
```

首次运行会自动初始化本机 PostgreSQL 18（`$KEEL_HOME/dev-pg` 或 `~/.keel/dev-pg`，仅回环）。
任意 PostgreSQL 18 的 DSN 同样可用（已有实例，或 13741 端口上的 `postgres:18` 容器）：
设 `KEEL_DSN` 即使用它，设 `KEEL_NO_AUTO_DB=1` 关闭自动初始化。

`local-only` 只应答回环并映射内置 local-admin，所以本快速开始不需要账号 fixture。真实部署用
`--auth required` + `keel admin bootstrap` + 用户名/密码（见[运维](/zh/harness/operations)）。

```bash
# 3. 建会话、提交输入、读已提交事件
T=http://127.0.0.1:13750
JAR=$(mktemp)
CSRF=$(curl -sS -c "$JAR" "$T/v1/auth/session" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrfToken"])')
SID=$(curl -sS -b "$JAR" -H "X-Keel-CSRF: $CSRF" -H 'Content-Type: application/json' \
  -d '{}' "$T/v1/sessions" | cut -d'"' -f4)

curl -sS -b "$JAR" -H "X-Keel-CSRF: $CSRF" -H 'Content-Type: application/json' \
  -d '{"text":"hello keel"}' "$T/v1/sessions/$SID/input"
curl -sS -b "$JAR" "$T/v1/sessions/$SID/events"
```

三个实际会收到的响应（在本修订上验证）：

```json
// POST /v1/sessions
{"sessionId":"586897e3-cec9-40d7-a5b0-3a9344773000"}

// POST /v1/sessions/{id}/input
{"created":true,"requestId":"9e5088a0-892c-4af3-b123-43c8a6c7fcf4","seq":1,"status":"committed","submissionId":"05c9689a-f556-45cf-8996-9b08c56144e0"}

// GET /v1/sessions/{id}/events
{"events":[
  {"seq":1,"kind":"user.message","payload":{"text":"hello keel","requestId":"…"}},
  {"seq":2,"kind":"assistant.message","payload":{"role":"assistant","content":[{"text":"fake provider received: hello keel","type":"text"}],"inputSeqs":[1],"stopReason":"end_turn"}},
  {"seq":3,"kind":"run.finished","payload":{"status":"done","turns":1,"provider":"fake","model":"fake","stopReason":"end_turn","durationMs":24}}}]
```

即使使用 fake provider，账本路径也被完整走了一遍：输入作为 edge 事件落账，worker 领取 run，loop 作答，
attempt 摘要在 settle 之前写入。要真实回答，用 `--model`/`--base-url`/`--api-key`（或
`KEEL_MODEL` 等环境变量）换上真实 provider。

只想跑裸运行时（仅 Core，无产品壳）：

```bash
go build -o keel-runtime ./cmd/runtime
KEEL_DSN="$KEEL_DSN" ./keel-runtime -addr 127.0.0.1:13740 -cas /tmp/keel-cas
```

`cmd/runtime` 与 `cmd/keel` 是同一内核的两种装配；两者的启动序列见
[嵌入 runtime](/zh/core/extend/embed-runtime)。

## 阅读地图

Core 主线由原理页与扩展路径组成：

| 页面 | 它回答什么 |
|---|---|
| [架构](/zh/core/architecture) | 账本、内核、服务图、loadout、handoff、背压分别是什么，边界在哪？ |
| [事件与协议](/zh/core/events) | 事件种类有哪些？提交如何分类、如何幂等？SSE 保证什么？ |
| [Harness API 参考](/zh/core/agentapi) | 每个 HTTP 面有哪些端点？请求/响应形状与错误码是什么？ |
| [存储与运行](/zh/core/storage) | 数据怎么存（PG、分块、CAS）？请求怎么分池？内存预算多少？ |
| [Loadout 与装配](/zh/core/assembly) | base、mode、preset 与 `config.toml` 如何叠加？row id 覆盖的是什么？ |
| [放置与队列](/zh/core/placement) | 组、租约与 queued 202 准入怎么计数？什么时候才是 429？ |
| [租户与配额](/zh/core/tenancy) | `keel serve` 与 `cmd/runtime` 下的租户、RLS 与两套配额词汇怎么工作？ |
| [文件与 Blob](/zh/core/files) | file manifest、tree object、CAS、GC 与缩略图之间是什么关系？ |
| [会话查询](/zh/core/session-query) | search、export、deliverables、task panel 与 file preview 怎么读一个会话？ |
| [扩展地图](/zh/core/extend/) | 我想改 X——该用哪个接缝？ |
| [嵌入 runtime](/zh/core/extend/embed-runtime) · [写一个服务描述符](/zh/core/extend/service-descriptor) · [接外部 backend](/zh/core/extend/external-backend) · [写 provider 适配器](/zh/core/extend/provider-adapter) · [测试与验收](/zh/core/extend/testing) | 四条扩展路径，各带代码、命令与验收口径。 |

仓库里有两份背景文档值得与这些页面并读：`docs/KERNEL.md`（内核边界与账本契约）与
`docs/SERVICE-GRAPH.md`（为什么服务图与 handoff 要这样做）。

## 状态与诚实说明

Keel 是一个持续加固中的可用运行时，不是已完成的 GA 产品。成熟度现在**按 change 取证**：
`openspec/changes/` 下每个 change 都带自己的 evidence 目录（契约快照、前后对照图、测试日志——例如
`openspec/changes/keel-oar-renderer/evidence/api-contract.txt:1`），能力声明以这些证据为准。
`docs/MATURITY.md` 保留 G0–G5 闸门与历史 v1 矩阵，但该矩阵是更早修订的快照，已不再是唯一事实源
（`docs/MATURITY.md:34`）。仍缺的能力在对应页点名——例如按租户热切换 loadout、sandbox/hooks 服务化、
外部 sidecar 的 CPU/内存预算。本站凡称「已完成」之处，均在所述修订上对过代码；凡属计划或 Deferred，
页面会明说。
