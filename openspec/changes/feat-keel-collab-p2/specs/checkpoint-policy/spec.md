# checkpoint-policy Delta

## ADDED Requirements

### Requirement: Checkpoint 摘要哈希校验

每个 checkpoint MUST 记录摘要的 sha256（`summary_hash`）；对应的 compaction 事件
payload SHOULD 记录同一 `summaryHash`。恢复 MUST 校验行内哈希与摘要、以及行哈希与
事件哈希的一致性；不一致 MUST fail closed，回退事件全量重建并记录 `corrupt` 原因，
MUST NOT 使用被篡改或不一致的摘要。旧行/旧事件缺少哈希时 MUST NOT 伪造该事实。

#### Scenario: 行内摘要被篡改
- **WHEN** checkpoint 行的 summary 与 summary_hash 不一致
- **THEN** 恢复回退全量事件，不使用该 checkpoint

#### Scenario: 行与事件不一致
- **WHEN** checkpoint 行哈希与 compaction 事件 payload 的 summaryHash 不一致
- **THEN** 恢复回退全量事件，不使用该 checkpoint

### Requirement: Manual requestId 幂等独立于 prune

manual compaction 的 requestId MUST 由独立于 checkpoint 投影的持久幂等事实承载。
Prune/TTL 只删除 checkpoint 投影，MUST NOT 删除 requestId 幂等记录。同一 requestId
MUST 返回原 checkpoint id/tokens/keptFromSeq/policyVersion，且 MUST NOT 再次调用模型
或追加第二个 compaction 事件；同一 requestId 指向不同 upto_seq MUST 被拒绝。

#### Scenario: 投影被 prune 后重放
- **WHEN** manual requestId 产生的 checkpoint 已被 max/TTL prune
- **THEN** 相同 requestId 仍返回原结果，不重新压缩、不新增事件

### Requirement: TTL 惰性清理

checkpoint TTL MUST 在会话的读取/恢复路径上惰性生效，而不只依赖写入新 checkpoint。
清理 MUST 只删除过期且非最新的投影；最近一个有效 checkpoint MUST 保留；事件日志与
requestId 幂等记录 MUST NOT 被清理。清理失败 MUST 不影响 run，并记录可观测原因。

#### Scenario: 长期无新压缩
- **WHEN** 会话配置 TTL 后长时间不再产生新 checkpoint，随后再次运行
- **THEN** 运行开始时清理过期的旧投影，恢复仍使用最新 checkpoint + tail

#### Scenario: 最新 checkpoint 保护
- **WHEN** 所有投影都超过 TTL
- **THEN** 最新有效投影仍保留，作为恢复锚点
