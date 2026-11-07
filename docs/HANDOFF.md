> **当前状态（2026-11-07）：已接线（W4 落地）。** `internal/handoff` 的 Coordinator 已接入
> `cmd/runtime`：runtime_id 持久化并写入 claim/审计；SIGTERM = shutdown drain；管理端点
> `POST /v1/runtime/drain {"mode":"handoff"|"shutdown","deadlineMs":N}` = 显式排水
> （`GET /v1/runtime/status` 可观测）；worker 在排水开始即停 claim，handoff 期间 edge 输入
> 照常落账；在途 run 受排水截止约束；新 runtime 以 epoch+1 接班并从账本重建。
> 本 change 之前遗留的「SIGTERM 只有 shutdown」表述作废。
>
> **怎么用（运维速查）**
>
> 1. 起新进程：同一个 PG DSN、**只设 `KEEL_RUNTIME_ID`（必填，若空则用 hostname-pid）**；
>    新进程 `ready` 后其 worker 只领尚未被旧 owner 持有的会话。
> 2. 让旧进程排水：`curl -XPOST $old/v1/runtime/drain -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
>    -d '{"mode":"handoff","deadlineMs":30000}'`；旧进程立即停 claim，继续接受
>    `POST /v1/sessions/{id}/input`（edge 输入落账并唤醒 run）；在途 run 的“安全点”就是
>    **提交边界**——已提交的事件即是全部事实，未提交的 partial（模型流中的半条消息、
>    执行中的工具）在截止时随上下文取消被丢弃，由新 owner 从账本重建（工具按 at-least-once
>    重执行，最终恰好一条结果）。超时在审计里记 `timed-out`，其租约过期后由新进程强制接管。
> 3. 观察：`GET /v1/runtime/status`（runtimeId / loadoutRevision / drainMode / claiming /
>    inFlightRuns）；审计表 `runtime_instances` + `handoffs`（旧/新 runtime、loadout revision、
>    epoch、开始/结束时间、在途 run 数）。
> 4. SSE 客户端按 `event: drain` 帧的 `retry_after_ms` 重连到新进程，`Last-Event-ID` 续传，
>    首帧 = 游标+1；已提交事件不重不漏。
>
> **升级纪律（跨版本割接，必须遵守）**：**先 drain 旧进程，再启动新二进制；禁止新旧写者
> 并存。** 旧二进制的写路径不过 fence（`store.AppendEvent` 不看 epoch），旧式
> `store.FinishRun` 也不清 `sessions.lease_until`——实测（`internal/kernel/cutover_test.go`）：
> 旧写者能在新 owner 持租期间直接补账；旧式 finish 之后新进程最长一个有界 TTL 内领不到该
> 会话（答案为 lease-held，等残留租约过期即可）。handoff 协议本身即防御：旧进程必须
> 先 drain 再让新进程接——handoff 模式下在途 run 跑到提交边界后 **Finish 会清租约**，
> 新进程随即能领；shutdown 模式下在途 run 是**放弃**（不 Finish、不 release），其租约
> 到期后由新进程接管，因此 SIGTERM 换二进制时新进程接手最多等一个有界 TTL。
>
> **外部（DSH）会话的边界**：外部会话的落库标记已落地（`sessions.state_owner/external_backend/
> external_ref/external_status/metering`，kernel `ExternalStore` + store 层读写 + PG 测试；
> 外部会话永不进入原生 claim/run 流，backend 崩溃 → `backend-unavailable` 可查询终态，
> 不伪造原生恢复）。**runtime 侧目前还没有暴露打开外部会话的 HTTP 入口**——那属于网关接线
> （W5），在此之前 DSH 兼容模式只有库级能力与测试。

## 回滚手册（把新二进制换回旧版）

1. **先排水**：`POST /v1/runtime/drain {"mode":"handoff"}`（或对旧进程发 SIGTERM）；确认
   `GET /v1/runtime/status` 的 `claiming=false` 且 `inFlightRuns=0`，或等 drain 截止后租约过期。
2. **停新进程、起旧二进制**：同一个 PG DSN；旧二进制不认新列是安全的（新列都可空/有默认，
   内核迁移是增量的；旧写路径不看 `read_watermark/pending_input`）。
3. **禁并存写者**：回滚同样必须「先停新、再起旧」——旧式写者无 fence，与新 owner 并存会
   产生不受围栏约束的追加（见上文升级纪律）。
4. **不要回滚数据**：事件账本只追加；新二进制写入的 `run.started`/`external.event` 等
   记录对旧版是未知 kind，旧版读事件时按未知类型跳过（不会崩溃），但客户端可能不识别——
   回滚后如需对外一致，按变更单在网关侧过滤。
5. **已知窗口**：旧进程 Finish 不清 `sessions.lease_until`，新进程最长一个有界 TTL 内领不到
   该会话；SIGTERM 放弃的 run 同样等一个 TTL。用 `KEEL_LEASE_MS` 可调小该窗口。

# Keel Runtime Handoff · 换班而不是热拔插

> 本文定义 `keel-service-graph` 的原生会话交接协议。
> 目标：升级 runtime / 替换服务 / 换 loadout 时，会话不丢、不重、SSE 游标连续。

## 1. 为什么不是热插拔

- Keel 是 Go 静态二进制；进程内替换代码需要 WASM/动态库，风险与收益不成比例。
- 2GB / 1000 会话要求可预测的内存与资源边界；热插拔会让旧代码/旧 goroutine/旧监听器
  的生命周期不可控。
- 事件账本 + lease + fencing 已经提供了更干净的升级原语：**新进程接班**。

## 2. 角色与状态

| 概念 | 含义 |
|---|---|
| `owner` | 当前 runtime 标识 |
| `epoch` | fencing token，每次 claim 递增 |
| `lease_until` | 旧 owner 最晚可续租/提交的时间 |
| `loadout_id/revision` | run 固定的服务图快照 |
| `cursor` | 已提交事件 seq / SSE 游标 |

## 3. 正常交接

1. **新 runtime 启动**：加载新 loadout，完成 `ready`（PG、迁移、服务图解析）。
2. **旧 runtime 进入 handoff drain**：
   - 停止 claim 新 run；
   - 不再接受新的“开始执行”请求；
   - 继续接收 edge-scoped 输入（写 `user.message` 事件）；
   - 唤醒 SSE 客户端，提示重连（可选）。
   > 这与 SIGTERM 的 shutdown drain 不同：shutdown 保留现有 ops 语义（拒绝新写、
   > 客户端重试/重连），不承诺 drain 期间输入落账。
3. **排空在途 run**：
   - 能完成的完成；
   - 模型流/工具执行到安全点后停止；
   - 已提交事实保留，未提交 partial 丢弃；
   - 释放 lease。
4. **新 runtime claim**：拿到 epoch+1；从账本重建；处理 pending run。
5. **SSE 续传**：客户端用 `Last-Event-ID`/offset 连接新 runtime；首帧 = 游标+1。

## 4. fencing

- 旧 runtime 的 `Commit` 必须携带 claim 时的 epoch。
- 新 runtime claim 后 epoch 增加；旧 epoch 的提交一律拒绝。
- 旧 runtime 收到拒绝后必须停止重试、退出；不得把本地结果重新包装成“新提交”。
- 审计记录：旧/新 owner、epoch、时间、在途 run、最后 cursor。

## 5. 五类安全点

| 交接点 | 期望 |
|---|---|
| 用户消息已提交后 | 新 runtime 从该 seq 继续，不重复 user 事件 |
| assistant tool call 已提交、结果未提交 | 新 runtime 补执行工具（at-least-once），最终一条 result |
| tool result 已提交 | 新 runtime 从结果继续下一轮，不重复执行 |
| 模型流中 | 未提交 partial 丢弃；新 runtime 从最后提交点继续，不出现半条消息 |
| 工具执行中 | 旧执行可能已产生副作用；新 runtime 按幂等键重试，最终收敛 |

## 6. 不变量

- 0 丢事件：已提交 seq 在新 runtime 可见；
- 0 重复最终回复：同一 `generationId` 只落一条 assistant；
- 0 工具双结果：同一 `toolCallId` 只有一条最终 result；
- SSE 游标连续：不跳 seq、不回退；
- run 最终收敛：`done` 或 `failed`，不得永久 `running`；
- 旧 owner 不能在新 epoch 下提交。

## 7. 外部 backend 兼容模式

DSH 等外部 AgentBackend 可以自己拥有会话状态。这类会话：

- 标记 `external_backend` / `state_owner=external`；
- Keel 只记录高层消息/引用，不承诺原生 kill-9 重建；
- handoff = 调用 backend 自己的 resume/重连，不是 Keel 的 fencing 交接；
- 监控、UI、报告必须区分，不得把兼容会话算作原生恢复成功。

## 8. 测试矩阵

- 正常 drain + 新 runtime 接班；
- 旧 runtime 在五类安全点被 kill；
- 旧 writer 晚提交（旧 epoch）→ 拒绝；
- drain 期间新 user 输入不丢；
- SSE 客户端跨 handoff 重连：首帧/无重复/无缺口；
- 2GB / 1000 会话下 handoff 不引发 OOM；
- 审计记录完整，可回答“哪个 run 在哪个 loadout/epoch 上完成”。

## 9. 运维入口（目标）

- SIGTERM：进入 drain（沿用现有 ops 排水）；
- 管理面：查询 runtime/owner/epoch/loadout；
- 测试/E2E：两个 runtime 进程共享 PG，一个 drain 后另一个 claim；
- 不做进程内代码热替换；不做双写账本。

## 10. P0 收敛补充

- 持久 `runtime_id` 写入 claim/审计；drain 必须有截止时间，截止后新 runtime 可按 lease
  过期强制接管，旧 owner 的 owner-scoped 提交被 fencing 拒绝。
- drain 期间 `user.message` 属于 edge-scoped 提交：不校验 epoch，仍可落账，由新 owner 处理。
- **未读输入是持久事实**（B3/B5）：edge 提交在同一事务里把 `runs.pending_input` 置位，
  `runs.read_watermark` 记录上一次 attempt 被证明过的读取水位；claim 只**交付**两者、
  不清零，只有「水位覆盖了全部输入」的 finish 才清零。放弃的接管（drain/kill/进程退出）
  因此不会吞掉未读输入；后继 runner 会把 `seq > read_watermark` 的 user 消息移到
  transcript 末尾交给模型——否则日志里「输入在旧回答之前」会让模型误以为无需回复
  （§6「0 重复最终回复」的反面：0 漏答）。
- SSE 只发送已提交事件；旧 runtime 可以继续服务旧连接，但执行归属必须在新 owner。
- handoff 双进程 RSS 纳入 2GB 验收；不能只测单进程。
