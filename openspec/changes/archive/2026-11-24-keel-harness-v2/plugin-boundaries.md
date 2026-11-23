# Plugin Boundaries（逐插件边界与原则）

配套 `state-machines.md`：每个插件的**权威范围**、**明确不管的事**、**依赖方向**、
**它拥有哪台状态机**、**失败语义**。原则先立，表格随后。

## 0. 十一条跨插件原则

1. **责任分层**：插件只向上声明「我做什么」，不对同层声明「我这里安不安全」；组合语义由 Harness
   在装配期决定并注入。（反例：skills 的 patch 去问文件系统有没有写保护。）
2. **单一真值**：结构在 DB，字节在 layerfs（内容寻址），执行空间是可丢弃的物化视图。
3. **插件粒度 ≠ 工具粒度**：模型只见恒定聚合工具面（`bash`/`read`/`write`/`edit`/`mcp`/
   `skills`/`tasks`/`agent`/`creation`/`approve`/`notify`）；动态能力以 action 进入。
4. **状态即事实**：每个状态指名权威事实；非法转移 fail-closed 并留下拒绝事实（`state-machines.md`）。
5. **可逆副作用**：装载即注册可撤销副作用；卸载反序撤销，工具/hook/触发器/UI/VM/进程/端口零残留。
6. **幂等**：有副作用的调用带幂等键（`requestId`/`generationId`/`toolCallId`）；重试不产生第二次效果。
7. **有界**：输出有界（head/tail + blob 引用）、内存有界（VM/缓冲上限）、并发有界（配额与排队）。
8. **不越界**：不读其他插件的内部状态，不直连内核内部实现，不硬编码主题与视觉语言。
9. **单一插件运行时**：不引入 Cordis/typert 或第二套 DI/插件框架；等价语义用
   `agentext` + `servicegraph` 表达。
10. **默认自愈**：中断自动恢复；失败必须有 owner（supervisor → 父/主代理 → owner → super-agent）。
11. **文档是契约的一部分**：每个插件必须交 `extensibility.json` + `EXTENSIBILITY.md`，否则
    不可被创造模式利用（官方插件发布门禁直接拒绝）。

## 1. 内核 seam（layer: kernel）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.context` | 上下文装配顺序与来源（指令/时间/文件引用/任务上下文/会话引用） | 压缩策略、存储、技能内容 | A3 |
| `keel.compaction` | 阈值、切点、摘要、checkpoint 时机 | 工具执行、存储格式 | A3 |
| `keel.spill` | 溢出阈值与「引用化」（长内容只留引用） | 字节存储（layerfs 的） | A3 |
| `keel.tool-executor` | 工具批次、并行、结果配对、超时与取消传播 | 工具语义、业务含义 | A2 |
| `keel.system-prompt` | 系统提示段组装与顺序 | 技能索引内容（skills 提供的） | A2 |
| `keel.llm` | provider 适配、重试、token 计量、失败归类 | 上下文、工具 | A2 / G3 |
| `keel.prompt-cache` | 稳定前缀与缓存边界（工具清单/系统段不抖动） | 内容本身 | A2 |
| `keel.checkpoint` | 何时落 checkpoint、保留多少 | 压缩内容 | A3 |

失败语义（统一）：seam 失败 MUST 降级到「最小可用」并留事实（如摘要失败保留原上下文、
缓存失效退化为无缓存），MUST NOT 吞掉原始状态。

## 2. 环境（layer: service / harness / provider）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `layerfs` | 字节对象与 tree 对象、去重、引用计数、GC、租户配额 | 路径含义、任务归属、权限 | F1/F2 |
| `keel.fs` | `read`/`write`/`edit` 与**写前置条件**（搜索/补丁不单独暴露） | capture（shell 的）、任务归属（harness 的） | F3 |
| `keel.shell` | gbash 适配、`timeout` 参数、超时转后台、输出落 blob | 作业表（jobs 的）、安全策略（非目标） | F1 / D3 |
| `keel.jobs` | 作业登记、上限、查询、收养、孤儿回收 | 命令语义 | D3 |
| `keel.exec-surface` | **执行面自研契约**：`materialize` / `capture` 与其能力矩阵 | 远端具体实现（适配器负责） | F1/F2 |
| `keel.working-directory` | 工作目录语义与切换事实 | 权限、配额 | F1 |
| `keel.attachment` | 附件登记与引用 | 图片流水线（v1 blob 的） | C4 |
| `keel.deliverables` | 交付物登记与工作区变更投影 | 任务语义（tasks 的） | C4 |
| `keel.subprocess` | 共享子进程及生命周期 | 作业调度 | D3 |
| `keel.terminal` | 持久终端会话（provider） | 命令解释（shell 的） | F1 |

## 3. 记忆与任务（layer: harness）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.tasks` | **all-in-one plan-plugin**：任务卡、任务树、fork 原点、资源引用、工具调用归属强制 | 字节存储、执行、外部运行时的内部 | C1–C4 |
| `keel.session-title` | 标题生成与更新 | 会话真值 | A1 |
| `keel.session-query` | 历史检索与导出（读模型） | 真值 | — |
| `keel.memory` | 记忆读写与召回（sidecar） | 上下文装配（context 的） | H3 |
| `keel.knowledge` | 知识库/RAG（登记） | — | H3 |
| `keel.supervisor` | 自动恢复、重试预算、退避、升级阶梯、护栏触发与处置记录 | 具体处置动作（L1+ 代理做的） | G1–G4 |
| `keel.super-agent` | 多 main-agent 编排、结果收敛（插件，非内核） | 内核契约 | G2 |

## 4. 交互（layer: harness / ui）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.approve` | decision 生命周期、选项语义、「其它」逃逸与 `validated` provenance、超时持久化 | 界面渲染（UI 槽位的） | D1 |
| `keel.notify` | 通知与多消费器状态、ack 事实、原生推送兜底 | 审批 | D2 |
| `keel.permission` | 策略预设与其可见性 | 执行点（沿用 v1 已登记边界） | — |
| `keel.commands` | slash 命令注册、补全、命令反馈 | 工具 | E1 |
| `keel.feedback` | 消息/会话反馈采集 | 真值 | — |

## 5. 能力接入（layer: harness / provider）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.mcp` | MCP registry、`list/describe/call`、二进制 content block、gbash CLI shim 共用同一 registry | 模型可见工具清单（恒定） | H1 |
| `keel.skills` | 技能索引/正文/管理/订阅源、内容哈希 | 脚本执行（走 bash 策略） | H3 |
| `keel.web-search` | 搜索与抓取（provider，v1 已交付） | 渲染 | — |
| `keel.subagent` | 内部子代理 `spawn/fork/resume/cancel/list/status`、独立会话与卡 | 外部运行时 | A2 / C3 |
| `keel.oar` | 外部运行时适配、带外 record stream、typed rejection | **任务图**（外部调用归属发起卡、不建子卡；内部细节只进渲染器） | H2 |
| `keel.lsp` | 代码导航（provider） | — | 登记 |
| `keel.browser-use` | 浏览器操作（provider，接 fde2e-bu） | — | 登记 |
| `keel.computer-use` | 桌面操作（provider，接 fde2e-cu） | — | 登记 |
| `keel.document` | Office→PDF（provider） | — | 登记 |
| `keel.ptc` / `keel.workflow` | 程序化工具调用 / 模型自写编排 | — | 登记 |

## 6. 触发器（layer: harness）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.triggers` | cron/interval/webhook、HMAC、幂等去重、漏跑补偿、每触发器并发上限 | 工作流编排（非目标） | H4 |
| `keel.schedule` | Host 定时任务与提醒 | 会话真值 | H4 |

## 7. 插件基础设施（layer: harness）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.plugin-manager` | 安装/启停/卸载/包清单/审计、durable 审批 | 槽位与 UI | E1 |
| `keel.plugin-market` | 市场索引（单一真值：catalog.json） | 托管 | E1 |
| `keel.bundles` | base/mode 装配面与 row id overlay | 插件实现 | E1 |
| `keel.presets` | agent preset 覆盖（工具/提示/技能） | base/bundle 定义 | E1 |
| `keel.settings` | 设置 schema 与编辑面 | 生效点（各插件自己的） | A4 |
| `keel.credentials` | 凭据引用与授权 | 加密实现细节 | A4 |
| `keel.telemetry` | 指标与 OTel span | 业务语义 | — |
| `keel.hmr` | 热重载与失败回滚（不打断 run） | 会话与账本 | E1 |
| `keel.testkit` | 无密钥测试台、mock-replay | 产品路径 | — |
| `keel.storage` | 域数据表（非会话） | 会话账本 | — |
| `keel.identity` | 匿名 id | 身份/授权 | — |
| `keel.creation` | **聚合扩展面文档**、只读发现、脚手架、校验、审批安装、列举/删除 | 插件实现质量、槽位定义（属提供者） | I-创造模式 |

## 8. UI 与槽位（layer: ui）

| id | 管什么 | 不管什么 | 状态机 |
|---|---|---|---|
| `keel.ui` | 槽位声明与贡献装载、主题令牌、桥（`keel.plugin.*` ↔ NSH `host.call`）与能力裁剪、目标降级 | 插件业务逻辑 | E3/E4 |
| `ui-*`（task-board / notifications / jobs / tool-renderer / skills / subagent / deliverables / settings / plugin-market / secret-mask / creation / trajectory） | 各自投影视图与交互 | 真值（只消费投影） | E3 |

## 9. 边界冲突的判定规则（写进 review checklist）

1. 一个能力被两个插件都声称拥有 → **由 Harness 装配期裁决**，不靠协商。
2. 一个插件需要另一个插件的行为保证 → **改由 Harness 注入**，不是加直连依赖。
3. 一个状态只有内存版本 → **判不合格**，必须落事实。
4. 一个副作用没有反向撤销路径 → **判不合格**。
5. 一个失败没有 owner（护栏/阶梯） → **判不合格**。
6. **失败 owner 默认阶梯**：supervisor → 父/主代理 → owner → super-agent。特殊项：进程类失败
   （layerfs / jobs / exec-surface / sidecar）→ JobManager 收养或标 `lost`；外部运行时失败 →
   网关重连策略 + `lost`；`keel.supervisor` 自身失败 → owner（不得自恢复）；`keel.super-agent`
   失败 → owner。
