# layerfs Specification

## Purpose
TBD - created by archiving change keel-harness-v2. Update Purpose after archive.

## Requirements

### Requirement: 内容寻址的对象模型

layerfs MUST 只提供两种对象：**file object**（文件字节，按 `sha256(content)` 寻址）与
**tree object**（目录项列表，按规范化条目哈希寻址）。对象 MUST 不可变；相同内容 MUST 只存一份。

#### Scenario: 相同内容只存一份
- **WHEN** 两个会话写入字节完全相同的文件
- **THEN** layerfs 只保存一份字节，两个会话各自持有对该 hash 的引用

#### Scenario: 目录树去重
- **WHEN** 一个目录内容与另一个会话中的目录完全一致
- **THEN** 两个会话引用同一个 tree hash，不重复存储目录项

### Requirement: 路径语义属于 DB，且是会话作用域

layerfs MUST NOT 认识路径、会话或任务。`路径 → hash` MUST 由 DB 持有：

```
file_manifest(session_id, path, hash, mode, size, updated_seq, last_task_id)
session_snapshot(session_id, seq, tree_hash)
```

- manifest 的键 MUST 是 `(session_id, path)`；`task_id` 只作**归因**（谁最后写了它），
  MUST NOT 作为隔离边界。
- `session_snapshot` MUST 在事件 seq 处记录 tree hash；fork MUST 以它做 O(1) 继承。
- 显式写（`write`/`edit`）MUST 追加 `fs.write{sessionId, taskId, path, hash, mode}` 事件；
  capture MUST 追加**一条批事件** `fs.capture{sessionId, taskId, baseSeq, count, treeHash}`。

#### Scenario: 同一字节两个路径
- **WHEN** 会话中 `a/logo.png` 与 `b/logo.png` 内容相同
- **THEN** manifest 有两条路径记录，layerfs 只有一个 file object

#### Scenario: 任务只做归因
- **WHEN** 任务 A 写了 `src/a.ts`，任务 B 在同一会话里读它
- **THEN** B 能读到；manifest 的 `last_task_id` 是 A，但文件不因任务边界而不可见

#### Scenario: fork O(1) 继承
- **WHEN** 从 `session_snapshot(session_id, seq)` fork 出子会话
- **THEN** 子会话按 tree hash 解析文件视图，不复制 manifest 行、不复制字节

### Requirement: 流式读写与内存上限

layerfs 的读写 MUST 为流式分块；单次请求的缓冲 MUST 有固定上限。MUST NOT 把整个文件读入内存后
再处理。大文件（图片、构建产物、日志）MUST 走同一条流式路径。

#### Scenario: 大文件不整段进内存
- **WHEN** 上传或读取一个 1GB 文件
- **THEN** 进程 RSS 不随文件大小线性增长，分块缓冲不超过配置上限

#### Scenario: 大输出落库
- **WHEN** bash 产生 200MB 输出
- **THEN** 输出流式写入 layerfs，工具结果只携带 head/tail 与 blob 引用

### Requirement: 去重、引用计数与回收

layerfs MUST 为对象维护引用计数；引用归零且超过保留窗口的对象 MUST 可被 GC。GC MUST 并发安全，
MUST NOT 回收仍被任一 manifest、snapshot 或账本前缀引用的对象。

#### Scenario: fork 共享不误回收
- **WHEN** 父会话被删除但其 blob 仍被 fork 出的子会话引用
- **THEN** GC 不回收这些 blob

#### Scenario: 无引用后回收
- **WHEN** 一个 blob 的所有引用都归零并超过保留窗口
- **THEN** GC 回收该对象，后续按该 hash 读取返回 not_found

### Requirement: capture 默认实时、按批结算

执行空间产生的变更 MUST 通过 `fs.capture` 进入 layerfs。默认模式 MUST 为 `per-tool-call`：
每次工具调用结束后结算变更集，MUST 使用 stat 缓存或文件监视只对变化文件计算哈希，MUST NOT 全树
重扫。**一次结算 MUST 只产生一条批事件**（`baseSeq` / `count` / `treeHash`），manifest MUST
批量 upsert 并以该事件 seq 作为 `updated_seq`，MUST NOT 为每个文件单独追加事件。忽略清单默认
MUST 只包含 `.git/objects`。
run 内结算 MUST 携带 run fence。后台作业（无 run lease）产生的变更 MUST 由 Harness 以**系统事实**
结算 `fs.capture`：payload 携带 `jobId` 归因，MUST NOT 伪造 owner token。

#### Scenario: 安装依赖只产生一条事件
- **GIVEN** 一个任务执行 `npm install` 写入十万个文件
- **WHEN** 工具调用结束触发 capture
- **THEN** 账本新增**一条** `fs.capture`，manifest 批量更新；事件数不随文件数增长

#### Scenario: 后台作业逃出 run 后仍结算
- **GIVEN** 一次 bash 调用超时转入后台作业，原 run 已结束并释放租约
- **WHEN** 作业写完文件后退出
- **THEN** 账本新增一条 `fs.capture{sessionId, taskId, jobId, baseSeq, count, treeHash}`，
  manifest 含该文件，不产生 `fs.capture_failed`，也不出现伪造的 owner token

### Requirement: capture 策略按 run 固定

策略 MUST 由租户文档 `filesystem.capture.mode` 提供，并 MAY 由 `config.toml` 同名
setting 最后覆盖；MUST 在 attempt 开始时固定，进行中的 attempt MUST NOT 中途切换。
`checkpoint` / `manual` 没有对应触发器时 MUST 以类型化错误失败，MUST NOT 静默吞掉
capture。后台作业的结算 MUST NOT 受 run 的 capture 模式影响。run summary MUST 记录本次
attempt 的 capture 模式与来源。`checkpoint`/`manual` 的延迟结算窗口 MUST 在运维文档写明；
默认 `per-tool-call` 不受影响。

#### Scenario: 配置关闭实时 capture
- **GIVEN** 子代理的租户文档设置 `filesystem.capture.mode=checkpoint`
- **WHEN** 它连续执行多条命令
- **THEN** 只有该 attempt 完成（run close）或显式 checkpoint 时才结算变更

#### Scenario: manual 只由显式 checkpoint 结算
- **GIVEN** 租户文档设置 `filesystem.capture.mode=manual`
- **WHEN** run 连续执行多条命令且没有调用显式 checkpoint
- **THEN** 不产生任何自动 `fs.capture`；`bash {action:"checkpoint"}` 结算一次，重复调用不追加第二条

#### Scenario: 配置轮转只影响下一次 attempt
- **GIVEN** 一次 attempt 已固定 `checkpoint`
- **WHEN** 租户文档在该 attempt 进行中改为 `manual`
- **THEN** 该 attempt 仍按 checkpoint 结算，只有下一次 attempt 使用 manual

#### Scenario: config.toml setting 最后覆盖
- **GIVEN** 存储租户文档设 `checkpoint`，`config.toml` 设 `manual`
- **WHEN** 下一次 attempt 开始
- **THEN** 该 attempt 使用 `manual`，summary 的来源为 `config.toml@revision=<n>`
- **AND** 删除文件 setting 后，管理端 JSON 的 `checkpoint` 重新生效

#### Scenario: 模式没有触发器
- **GIVEN** 租户文档选择 `checkpoint` 但运行时没有 run 完成钩子
- **WHEN** 下一次 attempt 开始
- **THEN** attempt 以 `capture_trigger_missing` 失败，不产生被静默吞掉的 capture

### Requirement: 执行空间是物化视图，显式写必须写穿

执行空间（bash 真正运行的地方）MUST 是可丢弃的物化视图。任务/会话开始时 Harness MUST 按
manifest 物化所需文件；平台 MUST NOT 承诺全量镜像。**显式 `write`/`edit` MUST 写穿到已物化的
执行空间**，使随后的 `bash` 能立即读到。执行面适配器 MUST 实现
`materialize(manifest)` 与 `capture(handle, since)`；远端适配器可以只返回增量（路径 + 哈希），
Harness MUST NOT 假设能扫盘。

#### Scenario: 写后立即可读
- **WHEN** 执行空间已物化，模型先 `write a.txt` 再 `bash cat a.txt`
- **THEN** 返回新内容；capture 不会把这次写重复算成新对象

#### Scenario: 执行空间丢失
- **WHEN** 本地执行空间被清空而任务仍在进行
- **THEN** Harness 能按 manifest 重新物化，任务从账本继续，不依赖磁盘残留

#### Scenario: 远端执行面只给增量
- **WHEN** 远端适配器返回变更路径与哈希
- **THEN** Harness 按增量更新 manifest，不要求远端提供全量目录扫描

### Requirement: 配额与错误分类

layerfs MUST 按 group/租户执行存储字节配额；超限 MUST 返回可区分错误
（`quota_exceeded` / `not_found` / `io` / `unavailable`）。流式 Put MUST 先写临时对象，在提交点
校验配额；超限 MUST NOT 产生可见的部分对象。

#### Scenario: 超配额可解释
- **WHEN** 写入会使租户超过存储配额
- **THEN** 返回 `quota_exceeded` 与当前用量，提交点拒绝，仓库中不出现半成品对象
