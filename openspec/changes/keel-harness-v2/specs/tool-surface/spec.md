# Tool Surface（恒定工具面与聚合工具）

本 spec 定义模型可见的工具集合、聚合工具模式、bash 主桥、MCP 暴露方式与工具契约模板。
工具的实现分布见各插件；本 spec 只约束「模型看到什么」。

## ADDED Requirements

### Requirement: 内建工具面恒定

模型可见的**内建**工具集合 MUST 保持恒定：`bash`、`read`、`write`、`edit`、`mcp`、
`skills`、`tasks`、`agent`、`approve`、`notify`。**动态能力**（MCP server、技能、任务、
订阅源）MUST 通过聚合工具的动作（action 参数）进入，MUST NOT 为其新增工具名。工具总数
MUST 与接入的 MCP server 数、技能数、任务数无关。

插件 MAY 在安装时通过 `provides.tools` 显式扩展工具面（沿用 `plugin-system` 的工具发布名
规则）；该扩展 MUST 经过安装审批、MUST 改变工具清单哈希、MUST 在面板可见，MUST NOT 在
运行中静默增删。这是**有意的 schema 变更**，不属于「动态能力」。

#### Scenario: 挂 100 个 MCP server 工具面不变
- **WHEN** Harness 装配了 100 个 MCP server
- **THEN** 模型可见工具清单仍为恒定集合，清单哈希不变

#### Scenario: 新增能力不新增工具
- **WHEN** 需要支持「列出订阅源里的技能」
- **THEN** 通过 `skills` 聚合工具新增一个 action 实现，而不是新增 `list_skill_sources` 工具

#### Scenario: 插件扩展是显式变更
- **WHEN** 安装一个声明了 `provides.tools` 的插件并通过审批
- **THEN** 工具清单新增该工具、清单哈希变化并可见；卸载后恢复；运行中不会静默出现或消失

### Requirement: bash 是主桥

Agent MUST 能通过 `bash` 完成系统操作；bash MUST 由 gbash 提供（可注入自定义命令、
自定义文件系统、跨平台）。`timeout` MUST 是工具参数，MUST NOT 依赖 bash 的 `timeout` 命令。
超时 MUST NOT 直接 kill 命令，而是转入后台并返回超时信息、终端信息与后台 pid。

#### Scenario: 超时转后台
- **GIVEN** 一条命令以 `timeout=30s` 启动
- **WHEN** 30 秒后仍未结束
- **THEN** 命令转入后台继续执行，返回超时说明与可查询的作业 id，不丢输出

#### Scenario: 自定义命令可注入
- **WHEN** Harness 向 gbash 注入一个自定义命令
- **THEN** Agent 在 `bash` 中直接可用，无需新增工具定义

### Requirement: 输出必须有界

工具结果 MUST 只携带 head/tail 摘要与 blob 引用；完整输出 MUST 流式写入 layerfs。
单次工具结果进入模型上下文的部分 MUST 有固定上限（按字节或 token），MUST NOT 因输出过大
撑爆上下文或进程内存。

#### Scenario: 超大输出
- **WHEN** 命令产生 200MB stdout
- **THEN** 进程内存不随输出线性增长，模型看到 head/tail 与可再读的引用

### Requirement: MCP 只经聚合工具与 CLI 暴露

MCP 服务 MUST 通过 `mcp` 聚合工具（`list/describe/call`）或 gbash 的 `mcp` CLI 暴露。
MUST NOT 生成 `mcp__<server>__<tool>` 形式的硬编码工具名。聚合工具与 CLI MUST 共用同一个
registry，CLI 只是 shim。二进制内容 MUST 以 content block 返回，MUST NOT 经 stdout 传输。

#### Scenario: 服务增删不影响清单
- **WHEN** 一个 MCP server 被移除
- **THEN** 工具清单不变，`mcp list` 不再列出它，模型仍可用 `mcp` 动作调用其它 server

#### Scenario: 二进制内容
- **WHEN** 一个 MCP 工具返回图片
- **THEN** 通过 content block 交给模型/前端，不经过 bash stdout

### Requirement: agent 聚合工具只用于内部子代理

`agent` MUST 提供 `spawn` / `fork` / `resume` / `cancel` / `list` 动作，只用于 Keel 内部
子代理（有独立会话与任务卡）。外部运行时（Codex/Claude/DSH）MUST 通过 provider 工具接入
（如 `keel.oar`），MUST NOT 伪装成内部 agent、MUST NOT 出现在 `agent list` 中。

#### Scenario: fork 出并行子代理
- **WHEN** 调用 `agent {action:"fork", eventSeq:42}`
- **THEN** 创建独立会话与任务卡（`parentTaskId` 指向原点卡），文件视图来自 42 之前的前缀

#### Scenario: 外部运行时不冒充内部 agent
- **WHEN** 一个 OAR 外部运行时被调用
- **THEN** 它以外部工具的形式出现并有专用渲染器，不出现在 `agent list`，不产生任务卡

### Requirement: read/write/edit 是薄接口

`read`/`write`/`edit` MUST 只是 file-plugin 的薄接口：解析 `file_manifest`、调用 layerfs、
追加 `fs.write` 事件。它们 MUST NOT 引入第二套写路径，MUST NOT 在工具层做版本合并或冲突
策略（冲突语义属于 file-plugin 契约）。

#### Scenario: 写路径唯一
- **WHEN** 用 `write` 或 `edit` 修改文件
- **THEN** 产生的 `fs.write` 事件与 bash capture 产生的变更记录结构一致

#### Scenario: read 支持分页
- **WHEN** 读取一个超大文本文件
- **THEN** 支持 offset/limit 分页，默认返回有界片段并标注总行数

### Requirement: 工具契约模板

每个工具 MUST 声明并实现：幂等键（有副作用的调用）、超时语义、取消传播、输出截断策略、
类型化错误码、内存档位（该工具允许的最大驻留内存）。MUST NOT 让插件自行发明错误形态。

#### Scenario: 重试不重复副作用
- **WHEN** 一次有副作用的工具调用因网络/进程故障被重试
- **THEN** 相同幂等键只产生一次效果，重复调用返回首次结果

#### Scenario: 取消传播
- **WHEN** 用户在运行中取消一个 run
- **THEN** 正在执行的工具收到取消信号，可清理资源并返回 cancelled 终态，不留下孤儿进程

### Requirement: JobManager 统一管理后台作业

后台作业 MUST 由 JobManager 统一登记（围绕 `nohup/jobs/ps` 的视图）。JobManager MUST
提供全局与每组上限、作业查询、输出引用、退出状态与收养语义；孤儿进程 MUST 可被回收。
超限时 MUST 拒绝新作业，MUST NOT kill 已有作业。

#### Scenario: 重启后收养
- **WHEN** Harness 进程重启而一个后台作业仍在运行
- **THEN** JobManager 能重新登记该作业并继续查询其状态与输出

#### Scenario: 孤儿回收
- **WHEN** 一个会话被删除而其后台作业仍存活
- **THEN** 作业被标记并按策略终止，进程表中不留孤儿
