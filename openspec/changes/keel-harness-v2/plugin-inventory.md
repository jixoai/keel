# Plugin Inventory（Keel Harness v2 的完整插件面）

> 基准：DSH `deepseek-ai/deepseek-harness` **pin `fcc0ad462172`**。`packages/bundle/base/cordis.patch.yml`
> **99 个装配行（文件 548 行）**、`packages/bundle/web-app/cordis.patch.yml` **97 个装配行
> （文件 601 行）**；全仓 `packages/` 共
> **53 个能力组 / 370 个 workspace 包**（2026-11-11 复核）。创造模式参照其 `packages/extensions/`
> （`cordis-host-runner` / `cordis-client-runner` / `tool-cordis` / `ui-cordis`）。
>
> 本文回答两件事：**DSH 的插件架构借鉴什么**、**Keel Harness v2 要做哪些插件**。

## 0. 本仓的四条既定裁决（2026-11-11，Owner）

1. **长跑件不单独做**：`goal` / `plan-mode` / `todo` 不设独立插件——**all-in-one 的 plan-plugin
   已经覆盖**（任务卡即目标、计划与待办）。
2. **护栏要更激进**：崩溃 / Docker 重启后**自动恢复整个 AgentLoop**是默认行为（可配置、有显式
   resume 接口）；子代理停了**必须能继续**，不许重开；预算耗尽后升级给主代理，主代理之上升级给
   super-agent（插件，可安装，可并行多个 main-agent）。见 `supervision` 与 §3.8。
3. **远端不照搬 DSH**：我们的架构比 DSH 更抽象，执行面走**自研契约**（`keel.exec-surface`），
   不复制它的 ssh 方案。
4. **browser-use / computer-use 暂缓**：已有 fde2e-bu / fde2e-cu 组件，内核稳定后经插件接入。

## 1. 借鉴与不借鉴

### 1.1 借鉴

1. **内核也由 seam 插件构成**。DSH base bundle 里 `agent-loop`、`system-prompt`、`tools`、
   `tool-executor-native`、`llm`、`compaction-basic`、`context` 全是插件；内核只保留「账本 + loop
   骨架」。我们的 `keel-context-plugin` 只是其中一条。
2. **装配模型**：base bundle + mode bundle + preset + 用户配置，按 **row id 覆盖、last write wins**，
   激活由「服务可用性」驱动。`config.toml` 轮转采用同一套 overlay 语义。
3. **能力 seam 化**：fs / shell / subprocess / exec surface / terminal 都是可替换 provider。
4. **默认就有的记忆卫生**：`compaction-basic`、`tool-result-pruner`、`image-offload`、
   `spill-local/policy`、`session-checkpoint-policy`。
5. **创造模式的形态**：运行时自省（inspect registry）+ **生成式目录**（tool/config/slot/api
   catalog）+ 只读发现工具 + 带 guard 的求值器 + 生命周期卡片 UI。差异见 §4.1。
6. **产品完整度**：`session-title`、`session-query`、`session-log-export`、`deliverables`、
   `workspace-changes`、`attachment`、`commands`。
7. **插件基础设施**：`plugin-manager`、`plugin-package-inventory`、`hmr`、`settings`、
   `config-editor`、`agent-preset-registry`。

### 1.2 不借鉴

| DSH | 为什么不照搬 |
|---|---|
| Cordis DI + `typert` 构建期类型图 | 我们是 Go + `agentext` + `servicegraph`；引入会变成第二套插件运行时 |
| 一能力一工具（`tool-bash`/`tool-fs`/`tool-web`/`tool-todo`…） | 工具面恒定，插件按 action 挂到聚合工具上；**插件粒度 ≠ 工具粒度** |
| 70 个 client UI 包 | 我们只有一套 NSH 客户端；**槽位由插件声明**，面板是贡献物（§3.10） |
| sandbox 族 / ssh 执行面 | 非目标，且远端走自研契约（裁决 3）；要强隔离上 Docker |
| `workflow` / `ptc` | 登记，不承诺 |
| session 持久化到 JSONL | 我们用 PG 账本；只借「投影/标题/查询」的能力面 |
| `agent-team` | 不做，另有结构方案（裁决；§3.5） |

## 2. 插件 layer（架构层）与 kind（承载）

`layer` 表达架构角色，`kind` 表达承载方式（`in-process` / `sidecar` / `webview`，闭集），两者正交：

| layer | 运行位置 | 契约 | 例子 |
|---|---|---|---|
| `kernel` | 内核进程内 | `agent-loop-contract` 的 seam | `keel.context`、`keel.compaction` |
| `harness` | Harness 进程内 | 工具注册 + 服务 + 放置 | `keel.tasks`、`keel.fs`、`keel.approve` |
| `provider` | 独立进程 / sidecar | JSON-RPC / MCP / 自定义 | `keel.web-search`、`keel.memory`、`keel.oar` |
| `ui` | 客户端（NSH chunk / 签名包 / iframe） | 槽位与贡献（见 `plugin-ui-extension`） | `keel.ui`、`ui-task-board` |
| `service` | 独立服务 | gRPC | `layerfs` |

**工具面映射**：插件可以只给聚合工具加 action；只有安装期显式声明 `provides.tools` 才扩展工具面。

## 3. 完整清单

### 3.1 内核 seam（layer: kernel）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.context` | 上下文装配：指令、时间、文件引用、任务上下文、会话引用 | `agent-instructions`、`time-context`、`file-reference`、`session-reference` | W1 |
| `keel.compaction` | 自动压缩 + 工具结果修剪 + 图片外置 | `compaction-basic`、`tool-result-pruner`、`image-offload` | W1 |
| `keel.spill` | 长文本溢出到存储，上下文只留引用 | `spill-local`、`spill-policy` | W1 |
| `keel.tool-executor` | 工具执行管线：批量、并行、结果配对、超时 | `tools`、`tool-executor-native`、`timeout-policy` | W1 |
| `keel.system-prompt` | 系统提示装配（基础段 + 技能索引 + 任务上下文） | `system-prompt` | W1 |
| `keel.llm` | provider 中立调用 + 重试 + token 计量 + provider 适配器族 | `llm`、`llm-retry`、`token-meter`、`llm-deepseek*` | W1 |
| `keel.prompt-cache` | 稳定前缀与缓存边界 | （DSH 无显式对应） | W1 |
| `keel.checkpoint` | checkpoint 策略（何时落、保留多少） | `session-checkpoint-policy` | W2 |

### 3.2 环境（layer: service / harness / provider）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `layerfs`（service） | 内容寻址：file object + tree object + refcount/GC | `fs-local`/`fs-sandbox` 的字节层 | W1 |
| `keel.fs` | `read`/`write`/`edit`（内部搜索/补丁供 skills 与 task 使用，模型入口只有三个） | `fs`、`tool-fs`、`tool-fs-search`、`tool-str-replace-editor` | W1 |
| `keel.shell` | bash（gbash）+ 持久终端 | `shell`、`tool-bash`、`tool-bash-persistent`、`shell-env` | W1 |
| `keel.jobs` | 后台作业表、收养、孤儿回收 | `jobs`、`tool-jobs` | W1 |
| `keel.exec-surface` | 执行面契约与适配器（本地 / 远端；**自研抽象**） | 不照搬 `ssh`；契约由本仓定义 | W1 |
| `keel.working-directory` | 工作目录语义与切换 | `working-directory`、`tool-working-directory` | W1 |
| `keel.attachment` | 图片/文件附件 | `attachment-local` | W2 |
| `keel.deliverables` | 交付物登记 + 工作区变更 | `deliverables`、`workspace-changes` | W2 |
| `keel.subprocess` | 共享子进程服务 | `subprocess-local` | W2 |
| `keel.terminal` | 持久终端（provider） | `terminal` | W2 |

### 3.3 记忆与任务（layer: harness）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.tasks` | **all-in-one plan-plugin**：任务卡、目标、计划、待办、资源关联、fork 原点 | 覆盖 `plan-mode` + `tool-todo` + `goal*` | W1 |
| `keel.session-title` | 会话标题生成 | `session-title`、`session-title-llm` | W2 |
| `keel.session-query` | 历史会话检索/导出 | `session-query-sqlite`、`session-log-export` | W2 |
| `keel.memory` | 记忆（sidecar，v1 已交付 `keel.context-memory`） | 社区记忆/RAG | W2 |
| `keel.knowledge` | 知识库/RAG | 社区 `WeKnora`/`OpenViking` | 登记 |

> `goal`、`plan-mode`、`todo` **不设独立插件**（Owner 裁决 1）：它们的语义（持久目标、只读规划、
> 待办）由 `keel.tasks` 的事件模型统一承载，`goal` 的「自动续跑」由 `keel.supervisor`（§3.8）承接。

### 3.4 交互（layer: harness / ui）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.approve` | confirm/prompt/choice + 「其它」逃逸 | `user-approval`、`user-questions`、`tool-ask-user` | W1 |
| `keel.notify` | 多消费器通知 + 原生推送兜底 | `schedule`（部分） | W1 |
| `keel.permission` | 权限预设/策略 UI | `permission-presets` | W2 |
| `keel.commands` | slash commands + 命令反馈 | `commands`、`command-feedback`、`command-goal`、`command-compact` | W2 |
| `keel.feedback` | 消息/会话反馈 | `message-feedback` | W2 |

### 3.5 能力接入（layer: harness / provider）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.mcp` | 聚合工具 + gbash CLI shim | `mcp-client`、`mcp-manager`、`mcp-resources` | W1 |
| `keel.skills` | ls/view/manage + 订阅源 | `skill`、`skill-filesystem`、`tool-skill` | W1 |
| `keel.web-search` | 搜索/抓取（provider，v1 已交付） | `web`、`web-search-deepseek`、`web-fetch-http` | W2 |
| `keel.subagent` | 内部子代理：spawn/fork/resume/cancel/list/status | `subagent`、`subagent-spawn-in-process`、`subagent-fork-in-process`、`tool-subagent*` | W1 |
| `keel.oar` | 外部运行时（Codex/Claude/DSH/ACP） | `subagent-claude-code`、`subagent-codex`、`subagent-acp`、`subagent-dsh-sdk` | W2 |
| `keel.lsp` | 代码导航（provider） | `lsp` | 登记（内核稳定后） |
| `keel.browser-use` | 浏览器操作（provider） | `browser-use` | 登记：接 fde2e-bu（裁决 4） |
| `keel.computer-use` | 桌面操作（provider） | `computer-use` | 登记：接 fde2e-cu（裁决 4） |
| `keel.document` | Office→PDF（provider） | `document` | 登记 |
| `keel.ptc` | 程序化工具调用 | `ptc-runtime`、`tool-executor-ptc` | 登记 |
| `keel.workflow` | 模型自写编排 | `workflow-ptc`、`tool-workflow` | 登记 |

> **不做 `agent-team`**（另有结构方案）。子代理树的协调由 `keel.tasks` 的父子卡 +
> `keel.subagent` 的 `spawn/fork/resume/cancel/list/status` 承担；不引入独立的团队协调插件。

### 3.6 触发器（layer: harness）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.triggers` | cron/interval/webhook + HMAC + 去重 + 漏跑补偿 | `schedule` | W1 |
| `keel.schedule` | Host 定时任务/提醒（含 UI） | `schedule` | W2 |

### 3.7 插件基础设施（layer: harness）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.plugin-manager` | 安装/启停/卸载/包清单 | `plugin-manager`、`tool-plugin-manager`、`plugin-package-inventory` | W1（v1 已有） |
| `keel.plugin-market` | 市场索引 | `plugin-market` | W1（v1 已交付） |
| `keel.bundles` | 装配面：base / mode（serve、headless、tui、sdk） | `bundle/*` | W2 |
| `keel.presets` | agent preset 覆盖（工具/提示/技能） | `agent-preset-registry`、`preset` | W2 |
| `keel.settings` | 设置 + config 编辑 | `settings`、`config-editor` | W1 |
| `keel.credentials` | 凭据引用 + 授权 | `credentials`、`authorization`、`deepseek-account` | W2 |
| `keel.telemetry` | 指标 + OTel | `otel`、`session-telemetry-otel` | W2 |
| `keel.hmr` | 插件热重载 | `hmr` | W2 |
| `keel.testkit` | 无密钥测试台 / LLM mock-replay | `test-support` | W2 |
| `keel.storage` | 域数据存储 | `storage`、`storage-json`、`storage-domain` | W2 |
| `keel.identity` | 匿名 id | `identity` | W2 |

### 3.8 护栏与恢复（layer: harness）——延伸 `supervision` spec

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.supervisor` | **自动恢复**（崩溃/Docker 重启后自动 resume 全部非终态 run）、重试预算与退避、升级阶梯、重复工具提醒、单次超时策略、额度/成本护栏 | `repeat-tool-reminder`、`timeout-policy`、`goal-round-driver`（续跑部分） | **W1** |
| `keel.super-agent` | 可安装的超级代理（层级高于 main-agent）：恢复 main-agent、并行多个 main-agent 解决问题 | 无对应（我们自研） | W2 |

### 3.9 创造模式（layer: harness / ui，**内置**）

| id | 职责 | DSH 对应 | 波次 |
|---|---|---|---|
| `keel.creation` | **内置创造模式**：聚合本机所有插件的可扩展能力文档 → 机器可读目录；只读发现 + 脚手架 + 校验 + 审批安装 + 列举/删除；让 AI 现场写出**Native 插件**（NSH chunk 槽位贡献，而非只能 webview） | `tool-cordis` + `cordis-host-runner/client-runner` + 生成式目录 | **W1** |
| `ui-creation` | 创造模式面板（历史生命周期卡片、开发中插件状态） | `ui-cordis` | W1 |

创造模式的**聚合协议**（本仓独有，DSH 只聚合宿主自己的目录）：

- 每个插件 MUST 随包提供 `EXTENSIBILITY.md`（人读）与 `extensibility.json`（机读），声明
  「我能被怎么扩展」：提供的槽位、接受的配置、可挂 hook、可替换的 provider、示例与约束。
- `keel.creation` MUST 在装配期与运行期**聚合**这些文档（含第三方已装插件），形成本机目录，
  经只读发现工具交给 AI：AI 因此知道「本机装了哪些插件、它们的扩展点是什么、能做什么自定义」。
- 生成式目录的口径与 `plugins/keel.plugin-market/catalog.json` 同源（单一真值）。

### 3.10 UI 与槽位（layer: ui）

**槽位不由 Harness 内置，由插件提供；最大的槽位提供者是 `keel.ui`（第一方 NSH 客户端）**。
详细契约见 `plugin-ui-extension` spec。`keel.ui` 声明的基础槽位（示例，非 Harness 清单）：

| 槽位 | 形态（由槽位所有者声明允许集） | 典型贡献者 |
|---|---|---|
| `keel.ui.route` 页面/路由 | chunk / package / iframe | 各插件（技能、记忆、交付物…） |
| `keel.ui.nav` 导航项 | data | 同上 |
| `keel.ui.action` 工具条动作 | data | 同上 |
| `keel.ui.tool-renderer` 工具调用渲染 | chunk | 外部工具（`keel.oar`）、`keel.shell`、`keel.mcp` |
| `keel.ui.message-part` 消息部件 | chunk | 官方插件优先 |
| `keel.ui.sidebar` 侧栏面板 | chunk / iframe | 官方插件优先 |
| `keel.ui.settings-section` 设置分区 | chunk | 各插件 |
| `keel.tasks.widget` 任务卡部件 | chunk | 官方插件优先（槽位由 `keel.tasks` 声明） |
| `keel.ui.surface` 独立表面 | package（签名）/ iframe | 重型工具 |

第一方面板是这些槽位的**贡献物**（不是 Harness 的固定清单）：`ui-session`、`ui-tool-renderer`、
`ui-task-board`、`ui-notifications`、`ui-jobs`、`ui-skills`、`ui-subagent`、`ui-deliverables`、
`ui-settings`、`ui-plugin-market`、`ui-secret-mask`、`ui-trajectory`、`ui-creation`。

## 4. 与既有 COVERAGE / catalog 的对账

`plugins/keel.plugin-market/catalog.json` 是**唯一机读真源**（需补 `wave` 与统一 status 枚举）；
`COVERAGE.md` 逐行映射到 `keelPluginId`，本清单覆盖全部 `keelPluginId`。现状口径：COVERAGE 约
75 个 id、本清单 60 余项、catalog 约 60 个 `keelPluginId`，status 还多出 `partial`——三者不是
id 集合相等关系。已知冲突逐行处置：

| 既有 id | 本清单处置 | 说明 |
|---|---|---|
| `keel.goal` | absent | 目标并入 `keel.tasks`；自动续跑归 `keel.supervisor` |
| `keel.todo` | absent | 待办是 `keel.tasks` 的动作 |
| `keel.plan` | absent | 计划模式是任务卡视图，不设独立插件 |
| `keel.team` | absent | 不做团队协调插件（另有结构方案） |
| `keel.guard` | 合并 → `keel.supervisor` | 重复工具提醒/超时策略并入护栏 |
| `keel.workspace` | 合并 → Group | 放置与协作单元已在 `harness-core` |
| `keel.skill` | 改名 → `keel.skills` | 聚合工具同名 |
| `keel.preset` | 改名 → `keel.presets` | 复数统一 |
| `keel.mcp` | 保留，改聚合 | `mcp(...)` + CLI shim；禁止 `mcp__*` |
| `keel.subagent` | 保留，动作改 `agent.*` | `spawn/fork/resume/cancel/list/status` |
| `keel.exec-surface(ssh)` | 改自研契约 | 不照搬 DSH ssh |
| `keel.browser-use` / `keel.computer-use` | 登记 | 接 fde2e-bu / fde2e-cu |
| `keel.creation` | 保留（内置），主路径改 chunk | 创造模式见 §3.9 |

**仅存在于 catalog / COVERAGE 的 id**（W0.5 MUST 逐条处置，否则对账必失败）：

- **catalog 独有 19 个**：`keel.core`、`keel.session`、`keel.ssh`、`keel.sandbox`、`keel.sdk`、
  `keel.api`、`keel.client-ui`、`keel.host`、`keel.util`、`keel.acp`、`keel.desktop`、
  `keel.mobile-gateway`、`keel.llm.subscription`、`keel.plugin-gate`、`keel.session-migrate`、
  `keel.research.literature`、`keel.blender`、`keel.theme.glass`、`keel.subagent-adapter`
  → catalog 标 `out-of-scope` 或合并进现有 id，逐条给理由。
- **COVERAGE 独有 10 个**：`keel.boot`、`keel.cli`、`keel.context`、`keel.plugin-manager`、
  `keel.shell.pwsh`、`keel.skill-store`、`keel.skill.distill`、`keel.subagent.claude-code`、
  `keel.tools`、`keel.web` → catalog 补 `absent`/`merged` 行与理由（其中 `keel.context`、
  `keel.plugin-manager` 等已在本清单，补进 catalog 即可）。

W0.5 MUST 跑 catalog 对账脚本（COVERAGE §A/§B 行 → `keelPluginId`），差异逐条列出并以非零退出。

## 5. 与 DSH 的关键差异

1. **内核不是插件容器，loop 是契约**：只有 seam（context/compaction/executor/prompt/llm）插件化。
2. **插件粒度 ≠ 工具粒度**：模型只见恒定聚合工具面。
3. **槽位由插件提供**（`keel.ui` 最大），Harness 只提供**槽位注册机制**；DSH 的 slot catalog 是
   宿主自己生成的一份目录，我们的是「插件声明 + 聚合」。
4. **创造模式聚合的是全机插件的扩展性文档**（DSH 只聚合宿主自身的 tool/config/slot/api 目录）。
5. **执行面是自研契约**（裁决 3），不是 DSH 的 ssh。
6. **没有沙箱族、没有 Cordis/typert、没有 70 个 UI 包、存储是 PG 账本。**
7. **护栏是默认行为**（裁决 2）：崩溃即自动恢复、子代理可续、升级阶梯到 super-agent。

## 6. 已完成/待做速查

- **v1 已交付可复用**：`keel.plugin-manager`、`keel.plugin-market`、`keel.web-search`、
  `keel.context-memory`、v1 的 durable 审批与 `agentext` 生命周期。
- **W1（v2 核心）**：`layerfs`、`keel.fs`、`keel.shell`、`keel.jobs`、`keel.exec-surface`、
  `keel.working-directory`、`keel.tasks`、`keel.approve`、`keel.notify`、`keel.mcp`、`keel.skills`、
  `keel.subagent`、`keel.context`、`keel.compaction`、`keel.spill`、`keel.tool-executor`、
  `keel.system-prompt`、`keel.llm`、`keel.prompt-cache`、`keel.settings`、**`keel.supervisor`**、
  **`keel.creation` + `ui-creation`**、`keel.ui` 槽位机制 + `ui-task-board`、`ui-notifications`、
  `ui-jobs`、`ui-tool-renderer`、`ui-skills`。
- **W2**：`keel.checkpoint`、`keel.session-title`、`keel.session-query`、`keel.deliverables`、
  `keel.attachment`、`keel.memory`、`keel.commands`、`keel.permission`、`keel.feedback`、
  `keel.oar`、`keel.super-agent`、`keel.bundles`、`keel.presets`、`keel.credentials`、
  `keel.telemetry`、`keel.hmr`、`keel.testkit`、`keel.storage`、`keel.identity`、`keel.schedule`、
  `keel.subprocess`、`keel.terminal`、`ui-subagent`、`ui-deliverables`、`ui-secret-mask`。
- **登记（内核稳定后）**：`keel.lsp`、`keel.browser-use`（fde2e-bu）、`keel.computer-use`（fde2e-cu）、
  `keel.document`、`keel.knowledge`、`keel.ptc`、`keel.workflow`、`ui-trajectory`。
- **明确不做**：`agent-team`（另有方案）、`goal`/`plan-mode`/`todo` 独立插件（并入 plan-plugin）、
  Cordis、typert、sandbox 族、DSH ssh、70 个 UI 包、JSONL 会话存储。
