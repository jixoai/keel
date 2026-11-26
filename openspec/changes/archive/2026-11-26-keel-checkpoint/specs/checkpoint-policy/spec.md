# checkpoint-policy

## ADDED Requirements

### Requirement: Checkpoint 是投影，不是真值

系统 MUST 保持事件日志为唯一真值；checkpoint MUST 只记录压缩后的可恢复投影，MUST NOT
删除/改写历史事件。恢复 MUST 能从 checkpoint + tail 得到与事件回放一致的结果。

#### Scenario: 历史仍可读
- **WHEN** 会话已生成多个 checkpoint
- **THEN** 原始事件仍可按 seq 读取，checkpoint 不替代历史

### Requirement: 安全的压缩切点

自动/手动压缩 MUST 不在工具调用中间切、不压缩最后一条 user；摘要失败或取消 MUST 保留原
上下文，MUST NOT 落半成品 checkpoint。成功时 MUST 在同一幂等边界追加 compaction 事件与
checkpoint。

#### Scenario: 工具中间不切
- **WHEN** 上下文中最后一个工具调用尚未闭合
- **THEN** 切点前移到安全边界，或本次不压缩

#### Scenario: 摘要失败
- **WHEN** provider 摘要请求失败
- **THEN** 当前上下文不变，不写 checkpoint，记录失败并可重试

### Requirement: 恢复一致性

恢复 MUST 使用最近有效 checkpoint + 后续事件 tail；checkpoint 缺失/损坏 MUST fail closed
并回退到事件全量重建，MUST NOT 静默使用不完整上下文。重启/handoff/Worker 恢复 MUST 走同一路径。

#### Scenario: 重启恢复
- **WHEN** host 在 checkpoint 之后、tail 之前重启
- **THEN** 恢复后的上下文与重启前一致，事件 seq 连续

### Requirement: 策略与保留

系统 MUST 支持 tenant/session 级 checkpoint 策略：启用、阈值、保留 token、最大 checkpoint
数、TTL、手动压缩开关。session MUST 只能收窄 tenant 上限。清理 MUST 只删除 checkpoint
投影，不删除事件；最近有效 checkpoint MUST 保留到被新 checkpoint 替代。

#### Scenario: 收窄不能突破
- **WHEN** tenant 上限为 3 个 checkpoint，session 配置 10 个
- **THEN** 实际生效为 3，且 UI/API 说明被 tenant 收窄

#### Scenario: TTL 清理
- **WHEN** 旧 checkpoint 超过 TTL 且已有更新 checkpoint
- **THEN** 旧投影被清理，事件与历史仍可读

### Requirement: 手动压缩

系统 SHALL 提供 `POST /v1/sessions/{id}/compact`：需要 session 写权限、幂等 requestId、
返回 checkpoint id/tokensBefore/After/是否执行；自动与手动压缩 MUST 共用切点与失败语义。

#### Scenario: 重复请求
- **WHEN** 同一 requestId 重复调用手动压缩
- **THEN** 返回同一结果，不生成第二个 checkpoint

#### Scenario: 无权限
- **WHEN** 调用者没有该 session 写权限
- **THEN** 返回 403，不读取/不压缩上下文

### Requirement: 可观测与脱敏

checkpoint 创建/恢复/失败/清理 MUST 产生结构化日志与指标，包含 tenant/session/run id、
cutSeq、policyVersion、tokens 和耗时；MUST NOT 包含消息正文或摘要正文。

#### Scenario: 审计脱敏
- **WHEN** checkpoint 因摘要失败被拒绝
- **THEN** 日志只有原因/ids/tokens，不含对话内容
