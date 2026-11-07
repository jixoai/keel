> **当前状态（2026-11-08）**：`internal/kernel`（写/claim 路径）与
> `internal/servicegraph` + `internal/loadout`（默认装配）**已接入生产 `cmd/runtime`**：
> 启动时经服务图解析并激活默认 loadout（conformance 门在装配路径上），每个 run pin 一个
> generation/revision，run 元数据记录 loadout id/revision/epoch。装配与配置映射见
> `docs/LOADOUT.md`。
>
> **W4 handoff 已交付**：两种 drain 模式（shutdown/handoff）、drain 截止与超时接管、
> runtime_id 持久化与审计、五类安全点回归、双真实进程的 SSE 跨 handoff 续传（见 `docs/HANDOFF.md`）。
>
> **W5 外部网关已接进运行时**：`external-agent` / `external-tools` 是默认蓝图里的 loadout
> 服务，`internal/gateway` 负责传输/协议/隔离，`internal/externalgw` 负责会话、事件投影与
> 失败隔离，`cmd/keel-dsh-ref` + `scripts/external-gateway-smoke.sh` 是可复跑的参考
> （DSH 手测见 `docs/DSH-REFERENCE.md`）。
>
> 仍未交付：按租户热切换 loadout、sandbox/hooks 服务化、外部 sidecar 的 CPU/内存预算
> （按部署契约写在 `docs/DSH-REFERENCE.md`）。

# Keel Service Graph · 账本、工位与换班

> 本文是 `keel-service-graph` change 的架构说明。它回答一个问题：
> **哪些东西绝对不能换，哪些东西应该能换，换的时候怎么不丢会话。**
>
> 状态：目标架构；W2 默认装配、W4 handoff、W5 外部网关均已落生产运行时（见文首、
> `docs/LOADOUT.md`、`docs/HANDOFF.md` 与 `docs/DSH-REFERENCE.md`）。

## 1. 一句话

Keel 不是「把整个运行时都做成插件」，而是：

> **账本不能换，工位可以换；换工位靠换班，不靠热拔插。**

- **账本** = 契约内核：事件顺序、幂等、工具配对、lease/ownership、租户边界、崩溃恢复真值。
- **工位** = 服务图：agent loop、prompt、tools、LLM、会话投影、压缩、MCP、遥测、sandbox。
- **换班** = runtime handoff：新 runtime 用新 loadout 启动，旧 runtime 排水、交出租约，新 runtime 从账本继续。

## 2. 分层

```
客户端 / SSE
     │
     ▼
┌─────────────────────────────────────────────┐
│ 契约内核（不可插件化）                        │
│ event order · turn/step · tool pairing      │
│ idempotency · lease/epoch · tenant/quota    │
│ crash truth                                 │
└───────────────┬─────────────────────────────┘
                │ Commit / Claim / Renew / Finish
                ▼
        PostgreSQL 事件账本（唯一真值）
                ▲
                │ 服务只提交提案，内核负责落账
┌───────────────┴─────────────────────────────┐
│ 服务图（loadout，可替换）                     │
│ pi-loop · prompt · tools · llm              │
│ session-projection · compaction · mcp       │
│ telemetry · sandbox                         │
└─────────────────────────────────────────────┘
```

### 契约内核

内核不思考、不调模型、不执行工具，只做四件事：

1. **收单**：校验事件是否合法、是否重复、是否配对；
2. **记账**：在事务内分配 seq、追加事件、拒绝旧 owner；
3. **发钥匙**：claim 时给 owner + epoch，lease 过期可回收；
4. **守边界**：tenant、配额、事件种类、session 所有权。

服务不能直接写 `events` 表。它们把结果交给内核的 `Commit`，由内核决定能不能落账。
这是 Keel 与「一切皆插件」最大的区别。

### 服务图

每个服务声明：

- `provides`：我提供什么接口；
- `requires`：我必须依赖什么；
- `optional`：有则用、无则降级的依赖；
- `factory`：如何构造；
- `effects`：启动时注册了什么，卸载时按反序清理。

服务图只负责**装配和生命周期**，不实现 agent 逻辑。它不在 token 热路径上做解析；
loadout 在 run 开始时固定，不在每个 token 重新选择。

### Loadout

loadout 是不可变快照：服务集合 + 版本 + 配置摘要。每个 run 记录
`loadout_id` / `revision`；运行中换配置不影响在途 run，新 run 才使用新 revision。

默认 loadout 必须精确复现当前 `cmd/runtime` 的装配；这是所有重构的安全绳。

### Handoff

Go 服务是编译进二进制的，不做进程内热拔插。升级路径：

1. 新 runtime 以新 loadout 启动；
2. 旧 runtime 停止 claim；
3. 旧 runtime 排水、写完已提交事实；
4. 旧 runtime 释放 lease；
5. 新 runtime 用新 epoch 领取；
6. 从事件日志重建，SSE 从 `Last-Event-ID` 继续。

旧 writer 用旧 epoch 提交会被内核拒绝。用户看到的是会话继续，不是进程不变。

## 3. 不可撼动 vs 可替换

| 不可替换（契约内核） | 可替换（服务图） |
|---|---|
| 事件顺序、seq、一会话一写者 | agent loop（默认 `pi-loop`，需 conformance） |
| 工具调用 ↔ 结果配对 | prompt 组装 |
| requestId / generationId / toolCallId 幂等 | 工具注册与执行 |
| lease / ownership / fencing epoch | LLM provider |
| 租户隔离、配额执行 | 会话读取/投影（追加仍归内核） |
| 崩溃恢复真值 | 压缩、MCP、遥测、sandbox |
| 默认 loop 的 PI 行为契约 | 外部 AgentBackend / ToolProvider |

**边界规则：服务可以换，但服务不能改账本。**

## 4. 生态边界

### PI

默认 `pi-loop` 必须过 PI conformance；替换 loop 也必须过同一套测试。
PI 是行为契约的第一手证据，不是可随意重解释的参考实现。

### MCP

外部工具走 MCP 风格协议；不做第三套传输。MCP 工具是无状态 provider，
结果由 Keel 内核提交。

### DSH

- **不做逐插件 Cordis RPC**。Cordis 是进程内对象图，服务身份、同步
  `get/provide`、effect、事件总线无法无损跨网络。
- **整机 sidecar**：DSH runtime 作为 ExternalAgentBackend，DSH 插件零改动；
  Keel 只通过 agent 协议对话。
- **纯工具 bridge**：DSH `defineTool` 类插件通过 ToolProvider 暴露。
- **状态所有权**：DSH backend 可以拥有自己的会话状态；这类会话标记为
  `external_backend` / `state_owner=external`，不承诺 Keel 原生 kill-9 恢复/交接。
- UI/native/Electron/plugin manager/HMR 不能在服务端逐插件复刻。
- **参考实现**：`cmd/keel-dsh-ref`（自动化、含进程树回收测试）+ 
  `scripts/external-gateway-smoke.sh`；真实 DSH（Node + `@deepseek-ai/dsh@0.2.1-rc.5`）
  的 SDK 协议与 Keel 协议不同，需 DSH 侧桥，手测步骤/预期/排查见
  `docs/DSH-REFERENCE.md`。

### 外部网关（W5 运行时口径）

- **配置词汇唯一**：`host.TenantConfig.External`（`external.backends` /
  `external.tools` / `external.quota`）。boot 默认来自 `KEEL_EXTERNAL_CONFIG`
  （同一 TenantConfig JSON 片段）；loadout 只按文档开关服务，不另造字段。
- **服务**：`external-agent`（AgentBackend 连接/会话/事件桥，requires `ctx.store`）
  与 `external-tools`（ToolProvider 工具进 prompt registry，prompt 的 optional 依赖）。
  生命周期走服务图：effects 反序清理、generation、失败回滚；坏 provider 只降级，
  不拖垮 runtime（重连按 `gateway.RestartPolicy` 退避）。
- **外部会话**：`POST /v1/external/sessions`（open）、`GET .../{id}`（query）、
  `POST .../{id}/messages`（send）、`POST .../{id}/cancel`、`POST .../{id}/close`、
  `GET /v1/external/backends`；租户头与原生 session API 相同。
- **审计/账本边界**：外部会话经 `kernel.ExternalStore` 落 `state_owner=external` 等标记，
  绝不进入原生 run/lease 流；backend 事件由运行时经内核 **external 提交类**落成
  `external.event`（payload 带 `backend/externalRef/stateOwner/direction/...`），
  普通事件/SSE 端点只回放这些 Keel 已记录的事件。原生 edge 写入在会话事务内被
  对称拒绝（`NativeEdge`），`external.event` 也不进 edge 白名单。
- **计量**：`metering=external|none` 来自 backend 声明；`external.quota.unmetered`
  必须显式选择（allow 记录 none 放行 / deny 拒绝打开），绝不静默记 0。
- **安全**：stdio provider 只拿 `MinimalEnv` + 显式声明的变量（`${NAME}`/`env:`/
  `secret:` 引用解析，字面密钥拒绝）；调用审计只记 provider/method/target/耗时/
  状态；`Close` 走关 stdin → 宽限 → 进程组 SIGTERM/SIGKILL，冒烟断言孙进程 0 残留。

## 5. 服务生命周期

```
PENDING ──依赖齐──▶ LOADING ──成功──▶ ACTIVE
   ▲                    │                │
   │                    └──失败──▶ FAILED│
   └────────依赖缺失────────── UNLOADING ◀┘
```

- 必需依赖缺失：`PENDING`，不是启动失败；诊断说明“在等谁”。
- 可选依赖缺失：可 `ACTIVE`，诊断记录降级。
- 启动失败：回滚已产生的 effects，不留半注册工具/监听器。
- 替换 provider：dependents 先 `UNLOADING`，provider 后卸载；新 provider 激活后重载。
- 每次重装配 generation+1；旧 effect 不得泄漏到新 generation。

## 6. 完成定义

- 默认 loadout 与旧行为等价；
- 五个 spec 的全部 Scenario 有测试；
- PI conformance 不退化；
- handoff 五类安全点：0 丢事件、0 重复最终回复、SSE 游标连续；
- 外部 gateway：慢/坏/崩/超时都不拖垮 run（W5 已接运行时；`internal/externalgw`
  单测覆盖 open/send/投影/崩溃/超时/版本/内存边界）；
- DSH sidecar/tool provider 参考适配器有真实测试（`cmd/keel-dsh-ref` +
  `internal/dshref` + `scripts/external-gateway-smoke.sh`；真实 DSH 手测见
  `docs/DSH-REFERENCE.md`）；
- 2GB / 1000 会话基线不退化；
- 子代理对抗审查通过，HTML 报告与公网链接可复现。

## 7. P0 实现约束（审查后）

- **租约线性化点**：session 行是唯一权威。Claim/Renew/Commit/Finish 在同一 session 行锁下
  重读 owner+epoch；统一锁序 sessions → runs；Renew 条件更新 owner+epoch。
- **提交分类**：owner-scoped（assistant/tool.result/compaction/run）带 epoch；
  edge-scoped（user.message）只校验 tenant/session/白名单/幂等；handoff drain 期间仍可落账
  （SIGTERM shutdown drain 保留现有拒绝新写语义，两者必须区分）。
- **API 收紧**：`POST /v1/sessions/{id}/events` 只收 user.message，其他 400；
  跨租户/不存在 404。这是显式 MODIFIED API 行为。`GET /v1/sessions/{id}/events` 与
  `GET .../events/stream` 同一口径（SSE 在建立流前判定，不再 200 空流）；
  `POST /v1/sessions/{id}/run` 对缺失/跨租户会话 404，且不为它创建 runs 行。
- **requestId 幂等作用域**：`(tenant, session, requestId)`——与事件日志的 dedupe 作用域
  一致；跨会话复用同一 requestId 是两次独立提交，绝不会返回别家会话的 submission/seq。
- **RETIRING**：旧 generation 不接新 run，但在途 run 仍可引用；引用归零/截止后反序卸载。
- **外部会话**：`state_owner=external`、`external_backend`、`external_ref`、
  `external_status`；不进原生 run/lease 作业流；SSE 只回放 Keel 记录；usage 标记
  `metering=external|none`。原生 edge 提交带 `NativeEdge` 标记，内核在 `BeginSession`
  的 session 行锁内读会话类别（`ExternalState`），external → `ErrExternalSession`
  再 append（httpapi 409，绝不落账；handler 的 `SessionOwner` 预检只是快速失败）；
  外部网关车道（`ClassExternal`/`external.event`）是对称判定：只接受 external 且
  未关闭的会话。`/run` 由入队守卫拒绝（409，不建 runs 行）；入队对 session 行
  加锁并与提升事务串行化，claim 谓词按 `state_owner <> 'external'` fail-closed 过滤
  （既有脏 run 行也不会被领取）。**提交类对称判定**（W5）：`CommitRequest.NativeEdge`
  只由原生 `/input`、`/events` 置位，内核在 `BeginSession` 持 session 行锁后判定
  `ClassEdge+NativeEdge` 必须非 external、`ClassExternal`（`external.event`）必须
  external；错误在 append 之前返回（HTTP 409），不依赖 HTTP 侧预检的正确性。
- **AgentBackend**：复用 MCP 传输/帧，但方法集是版本化私有协议，不是 MCP 标准方法。
