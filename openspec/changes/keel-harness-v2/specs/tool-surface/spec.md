# Tool Surface（恒定工具面、聚合工具与破坏性迁移）

本 spec 定义模型可见工具集合、聚合工具的 action 表、bash 主桥、MCP 暴露方式与工具契约模板。
**Owner 裁决（2026-11-11）：直接破坏性开发**——v2 不保留 v1 工具名的兼容 shim。

## 规范性表

### v1 工具名迁移表

| v1 工具名 | 终局 | 处置 |
|---|---|---|
| `spawn_subagent` | `agent.spawn` | 删除；显式废止 v1 `plugin-system` 的「兼容红线」 |
| `creation_scaffold` | `creation.scaffold` | 删除 |
| `creation_validate` | `creation.validate` | 删除 |
| `creation_install` | `creation.install` | 删除 |
| `creation_list` | `creation.list` | 删除 |
| `creation_remove` | `creation.remove` | 删除 |
| `skill` | `skills.view` | 删除 |
| `clock` / `echo` | 测试工具 | 移出模型面，只存在于 `keel.testkit` |
| `example_status` / `example_audit_tail` | 示例插件工具 | 移出内建面，只随示例插件 `provides.tools` |
| `mcp__<server>__<tool>` | `mcp.call` | 删除；全仓只允许在偏离登记与测试夹具中出现 |
| `keel_web-search_search` 等插件工具 | 保留 | 走 `provides.tools`（安装期显式扩展） |

### 聚合工具 action 表

| 工具 | actions |
|---|---|
| `bash` | `run`（命令、cwd、timeout、env 白名单）、`jobs`（list/status/adopt/kill） |
| `read` / `write` / `edit` | 单一动作（参数见 file-plugin 契约） |
| `mcp` | `list`、`describe`、`call` |
| `skills` | `ls`、`view`、`manage`（create/patch/put/delete/write_file/remove_file）、`sources`（list/subscribe/unsubscribe/pin） |
| `tasks` | `create`、`update`、`attach`、`detach`、`close`、`list`、`show`、`note` |
| `agent` | `spawn`、`fork`、`resume`、`cancel`、`list`、`status` |
| `approve` | `confirm`、`prompt`、`choice` |
| `notify` | `push`（含消费者声明）、`list`、`ack` |
| `creation` | `catalog`（只读聚合目录）、`scaffold`、`validate`、`install`、`list`、`remove` |

## ADDED Requirements

### Requirement: 内建工具面恒定（11 个）

模型可见的**内建**工具集合 MUST 恒定为：`bash`、`read`、`write`、`edit`、`mcp`、`skills`、
`tasks`、`agent`、`approve`、`notify`、`creation`。**动态能力**（MCP server、技能、任务、订阅源）
MUST 通过 action 进入，MUST NOT 为其新增工具名。工具总数 MUST 与接入的 MCP server 数、技能数、
任务数无关。`creation` MUST 始终存在于内建面；`keel.creation` 停用时其 actions MUST 返回
`plugin_disabled`，工具名不消失。插件 MAY 在安装时通过 `provides.tools` 显式扩展工具面（安装审批、改变清单哈希、
面板可见）；MUST NOT 在运行中静默增删。

#### Scenario: 挂 100 个 MCP server 工具面不变
- **WHEN** Harness 装配了 100 个 MCP server
- **THEN** 模型可见工具清单仍为 11 个内建工具，清单哈希不变

#### Scenario: 插件扩展是显式变更
- **WHEN** 安装一个声明了 `provides.tools` 的插件并通过审批
- **THEN** 工具清单新增该工具、清单哈希变化并可见；卸载后恢复；运行中不静默出现或消失

#### Scenario: 终局工具集断言
- **WHEN** 运行工具面断言（G0）
- **THEN** 模型可见集合精确等于 11 个内建工具 ∪ 已审批的 `provides.tools`，多一个少一个都失败

### Requirement: 从 v1 工具名破坏性迁移

v1 已在模型面暴露的工具 MUST 按「v1 工具名迁移表」迁移；旧名 MUST 删除（无 shim、无别名、不做双轨）。

#### Scenario: 旧名不可调用
- **WHEN** 模型调用 `spawn_subagent` 或 `mcp__fs__read`
- **THEN** 返回 `unknown_tool`，不路由到新 action，不留兼容层

#### Scenario: 迁移表可机检
- **WHEN** 运行迁移断言
- **THEN** 代码中不存在上表「删除」列的工具名（测试夹具白名单除外），`grep` 零命中

### Requirement: 各聚合工具的 action 表（闭集）

每个聚合工具的 action MUST 是闭集（见「聚合工具 action 表」）；未列出的 action MUST 拒绝并给类型化错误。

#### Scenario: 未列出的 action
- **WHEN** 调用 `skills {action:"delete_everything"}`
- **THEN** 返回 `unknown_action` 与可用 action 列表，不产生副作用

### Requirement: agent 聚合工具只用于内部子代理

`agent` MUST 提供上表动作，只用于 Keel 内部子代理（独立会话与任务卡）。外部运行时
（Codex/Claude/DSH）MUST 通过 provider 工具接入（如 `keel.oar`），MUST NOT 伪装成内部 agent、
MUST NOT 出现在 `agent list`。外部运行时的调用 MUST **归属发起它的任务卡**（作为工具调用事实），
MUST NOT 为它建子卡、MUST NOT 把其内部细节写进任务图。

#### Scenario: fork 出并行子代理
- **WHEN** 调用 `agent {action:"fork", eventSeq:42}`
- **THEN** 创建独立会话与任务卡（`parentTaskId` 指向原点卡），文件视图来自 42 之前的前缀

#### Scenario: 外部运行时不冒充内部 agent
- **WHEN** 一个 OAR 外部运行时被调用
- **THEN** 它以外部工具的形式出现并有专用渲染器，不出现在 `agent list`，不产生子卡

### Requirement: bash 是主桥

Agent MUST 能通过 `bash` 完成系统操作；bash MUST 由 gbash 提供（可注入自定义命令、自定义
文件系统、跨平台）。gbash 的版本、协议帧、进程模型与许可证 MUST 在 W0 契约冻结中 pin 并写入
`docs/GBASH-CONTRACT.md`。`timeout` MUST 是工具参数，MUST NOT 依赖 bash 的 `timeout` 命令。
超时 MUST NOT 直接 kill 命令，而是转入后台并返回超时信息、终端信息与后台 pid。

#### Scenario: 超时转后台
- **GIVEN** 一条命令以 `timeout=30s` 启动
- **WHEN** 30 秒后仍未结束
- **THEN** 命令转入后台继续执行，返回超时说明与可查询的作业 id，不丢输出

#### Scenario: 契约 pin 后才可开工
- **WHEN** `docs/GBASH-CONTRACT.md` 未冻结
- **THEN** `bash` 的实现任务保持阻塞，不得以"先写再说"绕过

### Requirement: 输出必须有界

工具结果 MUST 只携带 head/tail 摘要与 blob 引用；完整输出 MUST 流式写入 layerfs。单次工具结果
进入模型上下文的部分 MUST 有固定上限（按字节或 token）。MUST NOT 因输出过大撑爆上下文或进程内存。

#### Scenario: 超大输出
- **WHEN** 命令产生 200MB stdout
- **THEN** 进程内存不随输出线性增长，模型看到 head/tail 与可再读的引用

### Requirement: MCP 只经聚合工具与 CLI 暴露

MCP 服务 MUST 通过 `mcp` 聚合工具（`list` / `describe` / `call`）或 gbash 的 `mcp` CLI 暴露。
MUST NOT 生成 `mcp__<server>__<tool>` 形式的硬编码工具名（这是**对 PI 命名的有意偏离**，
理由＝恒定工具面；偏离 MUST 登记在 `docs/PI-PARITY.md`）。聚合工具与 CLI MUST 共用同一个
registry，CLI 只是 shim。二进制内容 MUST 以 content block 返回，MUST NOT 经 stdout 传输。

#### Scenario: 服务增删不影响清单
- **WHEN** 一个 MCP server 被移除
- **THEN** 工具清单不变，`mcp list` 不再列出它，模型仍可用 `mcp` 调用其它 server

#### Scenario: 偏离已登记
- **WHEN** 审查 `docs/PI-PARITY.md`
- **THEN** 有「MCP 工具命名」偏离条目，写明理由、影响面与验收方式

### Requirement: read/write/edit 是薄接口

`read`/`write`/`edit` MUST 只是 file-plugin 的薄接口：解析 `file_manifest`（**会话作用域**）、
调用 layerfs、追加 `fs.write` 事件、并**写穿到执行空间**（若已物化）。它们 MUST NOT 引入第二套
写路径，MUST NOT 在工具层做版本合并或冲突策略（冲突语义属于 file-plugin 契约）。

#### Scenario: write 后 bash 立即可读
- **WHEN** 先 `write a.txt`，再 `bash cat a.txt`
- **THEN** 能读到新内容（写穿生效），不需要重新物化

#### Scenario: read 支持分页
- **WHEN** 读取一个超大文本文件
- **THEN** 支持 offset/limit 分页，默认返回有界片段并标注总行数

### Requirement: 工具契约模板

每个工具 MUST 声明并实现：幂等键（有副作用的调用）、超时语义、取消传播、输出截断策略、
类型化错误码、内存档位（该工具允许的最大驻留内存）。MUST NOT 让插件自行发明错误形态。
内核侧 `AgentTool` 若无这些字段，MUST 由 Harness 包装层补齐并在 tasks 中单列接口扩展任务。

#### Scenario: 重试不重复副作用
- **WHEN** 一次有副作用的工具调用因网络/进程故障被重试
- **THEN** 相同幂等键只产生一次效果，重复调用返回首次结果

#### Scenario: 取消传播
- **WHEN** 用户在运行中取消一个 run
- **THEN** 正在执行的工具收到取消信号，可清理资源并返回 cancelled 终态，不留下孤儿进程

### Requirement: JobManager 统一管理后台作业

后台作业 MUST 由 JobManager 统一登记（围绕 `nohup/jobs/ps` 的视图）。JobManager MUST 提供全局与
每组上限、查询、输出引用、退出状态与收养语义；孤儿进程 MUST 可回收。超限 MUST 拒绝新作业，
MUST NOT kill 已有作业。

#### Scenario: 重启后收养
- **WHEN** Harness 进程重启而一个后台作业仍在运行
- **THEN** JobManager 能重新登记该作业并继续查询其状态与输出

#### Scenario: 孤儿回收
- **WHEN** 一个会话被删除而其后台作业仍存活
- **THEN** 作业被标记并按策略终止，进程表中不留孤儿
