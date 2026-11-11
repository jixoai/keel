# Design: keel-harness-v2

本文件是 `proposal.md` 的实现级展开。约定：**内核**＝AgentLoop（`docs/KERNEL.md`）；
**Harness**＝工具、环境、放置、UI 与插件编排（`docs/HARNESS.md`）。

## 1. 总纲：三层与责任分层

```
┌──────────────────────────────────────────────────────────────┐
│ Harness（每个 Group 由 worker 进程承载；本 change 一组一 worker）│
│  ├─ 放置：Group / session lease / 并发槽位                    │
│  ├─ 工具：bash、read/write/edit、mcp、skills、tasks、approve    │
│  ├─ 插件：plan、file、bash、mcp、skills、approve、notify、oar   │
│  ├─ UI：会话、任务面板、通知、技能、作业                       │
│  └─ 每次 run：组装 tool manifest + system sections + messages  │
├──────────────────────────────────────────────────────────────┤
│ 内核（AgentLoop，单进程；不知道用户、组、插件）                │
│  └─ loop：模型 ⇄ 工具调用；只通过宿主注入的能力工作             │
├──────────────────────────────────────────────────────────────┤
│ DB（结构，唯一真值）   layerfs（字节，内容寻址）  执行空间（可丢弃）│
└──────────────────────────────────────────────────────────────┘
```

三条边界：

1. **内核不感知用户/组/插件**。它拿到的是「工具清单 + system sections + message-parts +
   一个 invoke 回调」。这是 v1 已经成立的边界，本 change 不破。
2. **插件不检查同层保证**。每个插件只实现自己的契约；组合语义由 Harness 在装配时决定。
3. **DB 存结构，layerfs 存字节，执行空间可丢**。任何「文件」在 DB 里是一条
   `路径 → hash` 事实，在 layerfs 里是字节；执行空间只是物化视图。

## 2. Agent 与 Harness 的契约（注入式）

每个 run 开始时，Harness 交给内核四样东西：

| 注入物 | 形态 | 生命周期 |
|---|---|---|
| 工具清单 | `[{name, description, schema}]` | **run 内不可变**；只在 `config.toml` 轮转后变 |
| system sections | 有序文本段（基础提示、技能索引、任务上下文） | run 内不可变 |
| message-parts | 有序消息片段（来自账本重建 + 引导追加） | 每次请求可追加 |
| invoke 回调 | `invoke(tool, args, ctx) → result` | 贯穿 run |

内核**不做**：读文件、连 MCP、起进程、查数据库、决定用哪个模型。它只做
「模型 ⇄ 工具调用」与「上下文压缩」。

为什么是注入式：外部内核（Codex/Claude/DSH）只能接受注入（prompt + MCP），不可能来拉
我们的注册表。注入式让「keelCore 与 Codex 是同一种东西」这句话成立，也让 loop 可以单测、
可以换语言重写。

### 2.1 run 的边界与 config 快照

- 一次 run = 一次 attempt 的完整 loop（v1 的 run 语义不变）。
- run 开始时读取 `config.toml` 的当前版本，记录 `(configHash, anchorEventSeq)`。
- run 内注入物不变；用户中途改配置 → 只影响下一次 run（或下一次请求的追加段，
  见 §3）。
- 子代理 fork 会创建新 run，读取父任务那一版配置作为起点。

## 3. 上下文模型：一切皆文件

### 3.1 message-parts 与追加

- 上下文是**有序 message-parts**，不是一整块 prompt 字符串。
- 模型请求 = 已提交消息的纯函数（v1 契约），加上本次请求前的追加段。
- 变化通过**追加**进入下一次请求（v1 的 `guide` 模式：当前 turn 内注入、在所有 sibling
  tool results 提交之后、不打断模型流）。**永远不重写历史**。

### 3.2 一切皆文件

上下文的每个来源都是一个文件（或文件投影）：

| 来源 | 文件 | 何时变化 |
|---|---|---|
| 配置 | `config.toml`（轮转版本） | 用户/Agent 主动改写 |
| 技能索引 | `SKILL.md` 的 frontmatter | 技能增删 |
| 任务上下文 | `task_cards` 的事件投影（非文件，但同构） | 工具调用 |
| 工具描述 | 工具清单（装配期生成） | 配置轮转 |
| 引导输入 | `user.message{delivery:guide}` | 用户/触发器 |

规则：**新增上下文来源 = 新增一个文件 + 一条装配规则**，不改内核。

### 3.3 请求快照

每次请求记录：

- `inputSeqs`：本次呈现给模型的 `user.message` 集合（v1 已实现）。
- `configHash` + `anchorEventSeq`：本 run 的配置版本与锚点。
- `toolManifestHash`：本次工具清单的哈希（用于回答「这次 run 有没有新工具」）。

三者一起构成「这次 run 到底看到了什么」的可追溯证据。

## 4. 放置模型

### 4.1 Group

Group 是**项目组**，也是**资源与放置容器**：

- 一个 Group 由 Harness worker 进程承载；本 change 内一组一 worker，一组多 worker 的扩展
  留到后续 change（`session_placement` 预留 `worker_id`）。
- 一个 Group 可以承载多个用户的会话（项目组里不同成员派来的 Agent）。
- 一个用户的不同会话可以加入不同 Group（例如擅长设计的 Agent 被拉进某项目组）。
- Group 拥有：`max_concurrent_runs`（并发槽位）、内存上限、插件装配集、健康状态。

### 4.2 会话归属是 lease

```
session_placement(session_id PK, group_id, worker_id, epoch, lease_until)
```

- 加入/离开 = 写一条 placement 事实 + 更新 lease；不是静态绑定。
- 任何进程都能接管任何会话（v1 的 owner/epoch/lease 与事件账本不变）。
- 迁移 = 旧 group drain → 新 group 从账本重建（v1 的双排水 + Adopt/Rebuild）。

### 4.3 三种策略

| 策略 | 含义 | 用途 |
|---|---|---|
| `pack`（默认） | 按内存预算装箱，一个 group 可跨用户 | 密度优先、单机多租户 |
| `user` | 一个用户钉在一个 group | 企业隔离 SKU |
| `pin` | 指定会话钉在指定 group | 重会话、调试、灰度 |

### 4.4 并发槽位与配额

- 组内并发由 `max_concurrent_runs` 信号量控制；满了排队（带 ETA）而不是静默丢弃。
- 用户配额在 DB 侧聚合（跨 group 也成立）：并发 run 数、tokens、存储字节。
- 组内存上限由部署层（cgroup/容器）兜底；组 OOM 不扩散到其它组。

## 5. 任务日志与 fork

### 5.1 任务卡

```sql
task_cards(id, session_id, parent_task_id, title, status, created_seq, closed_seq)
task_events(id, task_id, seq, kind, payload, created_at)
```

- 任务卡是 DB 行；所有修改都是追加事件（`task.created` / `task.updated` /
  `task.attached` / `task.closed` / `task.note`）。
- 会话启动时自动创建初始卡；**每次工具调用必须带 `taskId`**（强制，缺省拒绝）。
- 任务树的当前状态是投影；历史永远可读。

### 5.2 与账本的关系

- 任务事件写进**所属会话的账本**（加 `taskId` 字段），保持 v1 的单一线性化点与
  `seq` 语义。
- 跨会话的子任务：子会话的卡带 `parentTaskId`，面板按 id 拼接成树。
- 不新增全局账本；跨会话的树是读模型。

### 5.3 fork

- fork 原点 = `eventSeq`（账本前缀）。
- 结构（任务卡、路径→hash）在 DB；字节不可变。所以「当时的文件」＝前缀引用到的 blob
  集合，**不需要文件系统版本号**。
- fork 创建：新会话 + 账本前缀复制（或引用）+ 新任务卡（`parentTaskId` 指向原点卡）。
- 用途：AI 派生子代理时快速岔出多个可并行分支；技能版本 A/B 对比。

## 6. layerfs

### 6.1 对象模型

| 对象 | 内容 | 寻址 |
|---|---|---|
| file object | 文件字节 | `sha256(content)` |
| tree object | 目录项列表（name → hash, mode） | `sha256(canonical entries)` |

- tree object 让「十万个没变的文件」在下一版里只是同一个树哈希。
- 引用计数 + GC：没有引用的对象才回收；fork/并行子代理共享引用。

### 6.2 路径在 DB，不在 layerfs

- `file_manifest(task_id, path, hash, mode, size, updated_seq)` 是当前态投影。
- 每次写追加一条 `fs.write{taskId, path, hash, mode}` 到会话账本。
- layerfs 只提供 `Put / Get / Stat / RefCount / GC` 与流式接口；它不认识路径。

### 6.3 capture 策略（默认实时）

`fs.capture` 决定执行空间的产物何时进入 layerfs：

| 模式 | 行为 |
|---|---|
| `per-tool-call`（默认） | 每次工具调用结束后结算增量（stat 缓存 + 文件监视，只算变了的文件） |
| `checkpoint` | 只在任务完成/子代理返回/显式 checkpoint 时结算 |
| `manual` | 只接受显式 attach |

- 默认实时，但**不重扫全树**：只对 mtime/size 变化的文件算哈希。
- 忽略清单默认只有 `.git/objects`（它本身就是内容寻址，收进来是双重存储）。
- 远端执行面由 adapter 提供增量（changed paths + hashes），Harness 不假设能扫盘。

### 6.4 执行空间

- 任务开始时按 `file_manifest` 物化到执行空间（本地目录或远端机器）。
- 平台**不做全量镜像承诺**：执行空间可丢弃，产物靠 capture 回 layerfs。
- 适配器契约：`materialize(manifest) → exec handle`；`capture(exec handle, since) → delta`。

## 7. 工具面

### 7.1 恒定工具面

| 工具 | 角色 |
|---|---|
| `bash` | 操作系统的聚合工具（进程、命令、管道） |
| `read` / `write` / `edit` | 高频例外，直连 file-plugin |
| `mcp` | 工具生态的聚合工具（list/describe/call） |
| `skills` | 知识库的聚合工具（ls/view/manage） |
| `tasks` | 记忆图的聚合工具（create/update/attach/close/list） |
| `agent` | 内部子代理的聚合工具（spawn/fork/resume/cancel/list） |
| `approve` | 对话式审批（confirm/prompt/choice） |
| `notify` | 通知（多消费器，系统推送兜底） |

工具总数恒定，与挂了多少 MCP server / 技能无关。新增能力 = 往聚合工具里加动作，
不是新增工具名。唯一例外是**安装期显式扩展**：插件可以在 `provides.tools` 里声明工具，
经安装审批后改变工具清单与清单哈希；它是一次可见的 schema 变更，不是运行中静默增删。

### 7.2 bash

- 底层 gbash：自定义命令、自定义文件系统、跨平台。
- **不用 bash 的 `timeout` 命令**，用参数：超时后不 kill，而是转入后台（`run-in-background`），
  返回超时信息、终端信息、后台 pid。
- 后台作业由 JobManager 管理（围绕 nohup/jobs/ps 的全局表）：收养、回收、UI 可见。
- 输出流式落 layerfs，工具结果只回 head/tail + blob 引用 + 退出码；绝不整段进内存/账本。

### 7.3 MCP

- registry 属于 Harness；`mcp(...)` 聚合工具是主路径（schema 发现、二进制内容、
  稳定工具清单）。
- gbash 里的 `mcp` CLI 是辅路径（脚本/管道/远端），与聚合工具共用同一个 registry。
- **禁止** `mcp__<server>__<tool>` 式硬编码工具名。

## 8. 外部运行时

- 内部 Keel Agent：真 agent（有任务卡、有账本）。
- Codex/Claude/DSH：外部工具（有渲染器、无任务卡）。通过 `oar-plugin`（sidecar）接入
  OAR：runtime 能力矩阵、带外 record stream（`seq` + `agentPath`）、typed rejection。
- 不给外部 runtime 注入「请记录任务卡」的指令——主动记录会拉低它的表现；归属由带外
  record stream 提供。
- 外部工具调用仍占用发起它的任务卡的 `taskId`（作为工具调用事实），但其内部细节只进
  渲染器数据，不进任务图。

## 9. 失败语义

| 场景 | 语义 |
|---|---|
| 工具重试 | 每个有副作用工具必须带幂等键（`requestId/generationId/toolCallId` 三键沿用） |
| 配置轮转 | 新文件原子替换；锚点先分配再 rename；旧版可回放 |
| 进程崩溃 | 会话被任何进程接管；后台作业由 JobManager 收养或标记 lost |
| 后台作业 | 有全局与每组上限；超限拒绝新作业；孤儿进程必须可回收 |
| 通知消费 | 消费是持久事实（ack 事件）；有 ack 能力的消费者在场时即时消费，定时器只做兜底 |
| 审批 | durable 决策（open/wait/answer/timeout），超时是持久状态，不是 setTimeout |
| capture 失败 | 不阻塞工具结果；记录 `fs.capture_failed`，下次 checkpoint 重试 |

## 10. 插件全景与装配

完整清单见 `plugin-inventory.md`（约 60 项，五种 kind）。装配模型借 DSH：

- **四层叠加**：base bundle → mode bundle（serve/headless/tui/sdk）→ preset → 用户
  `config.toml`；按 row id 覆盖，last write wins；激活由服务可用性驱动。
- **kind 边界**：kernel（loop seam）/ harness（工具与服务）/ provider（独立进程）/
  ui（面板）/ service（layerfs）。
- **能力 seam**：fs / shell / subprocess / exec-surface / terminal 均可换实现（本地/远端/SSH），
  上层工具契约不变。
- **不照搬**：Cordis/typert 运行时、「一能力一工具」、70 个 client UI 包、sandbox 族。

相对本 change 初稿新增的关键插件：`keel.spill`、`keel.compaction` 的修剪/外置、`keel.checkpoint`、
`keel.session-title`、`keel.session-query`、`keel.deliverables`、`keel.attachment`、
`keel.bundles`、`keel.presets`、`keel.hmr`、`keel.telemetry`、`keel.testkit`、
`keel.supervisor`、`keel.super-agent`、`keel.creation`。

不做的：`goal` / `plan-mode` / `todo`（并入 plan-plugin）、`agent-team`、DSH 的 ssh 执行面
（远端走自研契约）、browser-use / computer-use（等 fde2e-bu/cu）。

## 11. 创造模式与槽位模型

**槽位不由 Harness 内置**。Harness 只提供机制：注册/注销槽位、装载/卸载贡献、schema 校验、
目标降级。槽位由插件声明，id 带声明者命名空间；最大的槽位提供者是第一方客户端插件
`keel.ui`（route / nav / action / tool-renderer / message-part / sidebar / settings-section /
task-widget / surface）。第一方面板（任务板、通知、作业、技能…）只是这些槽位的贡献物。

贡献有三种形态，由槽位所有者声明允许集：

| 形态 | 运行方式 | 隔离 | 备注 |
|---|---|---|---|
| `chunk` | NSH 动态导入，同宿主 VM，作为组件渲染 | 无 | 跨 target（含 TUI）、零额外 VM |
| `package` | NSH 签名包，独立 surface + `HostLimits` | 独立 VM + 能力强制 | 重型工具；独立表面 |
| `iframe` | iframe / `WebView` | 浏览器级 | 兜底：非 NSH 存量插件；不主题化、TUI 不可用 |

**创造模式（`keel.creation`，内置）** 聚合本机所有插件的可扩展面文档（`EXTENSIBILITY.md` +
`extensibility.json`）成本机目录，经只读发现工具交给 AI；AI 据此写 Native 插件（chunk 贡献），
经校验、审批安装后在客户端渲染。它同时是验收手段：**证明 AI 能写出 Native 插件**。

## 12. 护栏与自动恢复

- **自动恢复是默认**：进程启动（崩溃后、容器重启后、升级换代）自动 resume 全部非终态 run，
  从账本重建，幂等重试；`config.toml` 可配 `auto|manual|never`、预算、退避、provider 兜底；
  另有显式 `POST /v1/sessions/{id}/resume`（可带配置轮转）。
- **子代理可续**：中断的子代理继续原任务，不重开替代者；父代理在预算耗尽后接手（可改配置）。
- **升级阶梯**：`keel.supervisor`（自动）→ 父/主代理 → owner（人工）→ `keel.super-agent`
  （可安装；恢复 main-agent、并行多个 main-agent）。
- 护栏项：重复工具提醒、单次超时、连续失败、额度/成本、内存压力；触发与升级都是账本事实。

## 13. 假设与未决

在本 change 立项时已确认的假设：

- A1：内核不改契约；新事件 kind 通过 `kindRegistry` 扩展。
- A2：一个 Group = 一个进程；单机内的 placement，跨机迁移不在本 change。
- A3：默认 `pack` 策略；`user` 是隔离 SKU。
- A4：capture 默认 `per-tool-call`，实现必须走增量，不得全树重扫。
- A5：忽略清单默认为 `.git/objects`。
- A6：外部内核只作为外部工具，不要求它维护任务卡。

未决（留给后续 change）：跨机迁移、多机共享 layerfs、技能脚本沙箱、
triggers 的流程编排能力、面板的实时投影实现。
