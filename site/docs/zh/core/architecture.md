# 架构

本页是 Core 的结构地图：账本、其外的内核、服务图、loadout、handoff 与背压。内容依据
`docs/SERVICE-GRAPH.md` 与 `docs/KERNEL.md`，并给仓库用测试钉住的部分附上代码锚点。

## 一条规则

> **账本不能换，工位可以换；换工位靠换班，不靠热拔插。**
>
> —— `docs/SERVICE-GRAPH.md:30`

下面的一切都是这句话的推论。

```
客户端 / SSE
     │
     ▼
┌──────────────────────────────────────────────────────┐
│ 契约内核（不可插件化）                                 │
│ 事件顺序 · turn/step · 工具配对                        │
│ 幂等 · lease/epoch · 租户/配额 · 崩溃恢复真值          │
└───────────────┬──────────────────────────────────────┘
                │ Commit / Claim / Renew / Finish
                ▼
        PostgreSQL 事件账本（唯一真值）
                ▲
                │ 服务只提交提案；内核决定落不落账
┌───────────────┴──────────────────────────────────────┐
│ 服务图（loadout，可替换）                              │
│ pi-loop · prompt · tools · llm · session-projection   │
│ compaction · mcp · telemetry · sandbox                │
└──────────────────────────────────────────────────────┘
```

## 1. 账本：每个会话一条 append-only 事件流

会话状态就是严格有序的事件序列。`sessions.next_seq` 在追加事件的同一事务里分配下一个 `seq`，因此
`(tenant_id, session_id, seq)` 是主键，顺序属于日志本身，而不是时间戳
（`internal/store/schema.sql:52`）。

两个推论贯穿整个设计：

- **checkpoint 与转录都是投影**。`checkpoints` 表存 `upto_seq` 之内的摘要；转录重建
  （`internal/agent/messages.go`）是事件日志的函数。二者都不允许成为第二真值源。
- **恢复即重放**。新进程不需要任何内存态延续；它读日志后继续。唯一仍在的协调状态是租约行（见下）。

账本同时驱动在线投递：`AFTER INSERT` 触发器发 `pg_notify('keel_events', …)`，SSE 订阅者因此是**被唤醒**
而不是轮询（`internal/store/schema.sql:207`）。丢一次通知的代价是一个心跳周期，永远不会丢数据。

## 2. 内核：四件事，别无其他

内核不思考、不调模型、不执行工具，只做四件事（`docs/SERVICE-GRAPH.md:61`）：

1. **收单**：校验事件是否合法、是否重复、是否配对；
2. **记账**：在事务内分配 `seq`、追加事件、拒绝旧 owner；
3. **发钥匙**：claim 时给 owner + epoch，租约过期可回收；
4. **守边界**：租户、配额、事件种类、会话所有权。

服务不能写 `events` 表。它们把结果交给内核的 `Commit`，由内核决定能不能成为事实。包边界由机器检查：
`internal/architecture/boundary_test.go:14` 列出内核包（`internal/loop`、`internal/agentcore`、
`internal/agenttool`、`internal/kernel`、`internal/agent`、`internal/store`），断言它们永不 import 产品包。

| 包 | 职责 | 不做什么 |
|---|---|---|
| `internal/agentcore` | 消息/内容/事件类型（PI 的 message & event 语义） | 不读日志、不调用模型 |
| `internal/agenttool` | 工具执行与批次（并行调用、结果配对） | 不认识具体工具的业务含义 |
| `internal/loop` | 一次 loop 执行：turn、tool batch、三个 drain 点、compaction、settle | 不写账本 |
| `internal/agentloop` | 可替换的 loop 契约 + PI conformance manifest | 不绑定某个具体实现 |
| `internal/kernel` | 账本：提交/去重、lease/fencing、run 行、外部会话通道、投递模式与运行摘要校验 | 不做业务决策（谁该跑、跑什么） |
| `internal/agent` | runner（rebuild → 填洞 → loop → 提交）＋ worker（claim/renew/settle） | 不做认证、列表、审批、插件 |

服务拿到的是**窄接缝**，不是 store 句柄：`kernel.Committer`、`kernel.Reader`、
`kernel.LeaseManager`、`kernel.ExternalSessions`（`internal/kernel/seams.go`），以及
`agent.Assembly`、`agent.RunMirror`、`agent.RunBinding` 这类按服务定义的接口。任何服务都拿不到
事件表写句柄、`seq` 分配或绕过租户/租约校验的旁路（`docs/LOADOUT.md:147`）。

## 3. 服务图：能力、描述符、生命周期

服务图是可替换的那一半。每个服务由 `ServiceDescriptor` 静态描述
（`internal/servicegraph/descriptor.go:26`）：

| 字段 | 含义 |
|---|---|
| `Name` | loadout 内唯一（`"pi-loop"`） |
| `Version` | 实现身份——实现变了就必须升 |
| `Provides` / `Requires` / `Optional` | 能力名（`ctx.llm`）；缺必需依赖 → `PENDING`，缺可选依赖 → `ACTIVE` 并记录降级诊断 |
| `Role` | 在能力提供链里是 `primary` 还是 `fallback` |
| `Effects` | 启动时注册的清理项名字（监听器、定时器等），给运维与诊断看 |
| `Factory` | `func(ctx, servicegraph.Resolved) (servicegraph.Instance, error)`（`internal/servicegraph/lifecycle.go:49`），由 loadout 构建器挂上 |

生命周期（`docs/SERVICE-GRAPH.md:170`）：

```
PENDING ──依赖齐──▶ LOADING ──成功──▶ ACTIVE
   ▲                    │                │
   │                    └──失败──▶ FAILED │
   └────依赖缺失────────── UNLOADING ◀── RETIRING ─┘
```

测试钉住的规则：

- 缺**必需**依赖是 `PENDING`，不是启动失败；诊断会说明「在等谁」。缺**可选**依赖仍可 `ACTIVE`，
  只记录降级诊断。`sandbox` 声明了能力但**完全没有工厂**：依赖方按降级运行，而不是阻塞在一个
  不可能存在的服务上（`docs/LOADOUT.md:48`）。
- 启动失败只回滚该服务注册过的 effects；已 ACTIVE 的独立服务不受影响。
- 替换 provider 时先卸载 dependents，再卸载 provider；每次重装配 `generation`+1，旧 effect 不得
  泄漏到新 generation。
- 重装配身份是 `(Name, Version, Provides, Requires, Optional, Role)`。若配置变了但服务没变，
  实例**跨代共享**，不重建。

## 4. Loadout：不可变快照，按 run pin

loadout 是 `{id, revision, version, summary, 服务描述符集合}` 的不可变快照，由租户配置文档
（`internal/host.TenantConfig`）解析而来。没有第二套配置词汇：`internal/loadout/tenant.go` 只做映射，
不定义自己的文档（`docs/LOADOUT.md:51`）。

默认蓝图是 `loadout.DefaultServices()`（`internal/loadout/loadout.go:83`）：

| 服务 | provides | requires | optional | 运行时实现 | 默认 |
|---|---|---|---|---|---|
| `store` | `ctx.store` | — | — | 只读 run 镜像 `agent.RunMirror` | 恒开 |
| `llm` | `ctx.llm` | — | — | `provider.ResolveProvider` → `StreamFn` | 恒开（空模型 = fake） |
| `prompt` | `ctx.prompt` | — | `ctx.mcp`、`ctx.sandbox`、`ctx.external-tools` | `host.New`（prompt/skills/registry） | 恒开 |
| `mcp` | `ctx.mcp` | — | — | `mcp.Manager`（连接/重连/关闭） | 配了 mcp.json 才开 |
| `sandbox` | `ctx.sandbox` | — | — | **无实现**：不提供工厂 | 从不开（降级诊断） |
| `tools` | `ctx.tools` | `ctx.prompt` | — | 同一 `host.Host` 的 registry 视图 | 恒开 |
| `pi-loop` | `ctx.loop` | `ctx.llm`、`ctx.tools` | `ctx.compaction` | `agent.Runner`（loop + 内核提交路径） | 恒开 |
| `compaction` | `ctx.compaction` | `ctx.llm` | `ctx.store` | `agent.CompactionOptions` | `compaction.enabled=true` 才开 |
| `session-projection` | `ctx.session-projection` | `ctx.store` | — | `kernel.Reader`（租户边界读） | 恒开 |
| `telemetry` | `ctx.telemetry` | — | — | `telemetry.Memory` | `telemetry.adapter` 非空才开 |
| `worker` | `ctx.worker` | `ctx.store`、`ctx.loop`、`ctx.prompt`、`ctx.tools` | `ctx.telemetry` | `agent.Worker` claim 循环 | `-no-worker` 关闭 |
| `external-tools` | `ctx.external-tools` | — | — | `externalgw.ToolService` | `external.tools` 非空才开 |
| `external-agent` | `ctx.external-agent` | `ctx.store` | — | `externalgw.AgentService` | `external.backends` 非空才开 |

两个刻意的结构（`docs/LOADOUT.md:42`）：

- **`prompt` 与 `tools` 共享同一个 host 对象**。技能、内置工具、MCP 工具、外部 ToolProvider 工具
  都注册在 `host.Host` 的 registry 里；`tools` 是它的视图，不是第二份 registry（否则 `RefreshMCP`
  会让两份注册表分叉）。因此 `tools` requires `prompt`，`external-tools` 是 `prompt` 的 optional 依赖。
- **`sandbox` 不造空工厂**。未实现的服务只是蓝图里的 optional 提供者；依赖方记录降级诊断，不会半启动。

**运行期 pin**。每个被 claim 的 run 调 `Engine.Acquire()`，得到带 generation/revision 的 lease；
`pi-loop` 从该 lease 解析（不是全局 Lookup），`FinishRequest.Loadout` 把
`loadout_id`/`revision`/`generation`/`owner_epoch` 写进 run 行
（`internal/kernel/schema/0005-loadout-revision.sql`、`docs/LOADOUT.md:99`）。只读查询
`GET /v1/sessions/{id}/run/loadout` 返回该 pin。

**Reload**。`Runtime.Reload` 应用新文档：新 run 用新 generation；在途 run 继续用旧实例；旧代进入
`RETIRING`（不接新 run），引用归零或 drain 截止后按反序卸载。截止后 `CollectExpired` 强制卸载，并把
仍持有旧代的 lease 标为 broken——持有者必须让 run 以 `drain-expired` 诊断失败
（`internal/servicehost/host.go:691`、`docs/LOADOUT.md:109`）。宿主 ticker 周期调用
`CollectExpired`，空闲进程也不会永远留着一代旧 effects（`docs/LOADOUT.md:114`）。

**Conformance 门（fail-closed）**。任何提供 `ctx.loop` 的服务都必须给出 PI conformance manifest，
其规范哈希与该服务的 pin 相等；未声明、未 pin 或哈希不符的 loop 在 loadout 存在之前就被拒绝
（`internal/loadout/conformance.go:55`）。默认 `pi-loop` pin 住 `PinnedPiLoopManifestHash`；
改测试套件必须显式重新 pin 并升 loop 服务版本。

## 5. Handoff：工位怎么换

Go 服务编译进二进制——没有进程内热拔插。升级原语是换班，步骤定义在 `docs/HANDOFF.md:75`：

1. 新 runtime 以新 loadout 启动并达到 `ready`；
2. 旧 runtime 进入 **handoff drain**：停止 claim 新 run，但继续接收 edge 输入（`user.message`
   照常落账并唤醒 run）；
3. 在途 run 排到安全点：已提交事实保留，未提交 partial（半条消息、执行中的工具）在截止时丢弃，
   释放 lease；
4. 新 runtime 以 `epoch+1` claim，从账本重建；
5. SSE 客户端用 `Last-Event-ID` 重连，首帧 = 游标+1。

五类安全点（`docs/HANDOFF.md:100`）与不变量（`docs/HANDOFF.md:110`）就是验收标准：用户消息已提交 →
从该 seq 继续；assistant tool call 已提交但结果缺失 → 重执行工具（at-least-once，最终恰好一条结果）；
工具结果已提交 → 继续而不重执行；模型流中 → 丢弃 partial，不出现半条消息；工具执行中 → 按幂等键重试
直至收敛。不变量：0 丢事件、0 重复最终回复（同一 `generationId` 只落一条 assistant）、0 工具双结果、
SSE 游标单调、每个 run 最终收敛到 `done` 或 `failed`。

fencing 是安全的前提：旧 owner 的 `Commit` 携带它 claim 时的 epoch；新 owner claim 后 epoch 更高，
旧 epoch 的提交一律被拒。被拒的 owner 必须停止重试并退出——不得把本地结果重新包装成「新提交」
（`docs/HANDOFF.md:93`）。内核在会话行锁下重读 owner+epoch（线性化点）来拒绝陈旧写者
（`docs/SERVICE-GRAPH.md:199`）。

**升级纪律**：先 drain 旧进程，再启动新二进制。旧二进制不走 fence（`store.AppendEvent` 不看 epoch），
因此「新 owner 持租期间旧写者仍能补账」是真实窗口。SIGTERM 的 shutdown drain 与 handoff drain 不同：
shutdown 保留既有语义（拒绝新写）并**放弃**在途 run（不 Finish、不 release），其租约到期后新进程才能
接管，因此换二进制时新进程最多等一个有界 TTL（`docs/HANDOFF.md:24`）。

## 6. 背压：带宽是显式上限

三种机制叠加，各管一层（`docs/BACKPRESSURE.md:16`）：

| 机制 | 反压来源 | 管住 | 管不住 | 成本 |
|---|---|---|---|---|
| (a) 阻塞 `Write`（`io.Copy`） | 内核 sndbuf + 对端接收窗 | 单流驻留内存（天然有界） | 总带宽、公平性、死连接 | 0 |
| (b) 全局/租户 token bucket | 用户态记账 | 总带宽上限、公平、可观测排队 | 单流慢客户端 | 每块一次 `WaitN` |
| (c) `SetWriteDeadline` | 超时 | 死/僵客户端、goroutine 泄漏 | 带宽总量 | 每次 Write 一次 syscall |

数据通路的推荐顺序是 **(b) bucket → (c) deadline → (a) Write**；桶按 base64 之后的线上字节计量，
burst 必须 ≥ 最大单次 Write（64KiB 块 → 87,384 字节，故 burst 176KiB、rate ≈ 链路 × 0.88）。

**按尺寸分池**（`docs/BACKPRESSURE.md:57`）：三类——Small <256KiB、Medium <2MiB、Large ≥2MiB——
软预约 640/300/60，硬上限 900/400/80。准入条件是该类未超硬上限，**且**为其他类预留未满软额之后仍有余量。
这样既不饥饿（单优先级队列 + SJF 会饿死大请求），也不浪费（本类未满软额时可吃满硬上限）。队列上限 256，
溢出立即 **429 + `Retry-After`**。

**流式管线**：blob 按 64KiB 分块读取，base64 整块编码（4 个 base64 字符 ≡ 3 个源字节，这正是任意
wire offset 可续传的原因），再 `io.Copy` 写出；每流预算约 227KiB（明文）/ 245KiB（TLS），因此 1000 条
活跃流约 390MB 用户态 + 约 128MB 内核 socket 缓冲（`docs/BACKPRESSURE.md:108`）。细节、PostgreSQL
查询纪律（keyset 批取、不长事务游标）与实测数字见[存储与运行](/zh/core/storage)。

## 7. 什么可替换，什么不可

| 不可替换（契约内核） | 可替换（服务图） |
|---|---|
| 事件顺序、`seq`、一会话一写者 | agent loop（默认 `pi-loop`，须过 conformance） |
| 工具调用 ↔ 结果配对 | prompt 组装 |
| `requestId` / `generationId` / `toolCallId` 幂等 | 工具注册与执行 |
| lease / ownership / fencing epoch | LLM provider |
| 租户隔离、配额执行 | 会话读取/投影（追加仍归内核） |
| 崩溃恢复真值 | 压缩、MCP、遥测、sandbox |
| 默认 loop 的 PI 行为契约 | 外部 AgentBackend / ToolProvider |

**边界规则：服务可以换，但服务不能改账本。**
