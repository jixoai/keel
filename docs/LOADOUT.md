# Keel Loadout（装载单）

> 变更来源：`openspec/changes/archive/keel-service-graph`（W2 服务图、loadout 与默认装配）。
> 本文回答三个问题：**默认运行时由哪些服务组成、配置从哪里来、换配置/换代时在途 run 怎么不被掀桌。**

## 1. 一句话

loadout 是**不可变快照**：`{id, revision, version, summary, 服务描述符集合}`。
它由**租户配置文档**（`internal/host.TenantConfig`，即管理员 API 写入的那份）
解析而来；`cmd/runtime` 启动时经服务图装配默认 loadout，run 在领取时**pin 一个
revision + generation**，之后配置变更只影响下一个 run。

```
host.TenantConfig ──resolve──▶ Loadout(revision, summary) ──Apply──▶ Engine(generation)
        │                                                              │
        └ 唯一配置词汇（admin PUT /v1/tenants/{id}/config）             ├─ 服务实例（工厂构建）
                                                                       └─ 每 run Lease：pin generation + revision
```

没有第二套配置词汇：`internal/loadout` 只做映射，不定义自己的 `TenantConfig`。

## 2. 默认蓝图

`internal/loadout.DefaultServices()`（声明顺序即装配顺序的依据）：

| 服务 | provides | requires | optional | 运行时实现 | 默认 |
|---|---|---|---|---|---|
| `store` | `ctx.store` | — | — | 只读 run 镜像 `agent.RunMirror` | 恒开 |
| `llm` | `ctx.llm` | — | — | `provider.ResolveProvider` → StreamFn | 恒开（空模型 = fake） |
| `prompt` | `ctx.prompt` | — | `ctx.mcp`, `ctx.sandbox`, `ctx.external-tools` | `host.New`（prompt/skills/registry） | 恒开 |
| `mcp` | `ctx.mcp` | — | — | `mcp.Manager`（连接/重连/关闭） | 配了 mcp.json 才开 |
| `sandbox` | `ctx.sandbox` | — | — | **无实现**：不提供工厂 | 从不开（降级诊断） |
| `tools` | `ctx.tools` | `ctx.prompt` | — | 同一 `host.Host` 的 registry 视图 | 恒开 |
| `pi-loop` | `ctx.loop` | `ctx.llm`, `ctx.tools` | `ctx.compaction` | `agent.Runner`（loop + 内核提交路径） | 恒开 |
| `compaction` | `ctx.compaction` | `ctx.llm` | `ctx.store` | `agent.CompactionOptions`（阈值/保留量） | `compaction.enabled=true` 才开 |
| `session-projection` | `ctx.session-projection` | `ctx.store` | — | `kernel.Reader`（租户边界读） | 恒开 |
| `telemetry` | `ctx.telemetry` | — | — | `telemetry.Memory` | `telemetry.adapter` 非空才开 |
| `worker` | `ctx.worker` | `ctx.store`, `ctx.loop`, `ctx.prompt`, `ctx.tools` | `ctx.telemetry` | `agent.Worker` claim 循环 | 进程级 `-no-worker` 关闭 |
| `external-tools` | `ctx.external-tools` | — | — | `externalgw.ToolService`（ToolProvider 连接 + `ext__` 工具适配） | `external.tools` 非空才开 |
| `external-agent` | `ctx.external-agent` | `ctx.store` | — | `externalgw.AgentService`（AgentBackend 连接、外部会话、事件投影） | `external.backends` 非空才开 |

两个刻意的结构：

- **prompt 与 tools 共享同一个 host 对象**。技能、内置工具、MCP 工具、外部
  ToolProvider 工具都注册在 `host.Host` 的 registry 里；`tools` 是它的视图，不是
  第二份 registry（否则 `RefreshSections` 会让两份注册表分叉）。因此 `tools` requires
  `prompt`，`external-tools` 是 prompt 的 optional 依赖。
- **sandbox 不造空工厂**。未实现的服务只是蓝图里的 optional 提供者；没有工厂时
  依赖方按 optional 缺失记录降级诊断，不会半启动。

## 3. 租户文档 → 服务集合（单源 adapter）

`internal/loadout/tenant.go`：

| 文档字段 | 作用 |
|---|---|
| `agent.provider/model/baseUrl/protocol/apiKeyRef` | `llm` 的 provider 选择（`apiKeyRef` 只存引用） |
| `agent.systemPrompt/sections` | preamble 与额外 prompt section |
| `agent.maxTurns/contextWindow` | run 回合上限 / compaction 窗口 |
| `skills.enabled/roots/fileReadTool` | 技能发现与索引措辞 |
| `mcp.servers` | 非空即启用 `mcp` 服务 |
| `compaction.enabled/threshold/keepRecentTokens` | `enabled=true` 才启用 `compaction` |
| `telemetry.adapter` | 非空即启用 `telemetry` |
| `tools.allow` | 工具白名单（摘要计数；过滤由 host 装配消费） |
| `hooks.commands` | 摘要计数（hook 装配仍走 host/loop 扩展点） |
| `external.backends` | 非空即启用 `external-agent`（AgentBackend 连接、外部会话） |
| `external.tools` | 非空即启用 `external-tools`（ToolProvider 工具进 prompt registry） |
| `external.quota.unmetered` | 外部会话计量策略的**显式选择**：`allow`（记录 `metering=none` 放行）/ `deny`（拒绝打开）；声明了 backends 时必填 |
| `external.quota.maxSessionsPerBackend` | 每个 backend 的并发会话上限（与 provider 自己的 `maxSessions` 取更小值） |
| `quota.runConcurrency/eventsPerMinute` | 契约内核的 per-tenant 准入（见 §6） |

进程级、不属于租户文档的输入放 `loadout.Options`：`ID` / `Revision` / `Version` /
`Worker`（`-no-worker`）/ `RunConcurrency`（`-runs`）。它们是“这个进程怎么跑”，
不是“租户要什么”。

`Summary(cfg, opts)` 是确定性摘要：只含计数/标志，**不含 secret 值或 hook 命令**，
随 loadout 快照记录，handoff/审计可直接对比。

## 4. 装配与生命周期

`cmd/runtime` 启动：

1. 打开 store、应用内核迁移；
2. 构造服务 seam：`Committer`/`Reader`/`LeaseManager`（契约内核）、只读 run 镜像、
   配额 seam、审计 sink；
3. 从环境变量构造**默认租户文档**（model/prompt/skills/mcp/telemetry/quota）；
4. `servicehost.Boot`：`loadout.FromTenantConfig` → `Engine.Apply` → 断言运行路径
   服务全部 ACTIVE → 启动宿主 ticker；
5. HTTP 面从服务实例取 `session-projection`（读 seam）与 `pi-loop`/worker（写路径）。

生命周期：`PENDING → LOADING → ACTIVE → RETIRING → UNLOADING`，失败 `FAILED`；
effects 反序释放；启动失败只回滚该服务的 effects，不影响已 ACTIVE 的独立服务。
每次 `Apply` 令 generation+1；服务的装配身份是
`(name, version, provides, requires, optional, role)` —— 配置变了但服务没变，
实例**跨代共享**，不重建。

### 运行期 pin

每个 claim 到的 run 在 `agent.Worker` 里 `Engine.Acquire()`：
- 得到该 run 的 `servicegraph.Lease`（generation/revision），
- `pi-loop` 实例从 lease 解析（不是全局 Lookup），
- run 结束（Finish）时 `FinishRequest.Loadout` 写入 run 行，
- `Release` 归还引用；旧代的引用归零后按反序卸载。

### 换 loadout（Reload）

`Runtime.Reload` 解析新文档并 `Apply`：新 run 用新 generation；在途 run 继续用旧
实例，旧代进入 `RETIRING`（不再接新 run），引用归零或 drain 截止后反序卸载。
`WithRetireTimeout` 截止后 `CollectExpired` 强制卸载，并把仍持有旧代的 lease 标为
broken —— 持有者必须让 run 失败（诊断 `drain-expired`）。

### 空闲收尾

`Apply`/`Acquire` 之外的 RETIRING 不会自己消失，因此宿主起一个 ticker
（`servicehost` 的 collector）周期调用 `Engine.CollectExpired`；空闲进程也不会
永远留着一代旧 effects。ticker 由 `Runtime.Close` 停止。

## 5. run 元数据与只读查询

迁移 `internal/kernel/schema/0005-loadout-revision.sql`（合并 W4 后编号顺延：0003/0004 已被 handoff 占用）：`runs` 增加
`loadout_id` / `loadout_revision` / `loadout_generation` / `loadout_owner_epoch`。
Finish 事务（持 session 行锁）内写入，与 run 结算同源。

只读查询：`GET /v1/sessions/{id}/run/loadout`（租户头与 `/run` 相同）：

```json
{"loadoutId":"default","revision":1,"generation":2,"ownerEpoch":7}
```

独立端点而非扩展 `/run` 响应：**不改变既有响应形状**（R1 差分白名单外零差异）。
未记录过 loadout 的旧 run 返回零值；无 run 行/跨租户 404。

## 6. 准入配额与审计

- **配额**：`internal/quota` 实现 `kernel.Limits`。默认值来自宿主文档
  （`KEEL_QUOTA_RUNS` / `KEEL_QUOTA_EVENTS_PER_MINUTE`）；若配置了
  `DocumentSource`（运行时接的是 `tenants.config`），则按租户读取存储文档
  （短 TTL 缓存）并合并覆盖默认值；读/解析失败 **fail-closed**（拒绝准入）。
  - run 并发：claim 成功预留一个 slot；Finish/Release 归还。
  - 事件速率：per-tenant token bucket（events/min），所有 session 共享。
  - 计数器是进程本地容量护栏，不是账本；持久用量在 usage ledger。
- **审计**：`servicehost.NewAuditSink` 把内核拒绝（stale-epoch/lease-lost/quota/
  store-failure…）投影成结构化日志：`reason/op/tenant/session/kind/owner/epoch/detail`。
  token 只拆成 owner+epoch，不落全量；同一 `(tenant, op, reason)` 按窗口限频并记录
  `suppressed` 计数，避免风暴。

## 7. 窄接口纪律

服务只拿到：

- `kernel.Committer` / `kernel.Reader` / `kernel.LeaseManager` /
  `kernel.ExternalSessions`（`internal/kernel/seams.go`）；
- 各自窄接口：`agent.Assembly`（prompt+tools）、`agent.RunMirror`（只读 run 行）、
  `LLMService`、`agent.RunBinding`/`BoundRun`（per-run pin）；
- 服务图绑定：capability → 实例，由 `BindAs` 断言成本地窄接口。

`store` 服务的绑定值是 `agent.RunMirror` 的包装，**不是** `*store.Store`；
任何服务都拿不到事件表写句柄、seq 分配或租户/lease 校验的旁路。`externalgw`
只拿 `kernel.Committer` + `kernel.ExternalSessions` + process 输入（密钥解析
lookup、基础 environ），其 import 边界由 `internal/externalgw/boundary_test.go`
机器门约束（不得 import store/kernel 原始写路径/durable）。

## 8. 验收

- **R1 差分**：默认 loadout 与 W0 基线二进制同输入、同库，规范化后白名单外 0 差异
  （`cmd/r1diff`，白名单 `openspec/.../r1-allow.json`）。
- **e2e 7/7**：`cmd/e2e`（重启/kill -9/SSE 游标/blob 续传）。
- **单测**：loadout 映射与摘要；服务图生命周期/回滚/反序/代际；servicehost
  装配与旧装配对表、Reload 共享与 revision pin、空闲 collector、审计限频；
  quota 的限流/释放/多租户/竞态；运行元数据（mem + PG）。
- **外部网关（W5）**：`internal/externalgw` 的服务单测（打开/查询/发送/取消/关闭、
  `external.event` 投影、metering 策略、崩溃→backend-unavailable、超时、版本不兼容、
  env 清洗、进程树回收）；`scripts/external-gateway-smoke.sh` 起真实 runtime +
  参考 sidecar 做 15 步冒烟并断言 0 残留。

## 9. 边界（未在 W2 交付）

- 按租户**热切换 loadout**（每个 session 解析自己的文档并 Apply）属于 W4 作用域/
  handoff；W2 交付的是默认文档装配 + Reload 机制 + 在途 pin。
- `sandbox` 服务、`hooks` 的服务化适配器未实现：蓝图为 optional/无工厂，或仍走
  host/loop 扩展点。
- 跨进程换代仍走 runtime handoff（新进程、drain、epoch+1），loadout engine 只负责
  进程内 generation。
