# LayerFS（内容寻址的字节层）

本 spec 定义 layerfs 的对象模型、路径归属、流式约束、去重与回收、capture 策略与执行空间
契约。结构事实（路径、任务、引用）见 `task-log`；本 spec 只管字节。

## ADDED Requirements

### Requirement: 内容寻址的对象模型

layerfs MUST 只提供两种对象：**file object**（文件字节，按 `sha256(content)` 寻址）与
**tree object**（目录项列表，按规范化条目的哈希寻址）。对象 MUST 不可变；相同内容 MUST
只存一份。

#### Scenario: 相同内容只存一份
- **WHEN** 两个任务写入字节完全相同的文件
- **THEN** layerfs 只保存一份字节，两个任务各自持有对该 hash 的引用

#### Scenario: 目录树去重
- **WHEN** 一个目录内容与另一个任务中的目录完全一致
- **THEN** 两个任务引用同一个 tree hash，不重复存储目录项

### Requirement: 路径语义不属于 layerfs

layerfs MUST NOT 认识路径、任务或会话。`路径 → hash` MUST 由 DB 的 `file_manifest` 持有，
每次写 MUST 追加一条 `fs.write{taskId, path, hash, mode}` 账本事件。读文件 MUST 由
file-plugin 先解析 manifest，再向 layerfs 取字节。

#### Scenario: 同一字节两个路径
- **WHEN** 任务中 `a/logo.png` 与 `b/logo.png` 内容相同
- **THEN** manifest 有两条路径记录，layerfs 只有一个 file object

#### Scenario: layerfs 不接受路径参数
- **WHEN** 客户端向 layerfs 传入包含 `/` 的路径
- **THEN** 接口拒绝该参数（找不到对应能力），路径解析只能发生在 Harness/file-plugin 侧

### Requirement: 流式读写与内存上限

layerfs 的读写 MUST 为流式分块；单次请求的缓冲 MUST 有固定上限。MUST NOT 把整个文件读入
内存后再处理。大文件（图片、构建产物、日志）MUST 走同一条流式路径。

#### Scenario: 大文件不整段进内存
- **WHEN** 上传或读取一个 1GB 文件
- **THEN** 进程 RSS 不随文件大小线性增长，分块缓冲不超过配置上限

#### Scenario: 大输出落库
- **WHEN** bash 产生 200MB 输出
- **THEN** 输出流式写入 layerfs，工具结果只携带 head/tail 与 blob 引用

### Requirement: 去重、引用计数与回收

layerfs MUST 为对象维护引用计数；引用归零且超过保留窗口的对象 MUST 可被 GC。GC MUST
并发安全，且 MUST NOT 回收仍被任一 manifest 或账本前缀引用的对象。

#### Scenario: fork 共享不误回收
- **WHEN** 父会话被删除但其 blob 仍被 fork 出的子会话引用
- **THEN** GC 不回收这些 blob

#### Scenario: 无引用后回收
- **WHEN** 一个 blob 的所有引用都归零并超过保留窗口
- **THEN** GC 回收该对象，后续按该 hash 读取返回 not_found

### Requirement: capture 策略默认实时且增量

执行空间产生的文件 MUST 通过 `fs.capture` 进入 layerfs。默认模式 MUST 为
`per-tool-call`：在每次工具调用结束后结算变更集。实现 MUST 使用 stat 缓存或文件监视
只对变化文件计算哈希，MUST NOT 全树重扫。忽略清单默认 MUST 只包含 `.git/objects`。
策略 MUST 可由 `config.toml` 覆写为 `checkpoint` 或 `manual`。

#### Scenario: 安装依赖后的增量结算
- **GIVEN** 一个任务执行 `npm install` 写入十万个文件
- **WHEN** 下一次工具调用结束触发 capture
- **THEN** 只对发生变化的文件计算哈希；未变化文件不产生新对象或新的 manifest 条目

#### Scenario: 配置关闭实时 capture
- **GIVEN** 子代理的 `config.toml` 设置 `fs.capture.mode=checkpoint`
- **WHEN** 它连续执行多条命令
- **THEN** 只有任务完成/子代理返回/显式 checkpoint 时才结算变更

### Requirement: 执行空间是物化视图

执行空间（bash 真正运行的地方）MUST 被视为可丢弃的物化视图。任务开始时 Harness MUST
按 manifest 物化所需文件；平台 MUST NOT 承诺对执行空间做全量镜像。执行面适配器 MUST
实现 `materialize(manifest)` 与 `capture(handle, since)` 两个操作；远端适配器可以只返回
增量（路径 + 哈希），Harness 不得假设能扫盘。

#### Scenario: 执行空间丢失
- **WHEN** 本地执行空间被清空而任务仍在进行
- **THEN** Harness 能按 manifest 重新物化，任务从账本继续，不依赖磁盘残留

#### Scenario: 远端执行面只给增量
- **WHEN** 远端适配器返回变更路径与哈希
- **THEN** Harness 按增量更新 manifest，不要求远端提供全量目录扫描

### Requirement: 配额与错误分类

layerfs MUST 按 group/租户执行存储字节配额；超限 MUST 返回可区分的错误（quota /
not_found / io / unavailable），Harness MUST 能把错误映射为可解释的用户提示。

#### Scenario: 超配额可解释
- **WHEN** 写入会使租户超过存储配额
- **THEN** 返回 `quota_exceeded` 与当前用量，写入不产生部分对象
