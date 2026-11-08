# Proposal: keel-kernel-loop

> 立项：2026-11-08。**分层纪律（Owner 裁决）**：keel 内核必须和 PI 一样简单纯粹——**内核就是一个
> loop**，可自由二次开发；产品能力（会话列表/停止/审批/插件/subagent/触发器/hooks/UI）属于
> `keel-agent` 产品层，不得渗进内核。本 change 只动内核语义与 loop 接线。

## Why

内核层现在缺三件事，且都不该由产品层代偿：

1. **投递模式**：PI 的 loop 有 `steer/followUp` 双队列与三个 drain 点（`internal/loop/agent.go`
   已逐字移植），但生产 runner 没接线——运行中继续输入只能排队到下一轮；
2. **输入消费事实**：`read_watermark` 是启发式，无法同时精确表达 queue 与 guide 的混排；
3. **运行摘要**：run 收口没有 `run.finished`（turns/tokens/stopReason），可观测性缺一块。

## What Changes

### 1. 内核纯度（结构性约束）

- 内核包（`internal/loop`、`internal/agentcore`、`internal/agenttool`、`internal/kernel`、
  `internal/agent`）MUST NOT import 产品层（`internal/agentapi`、`internal/agentext`、`web/`）；
  用包级测试钉住（`boundary_test.go`）。
- 内核只暴露 loop/事件/工具/账本原语；会话列表、停止、审批、插件、subagent、触发器、hooks
  一律在产品层。**内核不感知「谁在用它」**。

### 2. 投递模式（loop 语义）

- `user.message.payload.delivery: "queue"|"guide"`（缺省 queue）；语义对表 PI `steer/followUp`
  与 zcode `queue/guide`：guide 在当前 turn 内、所有 sibling tool results 提交后注入，一次一条，
  不打断模型流；非 streaming/有 stop/有附件时降级 queue。
- runner 给 loop 提供 `GetSteeringMessages`：只读未消费的 guide 输入（seq 升序），
  loop 在 PI 的三个 drain 点轮询。

### 3. 输入消费事实（替换水位启发式）

- `assistant.message.payload.inputSeqs`：本次请求呈现给模型的用户输入 seq 集合。
- 「未答输入」＝未被任何 assistant 的 `inputSeqs` 引用的 `user.message`；`read_watermark`
  保留为诊断/兼容字段，不再作为唯一真值。
- 崩溃矩阵：assistant 已提交→不重答；未提交→必重答；queue/guide 混排各恰好一次。

### 4. 运行摘要

- worker 在 run 收口（done/failed/cancelled）提交 `run.finished`
  （turns/tokens/toolTimings/context/stopReason/durationMs）；不进模型上下文。

## Non-Goals

- 不做会话列表/停止/审批/partials（产品层 `keel-agent`）；
- 不做 UI/NSH/TUI（客户端）；
- 不做插件/subagent/触发器/hooks（产品层）。

## Impact

- `assistant.message.payload` 增加 `inputSeqs`（R1 显式 MODIFIED，新增 allow 规则）；
- `user.message.payload` 增加可选 `delivery`；新事件 kind `run.finished`；
- 内核包依赖边界测试；既有 11 门回归不退化。
