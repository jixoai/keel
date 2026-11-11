# State Machines（隐式状态机梳理）

写验收标准的前提。本文把项目里**本来就存在但没写下来**的状态机全部显式化。四条纪律贯穿全文：

1. **状态即事实**：每个状态必须指名权威事实（账本事件 / DB 行 / 装配事实）。只有内存里知道的状态
   视为不存在。
2. **非法转移必须拒绝**：状态机的转移集合是闭集；未列出的转移 MUST fail-closed 并留下拒绝事实。
3. **非终态即可接管**：任何非终态 MUST 能被另一进程从账本重建后继续（`supervision`）。
4. **可逆性**：卸载、关闭、撤销类转移 MUST 无残留（工具/hook/订阅/VM/进程/端口）。

---

## A. 内核（AgentLoop）

### A1. 会话（session）

| 状态 | 权威事实 | 谁可转移 | 可恢复 |
|---|---|---|---|
| `idle` | 无在跑 run（账本末条是终态） | 任何 worker | — |
| `running` | `runs` 行非终态 + lease | 持有 lease 的 worker | ✅ 必接管 |
| `waiting_human` | 升级事实（`escalated(L2)`） | owner / super-agent | ✅ |
| `paused` | `resume.mode=manual/never` 的显式暂停事实 | owner | ✅ |

转移：`idle →(输入/触发) running`；`running →(settle) idle|waiting_human`；
`running →(中断) running[另一 worker, epoch+1]`；`waiting_human →(resume) running`；
`paused →(resume) running`；`* →(归档) archived`（只读，不可再写）。

非法：终态会话再 resume 必须返回明确结果（不得假装成功）；`archived → running`。

### A2. run（一次 attempt）

| 状态 | 权威事实 | 说明 |
|---|---|---|
| `pending` | `runs.status=pending` | 已入队未 claim |
| `running` | lease + `owner/epoch` | 含 turn/工具批/压缩/capture 子阶段 |
| `settling` | settle 开始（`run.finished` 提交前） | 提交失败不改变终态 |
| `done` / `failed` / `cancelled` | `run.finished` | 终态；`failed` 可重试（新 attempt） |

子阶段（每个都是可重入的 await 点）：`await_model` → `commit_assistant` → `await_tools` →
`commit_tool_results` → `maybe_compact` → `maybe_capture` → `settle`。

非法：同一会话两个 epoch 同时 `running`（必须由 lease 拒绝）；`settling` 之后再有事件提交
（必须拒绝并落拒绝事实）。

### A3. 上下文与压缩（kernel seam）

| 状态 | 权威事实 |
|---|---|
| `within_budget` | 估算 token < 阈值（run 内） |
| `near_limit` | ≥ 阈值，下一次请求前必须压缩 |
| `compacting` | `compaction` 事件提交中（不在工具调用中间压缩） |
| `compacted` | `compaction` + checkpoint 已提交，保留段 + 摘要重建 |

非法：`compacting` 期间接受新的工具调用提交；摘要失败后吞掉原上下文（失败必须保留原上下文）。

### A4. 配置版本（config.toml 轮转）

`active`（`config.toml`）--写 `.new`→fsync→`rename`--> `superseded`（旧版，带锚点事件序号）
→ 新 `active`。每次 run 记录 `(configHash, anchorEventSeq)`。非法：无锚点地替换（必须补锚点）；
恢复时使用未记录的版本。

### A5. 事件分类与提交者（v2 新增 kind）

v1 的 class 是 `edge` / `owner` / `external`；v2 新增 **`system`**（Harness 驱动、无 lease 的
durable 事实，必须有自然幂等键）。每个新 kind MUST 在 `kindRegistry` 有 class 与 key：

| kind | class | dedupe key（独立命名空间） | 提交者 |
|---|---|---|---|
| `task.*` | owner（run 内）/ system（初始卡） | `task:<taskId>:<kind>:<seq>`；初始卡 `task:<sessionId>:init` | kernel / harness |
| `fs.write` | owner | `fs:<toolCallId>:<path>` | file-plugin |
| `fs.capture` | owner | `capture:<toolCallId>:<baseSeq>` | shell/capture |
| `config.rotated` | system | `config:<configHash>` | harness |
| `placement.joined/left` | system | `placement:<sessionId>:<epoch>:<kind>` | placement |
| `supervision.escalated` | system | `supervision:<sessionId>:<attempt>` | supervisor |
| `guardrail.tripped` | system | `guardrail:<sessionId>:<trigger>:<seq>` | supervisor |
| `resume.attempt` | system | `resume:<sessionId>:<attempt>` | supervisor |
| `notification.ack` | system | `notif-ack:<notificationId>:<consumerId>` | notify |
| `notification.push_sent` | system | `notif-push:<notificationId>` | notify |
| `run.finished`（payload 扩展） | owner | `run:<runId>`（lease/fence） | kernel |

**每个 kind MUST 有独立 KeyKind，MUST NOT 复用 `tool:` 前缀**（否则 `fs.write` 与同一次调用的
`tool.result` 撞键）。内核 `CommitRequest` MUST 为这些命名空间新增键字段；「同 toolCallId 的
`tool.result` 后紧接 `fs.write`」 MUST 作为负控单测。

「只追加」的准确含义是：**账本历史只追加**；模型上下文可以被 `compaction`/checkpoint 投影替换，
替换本身以 `compaction` 事件为准（v1 已如此）。

---

## B. 放置（harness-core）

### B1. Group

`provisioning` → `ready` → `draining` → `stopped`；`ready ⇄ degraded`（资源压力/配额触顶）。
转移权限：Harness 放置器。非法：`stopped → ready`（必须新建）；`draining` 期间接受新会话放置。

### B2. Worker 进程

`starting` → `serving` → `draining` → `exited`；`serving → crashed`（SIGKILL/OOM）。
`crashed` 必须落事实并把该 worker 的会话标记为可接管。非法：`exited` 后再上报心跳。

### B3. 会话放置 lease

`unplaced` → `placed(group, worker, epoch, lease_until)` → `expiring` → `adopted(worker', epoch+1)`；
`placed → orphaned`（worker 崩溃且未及时接管）。任何进程可接管任何会话。非法：更低 epoch 写入；
两个 worker 同时持有同一 epoch。

### B4. 并发槽位与配额

`free` → `leased(session)` → `released`；`queued(n)`（满时排队并给出 ETA）。
配额（并发 run / tokens / 存储字节）在 DB 侧聚合，跨组一致。非法：越过用户配额启动 run；
静默丢弃排队请求。

---

## C. 任务与记忆（task-log / plan-plugin）

### C1. 任务卡

| 状态 | 权威事实 | 说明 |
|---|---|---|
| `open` | `task.created` | 会话初始卡自动创建 |
| `in_progress` | `task.updated{status}` | 默认工作态 |
| `blocked` | `task.updated{status, reason}` | 等审批/等依赖/等外部 |
| `done` / `cancelled` / `superseded` | `task.closed{status}` | 终态；`superseded` 用于被 fork 取代 |

非法：`done → in_progress`（必须新建卡或显式 reopen 事件）；跨会话直接改别人的卡（必须经
`parentTaskId` 关系与权限）。

### C2. 工具调用归属

`unbound`（不允许存在）→ `bound(taskId)`。**任何工具调用 MUST 绑定 taskId**；缺失即拒绝并落
拒绝事实。子代理的工具调用绑定**子卡**。

### C3. 任务树与 fork

`root` → `child(parentTaskId)`；fork 边 = `(originEventSeq, originTaskId)` → 新会话 + 新卡。
非法：环（父指针不得成环）；fork 原点指向不存在的 seq。

### C4. 资源关联

`attached(path, hash)` → `detached`（引用计数归零才可 GC）。非法：对不存在的 hash 建立引用；
把字节复制进账本（必须只记引用）。

---

## D. 交互原语

### D1. 审批（decision）

`opened` → `delivered` → `answered` / `timed_out` / `cancelled`；答案含 `validated: true|false`
（「其它」逃逸 = false）。超时是**持久状态**，不是定时器。非法：重复作答（除幂等同 requestId）；
对已超时者再作答（必须明确拒绝）。

### D2. 通知（notification）

`created` → `delivered(consumer_i)` → `consumed(任一 ack)`；无可 ack 消费者时：`created` →
`push_sent` → `consumed`/`expired`。每个消费者一个状态，**任一 ack 即 consumed**。
非法：已 consumed 再推原生通知；重启后重复推送（ack 与推送都必须持久）。

### D3. 后台作业（JobManager）

`foreground` →（超时）`promoted` → `running` → `exited(code)` / `killed` / `lost`；
`running → adopted`（新进程收养）。上限：全局与每组；超限拒绝新作业，不 kill 已有。
非法：作业无 owner 会话；父死且未收养却仍标 `running`。

---

## E. 插件与 UI

### E1. 插件生命周期（沿用 v1，补两点）

`installed → enabled → disabled → uninstalled`；`PENDING`（缺依赖）；`FAILED`（fail-closed）。
补：`enabled → reloading → rolled_back`（HMR 失败回滚，不打断 run）；`generation+1` 单调。
非法：同 generation 内两次装载；卸载后残留注册。

### E2. 扩展面声明（新增）

`missing` → `declared`（`provides.slots` / `contributes` / `extensibility.json`）。
`missing` 的插件可用但**不可被创造模式利用**；官方插件发布门禁拒绝 `missing`。
非法：贡献指向未声明槽位或槽位不允许的形态。

### E3. 槽位与贡献（plugin-ui-extension）

槽位：`declared` → `active`（所有者启用）→ `withdrawn`。
贡献：`proposed` → `validated` → `lazy_pending` → `mounted` → `unmounted`；
失败分叉：`failed`（可见失败态，宿主不受影响）；`unavailable_on_target`（TUI 降级，显式标注）。
非法：`mounted` 时改槽位形态；卸载后仍持有事件订阅。

### E4. UI surface（package 形态）

`starting` → `running` → `stopped`；`running → over_quota`（宿主中止该次调用，宿主不崩）。
MUST 关闭即卸 VM。

---

## F. 执行面（exec-surface / shell / fs / capture）

### F1. 执行空间

`unmaterialized` → `materialized`（按 manifest）→ `dirty`（有变更未 capture）→ `captured`；
`* → discarded`（可丢弃，重新物化即可）。非法：把执行空间当真值（真值只在 DB + layerfs）。

### F2. capture

`idle` → `scanning`（stat 缓存 + 监视，只算变化）→ `committing`（blobs + manifest）→ `idle`；
失败：`failed` →（下一次 checkpoint 或工具调用）重试。capture 失败 MUST NOT 阻塞工具结果。

### F3. 文件写与冲突

`read(v)` → `write(precondition v)`：命中 → 提交新版本并**写穿到已物化的执行空间**；未命中 →
`conflict`（agent 重读重试）。非法：无前置条件的覆盖写（file-plugin 契约内必须带版本令牌）；
capture 重复结算显式写的产物。

---

## G. 恢复与升级（supervision）

### G1. 中断检测与自动恢复

`interrupted`（进程死亡/容器重启）→ `detected`（启动扫描）→ `resuming` → `running`；
失败 → `attempt_failed(n)` → `n≥maxAttempts` → `exhausted`。
**默认 `resume.mode=auto`**：`detected → resuming` 自动发生，零人工。非法：`interrupted`
被当作终态静默丢弃。

### G2. 升级阶梯

`L0 auto`（supervisor 重试）→ `L1 parent/main agent`（可改配置后 resume）→ `L2 owner`
（`keel.notify` + `waiting_human`）→ `L3 super-agent`（若已安装；可并行多个 main-agent）。
每次升级是账本事实。非法：跳过阶梯直接 L3；L2 未通知即视为已升级。

### G3. Provider 兜底

`primary` → `exhausted` → `fallback_next` → `recovered` / `all_exhausted`（→ 升级）。
额度耗尽 MUST 立即停止对该 provider 的重试。非法：额度耗尽后仍重试该 provider。

### G4. 护栏触发

`armed` → `tripped(reason)` → `mitigated` / `escalated`。护栏项：重复工具调用、单次超时、
连续失败、额度/成本、内存压力。非法：tripped 后无处置记录。

---

## H. 能力接入

### H1. MCP server

`configured` → `connecting` → `connected ⇄ degraded` → `unavailable`；server 崩溃不拖垮 run。
工具清单缓存随连接状态失效；**server 变化不改变模型可见工具清单**。

### H2. 外部运行时（oar）

`unavailable` → `handshake` → `ready` → `session_open` → `turn_in_flight` → `closed`；
`resume` 返回 `resumed=false` ⇒ `backend_unavailable`（不伪造恢复）。外部运行时的调用**不建任务卡**，
只作为外部工具事实 + 渲染器数据。

### H3. 技能

`discovered` → `indexed`（name+description+path 进系统提示）→ `loaded`（正文按需）；
订阅源：`subscribed` → `pinned` → `update_available` → `conflict`。内容哈希变化 ⇒ 索引失效重建。

### H4. 触发器

`registered` → `armed` → `fired` → `consumed`；`fired` MUST 幂等去重（webhook 重投是常态）；
漏跑 MUST 有补偿策略。非法：同一幂等键触发两次会话。

---

## I. 前端与连接（可分离；其状态属前端域）

- **流**：`connecting` → `live` → `reconnecting(Last-Event-ID)` → `resumed` / `gap_filling`。
- **在场（presence）**：`attached` / `detached`——通知推送的判据（`ack` 才是消费事实）。
- **创造模式会话**（AI 驱动）：`intent` → `catalog_read` → `scaffold` → `generated` →
  `validated` / `rejected(errors)` → `approval_pending` → `installed` → `verified` / `rolled_back`。

---

## J. 跨域不变量（写进验收）

1. 每个非终态都有：**接管路径** + **幂等键** + **可查的恢复事实**。
2. 每个状态转移都有**拒绝事实**（非法转移必须可见，不许静默）。
3. 每个插件的每个副作用（工具/hook/触发器/UI/VM/进程/端口）都有**反序撤销**路径。
4. 内存预算：后端（内核 + Harness + layerfs）**目标 ≤300MB、上限 ≤500MB**；前端可分离，
   其 surface VM 配额单独设限（默认单 surface 32–64MB）。
