# Proposal: keel-service-graph

> 立项：2026-11-07。Owner 指令：把 Keel 从「能跑的无状态运行时」推进为
> 「可长期演进、可换零件、可交接、可接入 PI/DSH/MCP 生态」的 Agent 运行时底座。
> **本 change 不按 alpha/beta 分阶段对外交付；所有工作流完成后只有一个完成态。**
> 内部按依赖并行，不设阶段性停止点。

## Why

Keel 当前已经证明了几件最难的事：事件日志是唯一真值、SIGKILL 后可恢复、
1000 个带图片的 SSE 会话在 2GB 内、PI 行为契约全量 conformance 全绿（冻结基线 593 项；收口时实测 634 PASS / 0 FAIL / 1 SKIP）。
但这些能力仍然焊在 `cmd/runtime`、`agent.Runner`、`host.New` 的构造函数里：

- 换一个 agent loop、prompt 组装、tool 执行器或 LLM adapter，要改启动代码；
- 插件没有统一的「我提供什么 / 我需要什么 / 何时卸载」契约；
- 升级只能换二进制，没有正式的会话交接协议；
- 外部能力（MCP、DSH、其他 Agent 运行时）只能以临时适配器接入，
  没有一条明确的「生态兼容边界」。

如果继续把每个能力直接写进启动函数，Keel 会变成第二个难以替换的宿主；
如果反过来「把一切都做成插件」，又会破坏 Keel 最值钱的东西——账本、租约、
幂等、租户隔离和崩溃恢复。本 change 的职责就是把这条线划清。

## 我们要什么

### 不可撼动（契约内核）

- 事件顺序、seq 单调、一会话一个写者；
- 工具调用 ↔ 结果配对，未答调用不得静默消失；
- `requestId` / `generationId` / `toolCallId` 幂等；
- lease / ownership / fencing token，交接后旧 writer 不能继续提交；
- 租户隔离与配额执行；
- 崩溃恢复以事件日志为唯一真值，checkpoint 只是投影；
- 默认 agent loop 的 PI 行为契约（conformance 是门槛，不是建议）。

### 可替换（服务图）

- agent loop（默认 `pi-loop`，替换必须过 conformance）；
- prompt 组装、工具注册/执行、LLM adapter、会话投影/读取、压缩、MCP、
  遥测、sandbox；
- 外部 AgentBackend / ToolProvider（PI、DSH、MCP 风格服务）。

### 可兼容（生态）

- **PI**：默认 loop 与事件/工具/skill/MCP/telemetry 契约继续对齐；不改 PI 标准。
- **MCP**：外部工具与资源走既有 MCP 语义；不另造第三套工具协议。
- **DSH**：尊重其进程内 Cordis 生态。**不做逐插件 Cordis RPC**；
  DSH 以「整机 sidecar」或「纯工具 provider」两种方式接入。
  DSH 插件在 DSH 内部保持零改动；Keel 不假装它们是 Keel 原生插件。
- 外部 AgentBackend 可以选择由 backend 自己拥有会话状态（兼容模式，
  降低 Keel 原生恢复/交接保证）；这是明确的、可审计的取舍，不是偷偷绕过账本。

## What Changes

### 1. `contract-kernel`（新增）

把「谁能写账本、怎么写、写完谁认」从 `Runner`/`Store` 中抽成内核职责：

- `Commit` API：校验事件种类、配对、幂等、owner/epoch，再追加；
- `Claim` / `Renew` / `Finish` 带 fencing token；旧 owner 的提交被拒绝；
- 服务只能通过内核提交结果，不能直接持有事件表写权限；
- 租户/配额检查在提交与领取路径上执行。

### 2. `service-graph`（新增）

- 服务描述符：`provides` / `requires` / `optional` / `factory` / `effects`；
- loadout：不可变快照（服务集合 + 版本 + 配置摘要）；
- 依赖解析、必需/可选依赖、循环与重复 provider 拒绝；
- 生命周期：`PENDING → LOADING → ACTIVE → UNLOADING`，
  effects 反序清理，generation，启动失败回滚，诊断；
- 默认 loadout 必须逐字节复现当前外部行为（HTTP/SSE/事件/工具）。

### 3. `runtime-handoff`（新增）

- 新 runtime 启动 → 旧 runtime 停止 claim → 排水 → 释放 lease；
- 新 runtime 凭新 fencing token 从事件日志继续；
- SSE 客户端用 `Last-Event-ID` 续传；
- 交接点在用户消息后、assistant 工具调用后、tool result 后、模型流中、
  工具执行中都要有测试；
- 原生会话具备完整 Keel 保证；外部 DSH backend 的会话按兼容模式处理，
  单独登记，不混入原生保证。

### 4. `external-gateway`（新增）

- MCP 风格 JSON-RPC：stdio + Streamable HTTP；
- 流式 JSON 解析、帧上限、全链路背压、取消、超时、健康检查；
- v1 支持 `ToolProvider` 与 `AgentBackend` 两类外部服务；
- 外部进程不能访问事件帐本、lease、租户数据；
- 复用 `internal/mcp` 的传输/命名/裁剪能力，不另起协议。

### 5. `ecosystem-adapters`（新增）

- `pi-loop`：当前 loop 包成默认服务；替换 loop 必须过 PI conformance；
- `dsh-agent`：整个 DSH runtime 作为外部 AgentBackend，DSH 插件零改动；
- `dsh-tool`：纯工具插件通过 MCP 风格 ToolProvider 暴露；
- 明确的兼容矩阵：工具/skills 高，LLM adapter 中，hooks 中低，
  agent loop 低，UI/native 不支持；不承诺「全生态逐插件原生」；
- 兼容模式的状态所有权、审计、降级保证写入文档。

## 关键取舍（Alternatives Considered）

| 方案 | 结论 | 原因 |
|---|---|---|
| 把契约内核也做成插件 | ❌ | 插件可伪造事件、重复回复、抢会话；多租户/崩溃恢复失效 |
| 逐插件 Cordis RPC | ❌ | Cordis 是进程内对象图；服务身份、同步 `get/provide`、effect、事件总线无法无损 RPC；UI/native 插件不可服务端化 |
| DSH 整机 sidecar + 工具桥 | ✅ | 插件作者零改动；Keel 只把 DSH 当外部 provider；边界清楚 |
| in-process 二进制热插拔 / WASM | ❌ | Go 静态编译、2GB、安全隔离；收益低、风险高 |
| 新 runtime 接班（handoff） | ✅ | 与事件日志/租约天然契合；可回滚；不需要进程内可变代码 |
| 双账本（Keel + DSH 各记一份） | ⚠️ 仅兼容模式 | 一致性/顺序成本高；原生会话仍以 Keel 账本为唯一真值 |
| 外部 provider 直接写 Keel 账本 | ❌ | 绕过幂等/配对/租约；外部失败会污染事实 |

## 审查后收敛（P0，2026-11-07）

> 独立对抗审查发现以下问题；本节是权威收敛，规格与文档必须以此为准。

### 1. fencing 的线性化点：session 行是租约权威

`Claim` / `Renew` / `Commit` / `Finish` MUST 以 **session 行**为同一线性化点：

- `sessions` 增加 `owner`、`owner_epoch`、`lease_until`（或等价 lease 表，但必须与 seq 分配同行锁）。
- owner-scoped 提交在同一事务内 `FOR UPDATE` 锁 session 行，重读 epoch；旧 epoch 一律拒绝。
- `Claim` 锁 session 行并递增 epoch；`Renew` 条件更新 `owner+epoch`。
- 所有事务统一锁序：**sessions → runs**，禁止反向，避免 claim/finish 死锁。
- 验收：`claim || stale-commit` 交错压力测试，stale 提交 0 落账，最终回复计数=1。

### 2. 提交分两类，epoch 不是全局要求

- **owner-scoped**：assistant.message、tool.result、compaction、run 生命周期；
  必须带 owner+epoch。
- **edge-scoped**：user.message（客户端输入）；只校验 tenant/session/幂等，
  不校验 epoch，drain 期间仍可落账，由新 runtime 领取。
- 两类共用同一 seq 锁；edge 提交白名单在代码中注册，其他 kind 拒绝。

### 3. HTTP 事件端点收紧是显式 API 变更

`POST/GET /v1/sessions/{id}/events` 的现状（任意 kind、跨租户 200+空数组）
与内核严格校验冲突。本 change 明确采用：

- `POST` 只接受客户端白名单（`user.message`）；其他 kind 400；
- 跨租户/不存在统一 404；
- 现有 `/v1/sessions/{id}/input` 保持幂等语义，且：
  - 响应里的 `status` 报告真实值（`committed`）而非旧实现的 `pending`（提交已完成，
    `pending` 与事实不符）；
  - 同一 `requestId` 携带**不同内容**时返回 409（旧实现静默返回旧回执、吞掉新正文）；
- 这是一项 **MODIFIED API delta**，必须同步更新 E2E 与兼容说明；
  “不改变外部契约”的旧表述让位本条。以上每条差异都登记在
  `r1-allow.json`（路径 + 期望值 + 理由），R1 差分的通过条件是“白名单外零差异”。

### 4. 外部 backend 会话必须有数据模型

- `sessions.state_owner`：`keel | external`；
- `sessions.external_backend` / `external_ref` / `external_status`；
- 外部会话不进入原生 run/lease 作业流（或仅排队，不 claim）；
- SSE 只回放 Keel 记录的事件；backend 不可用是可查询终态；
- usage 必须标记 `metering=external|none`，不允许静默记 0 或假装原生计量。

### 5. loadout 替换采用 RETIRING，不是立即卸载

旧 generation 在还有在途 run 引用时进入 `RETIRING`，不再接新 run；
引用归零或 drain 截止后反序卸载。禁止“换 provider 立即卸旧实例”。

### 6. AgentBackend 不是 MCP 方法语义

复用 MCP 的传输/帧/背压，但 AgentBackend 是**版本化的私有方法集**：
`initialize`、`session.open/resume`、`message.send`、`event.stream`、
`cancel`、`health`、`close`。必须能力协商与版本拒绝。
DSH sidecar 依赖 pin（npm 包 + 版本）与 Node 工具链；生命周期、env/凭据清洗、
reaper、崩溃退避必须定义。DSH 纯工具桥需要一个 DSH 侧导出插件，**不是零代码**；
UI/native 插件不承诺。

### 7. 默认 loadout 用 R1 差分，不用“逐字节”

W0 合并后冻结基线 R1；验收器对同输入比较规范化 HTTP 状态/头、事件日志 dump、
工具序列；差异白名单只允许时间/随机 ID/日志行序；白名单外 0 差异。

## Non-Goals

- 不做 WASM、in-process 热插拔、Go `plugin` 包动态加载；
- 不把 DSH 插件逐个变成 Keel 原生服务；
- 不承诺 DSH UI/native/Electron 插件在服务端运行；
- 不做多节点共识；单主 PG + 租约；
- 不做价格字段/计费引擎；只做客观计量；
- 除本 change 显式登记的 `/v1/sessions/{id}/events` 收紧外，不改变现有 HTTP/SSE/blob/MCP 外部契约；
- 不在本 change 内做 DSH compatibility host 的逐插件模拟（单独 future change）。

## Compatibility & Migration

- 默认 loadout 对 R1 差分；除已登记的 `/v1/sessions/{id}/events` 收紧外，现有 API 不变；
- 旧装配保留 feature flag，直到 P 工作流全部通过；
- 新表/列只做增量：loadout revision、owner epoch、handoff 审计；
  不修改已有事件 payload；
- PI conformance 593（冻结基线）不退化（收口实测 634 PASS / 0 FAIL / 1 SKIP）；E2E 7/7、2GB/1000 基线是回归门；
- DSH 兼容模式 opt-in per tenant；审计日志记录 backend、版本、状态所有权。

## Risks

- **提交路径重构**：服务不再直接写事件，必须全部经内核；用默认 loadout 对比测试锁行为。
- **交接窗口**：模型流/工具执行中的旧 writer 可能晚提交；用 fencing token + 旧 owner 拒绝 + 事件去重兜底。
- **外部 backend 状态**：DSH 拥有自己的会话状态，不能假装有 Keel 原生恢复；必须在 API/文档/审计中显式标记。
- **插件失败**：启动失败要回滚，运行中失败不能污染账本；用故障注入和进程隔离测试。
- **性能**：服务图只在启动/换 loadout 时解析，不在 token 热路径；2GB 基线不得退化。

## Acceptance（唯一完成态）

- 五个 spec 的全部 Scenario 有自动化测试；
- PI conformance 不退化（冻结 593 → 收口 634）；E2E 7/7；2GB/1000 基线不退化；
- 原生 handoff 五类安全点：0 丢事件、0 重复最终回复、SSE 游标连续；
- 外部 gateway：慢消费者/大帧/坏帧/崩溃不拖垮 run；
- DSH sidecar 与 DSH tool provider 的参考适配器有真实测试；
- 默认 loadout 对 R1 基线差分：白名单外 0 差异；
- 子代理对抗审查通过；HTML 报告 + 公网链接；OpenSpec 归档。
