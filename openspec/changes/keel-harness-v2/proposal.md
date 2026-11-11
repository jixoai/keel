# Proposal: keel-harness-v2

> 立项：Owner 2026-11-11。本 change 是 **Harness 层**的下一版架构。
> 不动内核契约：内核（AgentLoop）仍是单进程、无状态、账本唯一真值；
> 多用户、放置、环境、工具、UI 全部在 Harness。

## Why

v0.1.0 已交付内核与第一版 Harness（`docs/KERNEL.md`、`docs/HARNESS.md`），并实测了
单进程 1000 会话 / 590MB。再往下走，卡在三件事上：

1. **环境不统一**：`read`/`write`/`edit` 与 `bash` 各走各的文件路径，文件冲突、大输出、
   去重都没有统一答案；换执行环境（本地/远端）时每个工具都要各自适配。
2. **记忆没有主线**：账本记录了会话，但没有记录「Agent 正在做哪件事」。工具调用与任务
   之间没有强关联，面板回答不了「这些动作是在完成什么」。
3. **放置没有单元**：v1 是单进程多租户，项目组、跨用户协作、隔离 SKU、排水粒度都没有
   承载对象。

## What Changes

### 1. 责任分层（架构原则，贯穿全篇）

**插件只向上声明「我做什么」，不对同层声明「我这里安不安全」；需要组合语义的地方由
Harness 编排。** 每个插件只依赖自己的契约，不打听同层的保证。反例：skills 的 patch 去问
文件系统有没有写保护；bash 去问 layerfs 是否强一致。正解：写保护是 file-plugin 契约内的
事，bash 只负责执行命令并报告事实。

### 2. 三层存储

| 层 | 放什么 | 特性 |
|---|---|---|
| DB | 会话、账本、任务卡、任务事件、路径→哈希、blob 引用 | 结构化、可查询、事务、唯一真值 |
| layerfs | 文件字节（file object）与目录树（tree object） | 按内容寻址、去重、不可变、引用计数/GC |
| 执行空间 | bash 真正跑的地方（本地真目录或远端机器） | 物化视图、可丢弃、平台不做全量镜像 |

- DB 认识「路径→哈希」，layerfs **不认识路径**，只有 hash→bytes。
- 同一内容只存一份；fork 与并行子代理共享字节，零复制。
- 执行空间的产物通过 `fs.capture` 进入 layerfs，默认在**每次工具调用后**结算增量。

### 3. 任务卡（plan-plugin）

- 任务卡是 **DB 行 + 追加事件**，不是文件夹。标题、内容、父子关系、资源引用都是事实。
- **每一次工具调用必须带 `taskId`**；会话启动时自动创建初始卡。
- 任务树是账本的投影；跨会话的子任务用 `parentTaskId` 拼接。
- **fork 原点 = `eventSeq`**：结构在 DB、字节不可变，所以「当时的文件」＝前缀引用到的
  blob 集合，不需要额外的文件系统版本号。

### 4. layerfs（Rust + gRPC）

- file object：`sha256 → bytes`，流式读写，绝不整文件进内存。
- tree object：目录树也按内容寻址（同 git tree），十万个没变的文件在下一版里只是同一个
  树哈希。
- 去重、引用计数、GC；配额按 group/租户。

### 5. 分组放置模型

- **Group ＝ 项目组 ＝ 资源与放置容器**，由 Harness worker 进程承载（本 change 内一组一
  worker；一组多 worker 留到后续）。一个组可以承载多个用户的会话；一个用户的不同会话可以
  加入不同的组（例如「擅长设计的 Agent 被拉进某个项目组」）。
- 会话归属是 **lease**（`session → group, epoch`），不是静态绑定；任何进程都能接管
  任何会话（v1 的 owner/epoch/lease 不变）。
- 组向会话分配**并发槽位**（`max_concurrent_runs`）；配额在 DB 侧按用户聚合。
- 策略：`pack`（默认，按内存装箱，可跨用户）、`user`（一人一组，隔离 SKU）、
  `pin`（会话级钉住）。

### 6. 恒定工具面

工具面恒定且小，能力一律经**聚合工具**进入：

`bash`（操作系统的聚合工具）· `mcp`（工具生态的聚合工具）· `skills`（知识库的聚合工具）·
`tasks`（记忆图的聚合工具）· `agent`（内部子代理的聚合工具）· `approve` · `notify` ·
`read`/`write`/`edit`（高频例外）。

- **禁止** `mcp__<server>__<tool>` 式硬编码工具名；MCP server 增删不改变工具清单。
- 插件 MAY 在安装时通过 `provides.tools` 显式扩展工具面（经安装审批、改变清单哈希），
  这是有意的 schema 变更，不属于动态能力。
- gbash 提供 `mcp` CLI 作为脚本/管道/远端执行面的辅路径，与聚合工具共用同一个 registry。

### 7. 外部内核 ＝ 外部工具

- 内部 Keel Agent：真 agent，有任务卡。
- Codex / Claude / DSH：**外部工具**，有专门的渲染器；不为它代开任务卡，也不要求它主动
  记录（主动记录会降低它的表现）。通过 `oar-plugin`（sidecar）接入 OAR，用其带外的
  record stream 做归属与渲染。

### 8. 上下文注入：一切皆文件

- 上下文是**有序 message-parts**；来源是文件（config、SKILL.md、任务日志、工具描述）。
- 变化以**追加**进入下一次请求（引导模式 / `guide`），不重写历史。
- 每次请求记录 `inputSeqs`（已实现）与所用文件的版本集合。
- 工具清单在 run 内不可变，只在 `config.toml` 轮转时变。

### 9. config.toml 驱动

- Agent 的全部行为由 `config.toml` 决定：模型、工具、capture 策略、并发、权限。
- 轮转是**不可变版本 + 账本锚点**：先写 `config.toml.new` → fsync → `rename()` 原子覆盖，
  旧版按事件序号锚定。
- 子代理 fork 时继承父的那一版配置，随后可写自己的 `config.toml`。

### 10. 插件全景（对齐 DSH）

DSH 的 base bundle（99 行）+ web-app（97 行）说明「一切皆插件」不是口号：内核 seam
（context / compaction / tool-executor / system-prompt / llm）、记忆卫生
（spill / tool-result-pruner / image-offload）、长跑件（goal / plan / guard）、
产品件（session-title / session-query、deliverables、attachment）都是默认装配。

本 change 按同一口径给出完整清单（`plugin-inventory.md`，约 60 项，分 kernel / harness /
provider / ui / service 五种 kind），并明确三条不照搬：Cordis/typert 运行时、
「一能力一工具」、70 个 client UI 包。v1 已交付的 plugin-manager / plugin-market /
web-search / context-memory 继续复用。

### 11. 创造模式（内置）

`keel.creation` 是内置插件，目标是**证明 AI 真的能写出 Native 插件**（NSH chunk 槽位贡献，
不是只能塞 iframe）。它的前提是聚合协议：

- **每个插件 MUST 交出可扩展面文档**（`extensibility.json` + `EXTENSIBILITY.md`）：我提供哪些
  槽位、接受哪些配置、能挂哪些 hook、可替换哪些 provider、示例与限制。
- 创造模式**聚合本机所有已装插件（含第三方）的这些文档**，形成机器可读目录，经只读发现工具
  交给 AI——AI 因此知道「本机装了什么、能做什么自定义」。
- 槽位**不由 Harness 内置**，由插件提供（最大的提供者是第一方客户端插件 `keel.ui`）。

### 12. 护栏与自动恢复（默认自愈）

- **崩溃 / Docker 重启后自动恢复整个 AgentLoop 是默认行为**，不需要点按钮；`config.toml`
  可配（`auto/manual/never`、重试预算、退避、provider 兜底），并有显式
  `POST /v1/sessions/{id}/resume`。
- **子代理停了必须能继续**，不许重开一个替代者；预算内自动重试，预算耗尽升级给父/主代理
  （可改配置后恢复，例如换掉额度耗尽的 provider）。
- **升级阶梯**：supervisor（自动）→ 父/主代理 → owner（人工）→ `keel.super-agent`（可安装，
  层级高于 main-agent，能恢复 main-agent、并行多个 main-agent）。
- 护栏覆盖重复调用、单次超时、连续失败、额度/成本、内存压力；每次触发与升级都是账本事实。

## Non-Goals

- **安全/沙箱**：本 change 不新增安全或沙箱要求；沿用 v1 已生效的插件 `permissions`
  （工具白名单 / net / secrets）与已登记边界（插件不做沙箱）。需要强隔离就上 Docker。
- **远端执行面照搬 DSH**：不学它的 ssh 方案——我们的抽象层级更高，执行面（本地/远端）
  由自研契约 `keel.exec-surface` 定义，适配器由我们定。
- **agent-team**：不做子代理团队协调插件（另有结构方案）；子代理协调由任务树 +
  `keel.subagent` 的 `list/control` 承担。
- **goal / plan-mode / todo 独立插件**：不做——all-in-one 的 plan-plugin（`keel.tasks`）已覆盖
  目标、计划与待办；「自动续跑」交给 `keel.supervisor`。
- **browser-use / computer-use**：本 change 不接；已有 fde2e-bu / fde2e-cu 组件，内核稳定后
  再经插件接入（登记）。
- **调度器**：CPU 交给 OS 与 runtime；本 change 只做组配额与并发槽位。
- **多机分布**：本 change 的放置是单机内的组；跨机迁移列入后续 change。
- **工作流引擎**：triggers 只是触发器，不做 workflow。

## Impact

- **内核**：不改既有契约。新增 `task.*`、`fs.*` 类事件需同步 `kindRegistry` /
  `internal/store` 常量 / 转录与 SSE 规则（本 change 内完成）。
- **Harness**：新增 placement（group/lease）、task、file、bash、mcp、skills、approve、
  notification、oar 插件。
- **新服务**：`layerfs`（Rust + gRPC），随 Harness 部署。
- **新表**：`groups`、`session_placement`、`task_cards`、`task_events`、`file_manifest`、
  `blob_refs`。
- **文档**：`docs/HARNESS.md`、`docs/KERNEL.md` 增补；`AGENTS.md` 补一句放置原则
  （默认单进程多租户；进程切分由 Harness 放置策略决定）。
- **预算**：后端整体（内核 + Harness + layerfs）**目标 ≤300MB、上限 ≤500MB**；前端可分离部署，
  surface VM 单独配额。注：v1 实测 1000 会话峰值 523.8–639.1MB，**新上限低于既有基线**，属待达标项。
- **兼容**：v1 的 HTTP/SSE 契约不变；`keel serve` 单进程模式保留为 `pack` 策略的一种。
- **对照**：冻结的 `keel-ecosystem` 中 `mcp__<server>__<tool>` 命名从未生效
  （`openspec/specs/` 无 mcp 能力），本 change 直接采用聚合工具面，不构成冲突。
