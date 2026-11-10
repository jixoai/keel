# 恢复与压缩

两个机制让长会话保持诚实：**恢复**（让 `kill -9` 成为非事件）与**压缩**（在不改写历史的前提下
让上下文有界）。两者遵循同一规则——账本只追加，且两者的结果都是一条已提交事实。

## 1. 恢复，一句话

> 崩溃的进程从不被续命，它被**取代**。继任者 claim 会话，从账本重建转录，只重做账本判定
> 缺失的部分。

这一句话解释了本节的每个设计决策：read watermark、消费事实（`inputSeqs`）、工具执行
at-least-once 契约与推导式 generation id，存在的意义就是让「缺什么」成为事实而非猜测。

## 2. 崩溃留下了什么

账本是 durable 状态——`SIGKILL` 之前的所有已提交事实全部幸存。没能幸存的是进程本地状态：
在途模型流、执行了一半的工具批次、hub 里的 token 级 partials。这些永远不被当作事实。

恢复是普通 claim 路径，不是特殊模式（`internal/agent/worker.go:170`）：

1. 死掉的 attempt 不再续租；租约过期（默认 TTL 30s，`KEEL_LEASE_MS` 可覆盖——
   `internal/agent/worker.go:151`）；
2. 下一次 claim 接管会话：内核签发**新 epoch**，claim 报告 `Reclaimed` 使租约过期指标保持
   诚实（`internal/kernel/kernel.go:123`、`internal/agent/worker.go:200`）；
3. 旧 owner 的任何写入都被围栏拒绝——继任者是唯一写者（`stale-epoch` / `lease-lost`，
   `internal/kernel/kernel.go:355`）。

### 继任者知道什么

| 问题 | 回答机制 | 出处 |
| --- | --- | --- |
| 死掉的 attempt 没读到哪些输入？ | 在任何其他工作之前持久化的 **read watermark** | `internal/agent/runner.go:295` |
| 这条输入是否已被回答？ | **消费事实**：完成态 assistant 消息的 `inputSeqs` | `internal/agent/runner.go:707` |
| 死掉的 attempt 是否只答了一半？ | 最新 assistant 消息之后未解析的工具调用 | `internal/agent/runner.go:829` |
| 同一条答案会不会被写两次？ | 不会——**generation id** 从日志推导，重试答案自动去重 | `internal/agent/runner.go:634` |
| attempt 运行期间有输入到达吗？ | `FinishRequest.ReadThrough` 让水位之上的一切重新入队 | `internal/agent/worker.go:399` |

### 工具是 at-least-once

跨崩溃的工具执行是 **at-least-once**：已提交调用但缺结果时，继任者会再次执行，内核保证每个
`toolCallId` 恰好一条结果（`internal/agent/runner.go:829`、`:937`）。这是刻意的产品契约——
框架从不假装 exactly-once：

> 幂等效果：工具需自带幂等键（replay-safe ≠ exactly-once，框架不假装保证）。
> —— `docs/ARCHITECTURE.md` §3

### 干净关闭是另一回事

执行中途关闭的进程不会把 run 结算为失败；它**把 claim 还回去**（`Release`），下一个进程立即
领取该 run，租户准入槽位被归还而不是泄漏（`internal/agent/worker.go:368`、
`internal/kernel/lease.go:112`）。

### 手工重试输入

若 attempt 以终态失败（provider 错误），输入保持「欠着」，下一次 claim 会重试。运维或客户端
也可以显式重问一条已提交输入：`POST /v1/sessions/{id}/retry` 以新的 request id 提交同一文本，
并在载荷里记录 `retryOf`（它重问的 seq）（`internal/httpapi/server.go:156`、
`internal/httpapi/input_commit.go:41`）。

## 3. Handoff：跨进程恢复

升级就是把同一问题放大到进程尺度。Harness 从不热换运行中的进程：**handoff** 启动新 runtime、
排空旧的，让新 epoch 接班（`docs/HANDOFF.md`）。

```
旧 runtime                                    新 runtime
  │ claim 循环                                    │
  │◀── POST /v1/runtime/drain {"mode":"handoff",  │
  │        "deadlineMs":30000}                    │
  ├─ 立即停止 claim                                │
  ├─ 继续接受 edge 输入（仍然落账）                 │
  ├─ 在途 run 跑到提交边界；                      │
  │     未提交的 partial 被丢弃                    │
  ├─ 释放租约 ───────────────────────────────────▶│ 以 epoch+1 领取
  │                                                ├─ 从账本重建
  │ SSE 客户端收到 `event: drain`                  │ 客户端从 Last-Event-ID 续传
  │ {retry_after_ms} 后重连 ──────────────────────▶│ 首帧 = cursor + 1
```

- 两种排水模式：`shutdown`（SIGTERM 路径——在途 run 被放弃，其租约到期）与 `handoff`
  （在途 run 跑到提交边界结算，租约被清除）（`docs/HANDOFF.md`，`POST /v1/runtime/drain` 见
  `internal/httpapi/runtime.go:178`）。
- 审计：`runtime_instances` 与 `handoffs` 记录新旧 runtime、loadout revision、epoch 与在途
  数量；`GET /v1/runtime/status` 暴露 `runtimeId`、排水模式、`claiming` 与 `inFlightRuns`
  （`docs/HANDOFF.md`）。
- **升级纪律（强制）**：绝不允许两个写者并存——*先排空旧进程，再启动新二进制*。旧二进制的
  写路径不过围栏，并存会破坏「恰好一个写者」不变量（连同切换测试
  `internal/kernel/cutover_test.go` 与回滚手册记录在 `docs/HANDOFF.md`）。
- 用户看到的是会话在继续，而不是进程被保住：0 事件丢失、0 重复最终回复、SSE 游标连续
  （W4 验收见 `docs/SERVICE-GRAPH.md` §6）。

## 4. 压缩

长会话必然超出任何上下文窗口。压缩把转录最旧的部分摘要化，并提交这一事实——它从不编辑或
删除历史。

```
转录: [m1 … m40][m41 … m60][m61 … m80]      ← 越过阈值
                     │
    compaction.cutpoint / summary / tokens          internal/compaction/compact.go
                     ▼
提交事件 kind=compaction                            internal/agent/runner.go:478
  {summary, firstKept, summarized, timestamp}
                     ▼
下一次请求呈现: [system, summary, kept messages…]
```

- **触发**：compaction 服务从租户文档取设置——`compaction.enabled`、`threshold`、
  `keepRecentTokens`（`docs/LOADOUT.md` §3）。当模型声明了自己的上下文窗口时，优先采用模型
  窗口而非租户默认值（`internal/agent/runner.go:489`）。
- **切点**：由 compaction 包策略选定，钉在回合边界，绝不落在工具调用/结果配对中间
  （`internal/compaction/cutpoint.go`）。
- **事件形状**：`{summary, firstKept, summarized, timestamp}`
  （`internal/agent/runner.go:467`）；`firstKept`/`summarized` 是索引事实，重建时可精确复原
  请求当时呈现的内容。
- **投影**：客户端把压缩渲染为维护事实（`compaction`，见
  `packages/chat-surface/src/projection.ts`）；压缩进行中会标记会话忙碌并阻止队列排空
  （`docs/CHAT-SURFACE.md` §5）。
- **一致性**：压缩核心移植自 PI 测试套件，属于 conformance 矩阵的一部分
  （`docs/PI-CONFORMANCE.md`、`internal/compaction/conformance_test.go`）。

## 5. 验收证据

恢复保证是被测试钉住的，不是被承诺的：

| 保证 | 证据 |
| --- | --- |
| requestId 幂等、SSE 游标、blob 续传、2000 事件序列无缺口 | `cmd/e2e`（`README.md` 记 7/7） |
| 模型调用中与工具执行中 `SIGKILL` 后继续 | `internal/agent/crash_matrix_test.go`、`internal/durable/*_test.go` |
| 放弃的 attempt、租约接管、排水门 | `internal/agent/worker_abandon_test.go`、`worker_drain_gate_test.go` |
| handoff 五类安全点与双进程 SSE 续传 | `internal/handoff/*_test.go`、`docs/HANDOFF.md` |
| 割接纪律（旧写者 vs 围栏） | `internal/kernel/cutover_test.go` |
| 压缩切点与策略一致性 | `internal/compaction/conformance_test.go`、`internal/loop/loop_compaction_test.go` |
| 全量门禁 | `scripts/acceptance-all.sh` |
