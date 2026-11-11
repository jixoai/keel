# Task Log（任务卡与可溯源记忆图）

本 spec 定义任务卡的数据模型、强制归属、任务树投影、资源关联与 fork 语义。
任务卡是 DB 行 + 追加事件，不是文件夹；文件字节见 `layerfs`。

## ADDED Requirements

### Requirement: 任务卡是数据库行加追加事件

任务卡 MUST 是 DB 行（`task_cards`），其一切变化 MUST 通过追加事件
（`task.created` / `task.updated` / `task.attached` / `task.closed` / `task.note`）完成，
MUST NOT 就地覆盖历史。任务卡的当前状态 MUST 是事件投影。

#### Scenario: 改标题留痕
- **WHEN** 任务标题从 A 改为 B
- **THEN** 追加一条 `task.updated` 事件；面板显示 B，历史仍可查回 A 与修改时间

#### Scenario: 关闭后可回看
- **WHEN** 任务被关闭
- **THEN** 其全部事件、资源引用与关联工具调用仍可读取，不随关闭删除

### Requirement: 任务事件是账本投影

`task_events` MUST 是从所属会话账本重建的**物化投影**，MUST NOT 直接写入；重建 MUST 幂等，
且与账本中的 `task.*` 事件逐条一致。任务树与面板 MUST 只读该投影。

#### Scenario: 重建一致
- **WHEN** 从账本重建 `task_events`
- **THEN** 行数与 `task.*` 事件数一致，payload 逐条相等；重复重建不产生重复行

#### Scenario: 直接写入被拒
- **WHEN** 任何代码绕过账本直接写 `task_events`
- **THEN** 被拒绝（或在一致性测试中被判失败）

### Requirement: 每次工具调用必须带 taskId

Harness MUST 要求每一次工具调用携带 `taskId`；缺失 MUST 拒绝执行并返回类型化错误。
会话启动时 MUST 自动创建初始任务卡。内部子代理 MUST 拥有自己的任务卡，并以
`parentTaskId` 指向派生它的卡。

#### Scenario: 无卡调用被拒
- **WHEN** 一次工具调用没有 `taskId` 或指向不存在的卡
- **THEN** 调用被拒绝，事件流中记录一次被拒的调用事实，模型看到可解释的错误

#### Scenario: 会话起步即有卡
- **WHEN** 新建一个会话并提交第一条输入
- **THEN** 已存在一张初始任务卡，第一条工具调用即挂在它下面

### Requirement: 任务树是投影，跨会话可拼接

任务树 MUST 由 `parentTaskId` 关系投影得到；跨会话的子任务（子代理）MUST 通过
`parentTaskId` 拼接进同一棵树。任务事件 MUST 写入所属会话的账本并携带 `taskId`，
MUST NOT 引入第二套全局账本。

#### Scenario: 子代理挂上父卡
- **WHEN** 一个子代理在独立会话中创建自己的卡
- **THEN** 面板把它显示为父卡的孩子，父卡的会话与子卡的会话各自的 seq 仍独立连续

#### Scenario: 迁移后树不变
- **WHEN** 一个会话被迁移到另一个 Group
- **THEN** 任务树的父子关系不变（关系在 DB，不在进程）

### Requirement: fork 以 eventSeq 为原点

fork MUST 只接受一个 `eventSeq` 作为原点，创建新会话并继承该点之前的账本与文件视图；
MUST NOT 要求额外的文件系统版本号。文件视图 MUST 由 `session_snapshot(session_id, eventSeq)` 的
tree hash 解析（O(1)），MUST NOT 复制 manifest 行或字节。fork MUST 创建新的任务卡并以
`parentTaskId` 指向原点所在卡。

#### Scenario: 并行子代理从同一点岔出
- **WHEN** 从 `eventSeq=100` 连续 fork 出三个子代理
- **THEN** 三者看到相同的文件视图，各自的后续写入互不可见，字节与 manifest 行都不重复存储

#### Scenario: 技能版本 A/B 对比
- **GIVEN** 两个技能版本在同一 `eventSeq` 处 fork
- **WHEN** 两个分支执行同一任务
- **THEN** 两条分支的账本可逐事件对比，文件差异可归因到各自分支

### Requirement: 资源关联只记引用，可解绑

任务关联的文件 MUST 以 `(path, hash)` 形式记录（`task.attached`），MUST NOT 复制字节。
解绑 MUST 通过 `task.detached` 事件表达；引用归零后由 GC 回收。MUST NOT 对任务关联的资源承诺
版本语义之外的东西（例如「资源永不变」）；变化通过新事件表达。

#### Scenario: 挂载构建产物
- **WHEN** 一个任务执行完 `pnpm build` 并把 `dist/` 挂到卡上
- **THEN** 卡上出现 `(path, hash)` 引用集合，layerfs 按内容去重，不复制已有字节

#### Scenario: 替换与解绑
- **WHEN** 同一路径被重新挂载为新内容，或资源被显式解绑
- **THEN** 追加新的 `task.attached` / `task.detached` 事件；旧 hash 仍可回看，直到引用归零被 GC

### Requirement: bash 只追加执行事实

bash 调用 MUST 只向任务日志追加：命令、开始/结束时间、耗时、退出码、输出引用
（blob hash + head/tail）。MUST NOT 让 bash 承担任务语义（例如判断「这一步是否完成」）。

#### Scenario: 长命令输出
- **WHEN** 一条命令产生 200MB 输出
- **THEN** 任务日志记录 blob 引用与 head/tail，不内联全文

#### Scenario: 超时转后台
- **WHEN** 一条命令超时被转入后台
- **THEN** 任务日志记录超时事件、后台 pid 与后续可查询的作业引用
