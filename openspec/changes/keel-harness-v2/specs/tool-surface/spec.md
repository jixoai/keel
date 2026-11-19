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
`plugin_disabled`，工具名不消失。`creation` 工具由 Harness 内建持有，`keel.creation` 只提供 action
后端；停用不触发「插件工具注册副作用」的撤销范围。插件 MAY 在安装时通过 `provides.tools` 显式扩展工具面（安装审批、改变清单哈希、
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
- **WHEN** 运行工具注册表快照断言
- **THEN** 快照中不存在上表「删除」列的具体名字（精确名，不用子串 grep）；
  `spawn_subagent` / `creation_*` / `mcp__<server>__<tool>` 的注册点为 0

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
文件系统、跨平台）。gbash 的版本、协议帧、进程模型与许可证 MUST 冻结在
`docs/GBASH-CONTRACT.md`（pin `v0.0.41`；进程内 goroutine、无真实进程、`$$`=1）。
`timeout` MUST 是工具参数，MUST NOT 依赖 bash 的 `timeout` 命令，MUST NOT 使用上游
`ExecutionRequest.Timeout`（其语义是取消+杀，与 keel 冲突）。超时 MUST NOT kill 命令，
而是转入后台作业并返回超时信息与作业 id。

#### Scenario: 超时转后台
- **GIVEN** 一条命令以 `timeout=30s` 启动
- **WHEN** 30 秒后仍未结束
- **THEN** 命令转入后台作业（独立 Session+goroutine，非同一 session 的 `&`）继续执行，
  返回超时说明与可查询的作业 id，不丢已产生的输出

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
bash 执行空间的 layerfs MUST 挂在**非根挂载点**（`MountableFileSystem`）：gbash 在 session
初始化时向根 FS 写约 263 次虚拟命令桩（`/bin/*`、`/usr/bin/*`），直接把 layerfs 作为根会让
这些桩污染内容寻址层（`docs/GBASH-CONTRACT.md` §4.2，实测）。

#### Scenario: write 后 bash 立即可读
- **WHEN** 先 `write a.txt`，再 `bash cat a.txt`
- **THEN** 能读到新内容（写穿生效），不需要重新物化，且 layerfs 中不出现 gbash 的命令桩

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

后台作业 MUST 由 JobManager 统一登记（`nohup/jobs/ps` 视图是 keel 自研命令，读 JobManager）。
一个作业 MUST 独占一个独立 gbash `Session` 与独立 goroutine（同 session 的 `Exec` 串行且后台任务
出 `Exec` 即 cancel，故同 session `&` 不能承载跨调用存活）。其「pid」MUST 是 keel 作业 id，
MUST NOT 暴露 gbash 的 `$$`/`$!`（前者恒为 1，后者是虚拟标签）。JobManager MUST 提供全局与
每组上限、查询、输出引用（blob）与退出状态；超限 MUST 拒绝新作业，MUST NOT kill 已有作业。
作业持久化事实（id/命令/cwd/输出引用/退出码）MUST 落库；**因作业载体是进程内 goroutine，
Harness 重启 MUST 把未收敛作业一律收敛为终态（`exited`/`interrupted`）并保留可读输出**，
MUST NOT 声称「重新挂上仍在运行的作业」。

#### Scenario: 重启后恢复事实并收敛终态
- **WHEN** Harness 进程重启而一个后台作业此前未收敛
- **THEN** JobManager 从持久化事实恢复其查询与输出引用，状态收敛为 `interrupted`
  （原因标注 harness restart），MUST NOT 出现「仍在运行」的假状态

#### Scenario: 孤儿回收
- **WHEN** 一个会话被删除而其后台作业仍存活
- **THEN** 作业的 session context 被取消并按策略终止，作业被标记终态，进程内不留孤儿 goroutine

#### Scenario: 作业结束结算执行空间变更
- **WHEN** 一个后台作业写完文件后退出（其父 run 可能已结束）
- **THEN** 它以一条 `fs.capture`（payload 携带 `jobId`，不依赖 run lease）结算变更，
  见 layerfs「capture 默认实时、按批结算」
