# agent-loop-contract Specification

## Purpose
Keel 的 agent 内核契约：一次 run 只通过注入的工具清单、system sections、message-parts 与 invoke 回调工作，内核不读文件、不连数据库、不选模型、不持有跨 run 状态；run 内注入物不可变、上下文只追加、记忆卫生只派生请求视图；fork/resume 以 eventSeq 为原点，外部内核按能力矩阵显式降级并以类型化错误拒绝不支持的能力。

## Requirements

### Requirement: 注入式 run 契约

每个 run 开始时，Harness MUST 向内核注入四样东西：**工具清单**（name/description/schema）、
**system sections**（有序文本段）、**message-parts**（有序消息片段）、**invoke 回调**
（`invoke(tool, args, ctx)`）。内核 MUST 只通过这四样工作；MUST NOT 读文件、连数据库、
连网络、起进程或选择模型。

#### Scenario: 内核可单测
- **WHEN** 用一个固定工具清单与固定消息集驱动内核
- **THEN** 不启动任何外部依赖即可完成一次 run，并产出可断言的事件流

#### Scenario: 换内核不换宿主
- **WHEN** 把一个符合本契约的外部内核接到同一套注入物上
- **THEN** Harness 侧的工具、任务、审批行为一致，只有事件流的 provider 字段不同

### Requirement: run 内注入物不可变

工具清单与 system sections MUST 在 run 内保持不可变，只在 `config.toml` 轮转后、下一次
run 时更新。每次 run MUST 记录 `(configHash, anchorEventSeq, toolManifestHash)` 三个值，
用于回答「这次 run 到底看到了什么」。message-parts MAY 在 run 内按追加语义增长。

#### Scenario: 中途加 MCP server 不改变清单
- **GIVEN** 一个 run 正在进行
- **WHEN** 运行中的 Harness 接入了一个新 MCP server
- **THEN** 本次 run 的工具清单哈希不变；新能力只能通过 `mcp` 聚合工具的动作面被感知

#### Scenario: 配置轮转后下一次 run 生效
- **GIVEN** 用户在 run 进行中改写了 `config.toml` 并触发轮转
- **WHEN** 下一次 run 启动
- **THEN** 使用新版本配置与新的工具清单，且 `anchorEventSeq` 指向轮转事件

### Requirement: 上下文只追加，不重写

上下文 MUST 是**有序 message-parts**；变化 MUST 以下一次请求前的追加进入（v1 的 `guide`
语义），MUST NOT 重写历史消息。每次请求 MUST 记录 `inputSeqs`（本次呈现给模型的输入
集合）与所用文件的版本集合。

#### Scenario: 引导输入不打断模型流
- **WHEN** 一个 `delivery:guide` 的输入在 turn 内到达
- **THEN** 它在所有 sibling tool results 提交之后、下一次模型请求之前追加，不打断当前流

#### Scenario: 请求快照可归属
- **WHEN** 回看一条 assistant 事件
- **THEN** 其 `inputSeqs` 精确指出本次请求呈现了哪些输入，不因后续追加而改变

### Requirement: 记忆卫生只派生请求视图，不重写账本

`keel.compaction` 的 tool-result-pruner 与 image-offload、`keel.spill` 的长文本溢出
MUST 只作用于**下一次请求的派生视图**（loop 的 in-memory context 与 runner 的 transcript
rebuild），MUST NOT 改写已提交的消息事件。被修剪/外置的内容 MUST 以**可恢复引用**留在
上下文中（存储 id、内容哈希、字节数、有界预览），MUST NOT 静默丢弃；存储失败时 MUST 保留
原文并留下可观测失败事实。`keel.prompt-cache` 的缓存边界 MUST 位于 system/tool 前缀之后、
transcript 之前，使消息修剪/外置/压缩不会使缓存前缀失效；系统提示与工具声明 MUST 在
turn 之间保持字节稳定（工具顺序与 schema 对象键顺序 MUST 规范化），且缓存策略 MUST 是
显式 opt-in（默认关闭时请求字节与未启用时一致）。

#### Scenario: 大工具结果被修剪后可恢复
- **WHEN** 一个超出保留窗口的大工具结果被修剪
- **THEN** 上下文只留下有界引用（存储 id、sha256、字节数、预览），按该引用读取存储得到逐字节原文；已提交的 tool result 事件不变

#### Scenario: 旧图片外置保留 blob 引用
- **WHEN** 一个图片引用落到图片窗口之外
- **THEN** 请求视图把它替换为命名 blobId 的文本引用，provider 不再上传/内联旧图；blob 仍可按引用恢复

#### Scenario: 溢出失败不丢内容
- **WHEN** spill 存储不可用或引用缺少 blob id
- **THEN** 原始消息保持在请求视图中，并产生可观测失败事实；run 不因记忆卫生失败而失败

#### Scenario: 稳定前缀不被消息修剪击穿
- **WHEN** 同一会话的相邻两次请求之间只有 transcript 被修剪/压缩
- **THEN** system/tool 前缀字节不变，缓存断点仍命中；规范化的工具清单顺序与 schema 键序不因注册顺序抖动

#### Scenario: 缓存默认关闭时请求不变
- **WHEN** 租户未显式开启 `agent.promptCache`
- **THEN** 请求体与未引入该能力时一致（不排序、不加 `cache_control`）；开启后才应用规范化与断点

### Requirement: 外部内核的能力矩阵与类型化拒绝

外部内核适配器 MUST 声明自己的能力集（是否支持流式、工具、审批、fork、resume、压缩），
MUST 在启动前拒绝不支持的能力，并返回类型化错误；MUST NOT 静默降级。外部内核的事件
MUST 归一化为本契约的事件流，原始 payload MUST 保留。

#### Scenario: 不支持 fork 的外部内核
- **WHEN** 调用方要求一个不支持 fork 的外部内核执行 fork
- **THEN** 启动前返回 `unsupported_capability`，不创建任何会话或进程

#### Scenario: 原始事件保留
- **WHEN** 外部内核产生一条原生事件
- **THEN** 归一化事件携带原始 payload 与来源标识，渲染器可回退到原始内容

### Requirement: fork 与 resume 以 eventSeq 为原点

fork MUST 以一个 `eventSeq` 为原点，创建新会话并继承该点之前的账本视图与文件视图。
resume MUST 从账本重建，不依赖进程内状态。fork MUST NOT 复制文件字节（字节按内容共享）。

#### Scenario: fork 原点精确
- **GIVEN** 会话在 `eventSeq=42` 处被 fork
- **WHEN** 子会话读取文件
- **THEN** 看到的是 42 之前引用到的 blob 集合，父会话之后的新写入不可见

#### Scenario: fork 不复制字节
- **WHEN** fork 一个引用 1GB 文件的任务
- **THEN** 子会话的文件视图只是新的引用集合，layerfs 中不产生第二份字节

### Requirement: 内核不持有跨 run 状态

内核 MUST 在 run 结束后释放所有会话相关内存；任何跨 run 的状态 MUST 在账本或 checkpoint
中。进程 MUST 能在任意时刻被杀，并由另一进程从账本继续。

#### Scenario: 杀进程后续跑
- **WHEN** 内核进程在 run 中途被 `kill -9`
- **THEN** 另一进程接管后从账本继续，已提交的工具结果不重复执行，未完成调用按幂等键重试
