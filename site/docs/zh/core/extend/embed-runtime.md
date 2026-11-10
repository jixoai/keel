# 嵌入 runtime

「嵌入」Keel 意味着在自己的进程与二进制里跑 Core，而不是使用现成的 `cmd/runtime` 或 `cmd/keel`。
本页说明什么是可行的、可见性规则允许什么，以及两个随仓交付的二进制各自的确切启动序列。

## 1. 可见性规则（先读这条）

运行时包全在 `internal/` 下：

```
internal/kernel     internal/agent      internal/loop      internal/agentcore
internal/store      internal/servicegraph  internal/loadout   internal/servicehost
internal/agentapi   internal/agentext   internal/agentloop  internal/provider
internal/host       internal/httpapi    internal/blob       internal/gateway …
```

Go 的 `internal/` 规则意味着这些包只能被模块 `github.com/jixoai/keel` 内的代码 import。**没有**
可供第三方 import 的公开 Go API，项目也不对 internal 包作版本承诺。实际选择：

1. **在本仓检出（或 fork）里工作**，新增自己的 `cmd/<你的二进制>`。这是预期路径——`cmd/runtime` 与
   `cmd/keel` 本身就是同一内核的两种装配。
2. **vendor/replace 该模块**做私有构建。代码是 Apache-2.0，合法；边界测试仍保证内核纯净，但升级路径
   归你自己。
3. **不嵌入进程，而是与它对话**。若你的需求是「我的应用驱动一个智能体」，受支持的契约是 HTTP 面——
   [AgentAPI](/zh/core/agentapi) 加 SSE。该契约**是**版本化的（`/v1`，只增不改），也是随仓客户端的用法。

1、2 见下；3 是「除运行时本身之外的一切」的推荐默认。

## 2. 两个随仓二进制各装配了什么

两个入口按同样顺序搭建同样的部件；`cmd/runtime` 是完整装配，`cmd/keel serve` 是其子集之上的单租户
产品壳。

`cmd/runtime` 启动序列（`cmd/runtime/main.go`）：

1. `store.Open(ctx, store.Config{...})` —— pgx 池（默认 24 连接，`internal/store/store.go:76`）。
2. `kernel.NewPG(ctx, st.Pool())` —— 应用基础 schema 与内核迁移（`cmd/runtime/main.go:105`、
   `internal/kernel/pg.go:68`）。
3. `kernel.New(kernelLog, kernel.Options{Audit, Limits, OnCommit})` —— 账本对象；`OnCommit` 在不阻塞
   写路径的前提下喂指标（`cmd/runtime/main.go:137`、`internal/kernel/kernel.go:591`）。
4. `servicehost.Boot(ctx, cfg)` —— 从租户文档解析默认 loadout，经服务图引擎应用，断言运行路径服务
   全部 `ACTIVE`，并启动回收过期代际的宿主 ticker（`cmd/runtime/main.go:394`、
   `internal/servicehost/host.go:242`）。
5. `httpapi.New(httpapi.Config{...})` —— HTTP/SSE 壳从**服务实例**取投影与写路径句柄，而不是从全局
   （`cmd/runtime/main.go:515`）。

`servicehost.Config` 就是嵌入面（`internal/servicehost/host.go:161`）：

| 字段 | 含义 |
|---|---|
| `Seams` | `kernel.Committer`、`kernel.Reader`、`kernel.LeaseManager`、`kernel.ExternalSessions`、`agent.RunMirror`、`*quota.Limits`、logger（`internal/servicehost/host.go:69`） |
| `Tenant` | 解析 loadout 所用的租户配置文档（`internal/host.TenantConfig`） |
| `Options` | loadout 快照头（`loadout.Options`：id、revision、version、summary、worker 开关、run 并发） |
| `RuntimeID` | 每次 claim 写入的 worker owner 身份（handoff 身份） |
| `Prompt` / `MCP` / `LLM` / `External` / `Product` | 宿主进程输入：prompt preamble/sections、已加载的 mcp.json 条目、provider 凭据查找、外部 sidecar 监管、产品装配钩子 |
| `Metrics`、`DrainGate`、`RetireTimeout`、`CollectInterval` | 可观测性、排水协调、代际回收 |

## 3. 配方 A —— 同一内核，你自己的进程壳

这是「新增 cmd」路径。下面骨架与 `cmd/runtime` 同构；它是骨架而不是可复制程序（真实代码必须处理
错误、信号与关闭）。

```go
// cmd/youragent/main.go  （本仓内）
package main

import (
    "context"
    "log/slog"

    "github.com/jixoai/keel/internal/kernel"
    "github.com/jixoai/keel/internal/loadout"
    "github.com/jixoai/keel/internal/quota"
    "github.com/jixoai/keel/internal/servicehost"
    "github.com/jixoai/keel/internal/store"
)

func main() {
    ctx := context.Background()
    log := slog.Default()

    // 1. 账本存储
    st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 24, MinConns: 8})
    if err != nil { panic(err) }
    defer st.Close()

    // 2. 内核（应用迁移；账本唯一写者）
    pgStore, err := kernel.NewPG(ctx, st.Pool())
    if err != nil { panic(err) }
    quotas := quota.New() // 实现 kernel.Limits
    kern := kernel.New(pgStore, kernel.Options{
        Audit:  servicehost.NewAuditSink(log, servicehost.AuditOptions{}),
        Limits: quotas,
    })

    // 3. 服务图：内核窄接缝是服务能看到的一切
    rt, err := servicehost.Boot(ctx, servicehost.Config{
        Seams: servicehost.Seams{
            Committer: kern, Reader: kern, Leases: kern,
            Runs:   yourRunMirror, // store 之上的 agent.RunMirror 适配
            Limits: quotas, Logger: log,
        },
        Tenant:    tenantDoc, // internal/host.TenantConfig
        Options:   loadout.Options{ID: "default", Revision: 1},
        RuntimeID: "youragent-1",
    })
    if err != nil { panic(err) } // 运行路径服务未 ACTIVE 即失败

    // 4. 你自己的 HTTP 面，或者完全不要
    select {}
}
```

关键点由代码而非约定保证：

- 运行路径上任一服务不是 `ACTIVE`，`servicehost.Boot` 就失败（`internal/servicehost/host.go:343`）；
  半装配的图永不启动。
- 你拿不到事件表。一切 durable 写入都走 `kern.Commit`。
- 可以在**不带 worker** 的情况下运行装配（`Options.Worker = false`，或 `cmd/runtime -no-worker`），
  只服务 API 与投影，另起 worker 舰队。

## 4. 配方 B —— 只要账本

只想要 durable 原语而不需要服务图，直接用 `internal/store` + `internal/kernel`。你必须遵守的契约：

- `user.message` 按 **edge 类**提交（不带 fence），其他 kind 按会话的 **owner** token 提交
  （claim → epoch → commit → finish/release）。
- 携带该 kind 声明的唯一幂等键（`requestId`/`generationId`/`toolCallId`）。
- 把租约当作唯一可变协调：`FOR UPDATE SKIP LOCKED` 领取、续租、用同一 token 提交、写 attempt 摘要并
  finish、release。

你等于要实现的那套 run-loop 语义，规格在[架构](/zh/core/architecture)与 `docs/KERNEL.md`；内核在提交时
校验载荷形状（[事件与协议](/zh/core/events) §8），因此手写写者无法用不可解码的载荷毒化日志。

## 5. 配方 C —— 在默认装配里加服务

加一个服务（而不是整个进程）是下一页的主题：[写一个服务描述符](/zh/core/extend/service-descriptor)。

## 6. 嵌入给不了什么

| 拿不到 | 原因 | 替代 |
|---|---|---|
| 稳定的公开 Go API | 运行时包是 `internal/`；不对它们作 semver 承诺 | 走 HTTP [AgentAPI](/zh/core/agentapi)：`/v1` + 能力探测 |
| 进程内热替换服务 | 装配被编译进二进制；热拔插旧 goroutine/监听器会让 2GB 边界不可预测 | [handoff](/zh/core/architecture) |
| 同一会话多写者 | 「一会话一写者」是恢复可证的前提 | 租约与 fencing（claim/epoch） |
| `keel serve` 的进程级多租户 | `serve` 是单租户壳（`KEEL_SERVE_TENANT`）；多租户运行时是 `cmd/runtime` | `cmd/runtime` + 每请求 `X-Tenant-ID` |
