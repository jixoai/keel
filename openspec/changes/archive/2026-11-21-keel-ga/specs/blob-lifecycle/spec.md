# blob-lifecycle

## ADDED Requirements

### Requirement: 引用计数由事件维护

blob 的 `ref_count` SHALL 由引用它的事件（user/assistant/tool.result 中的 blobId）
维护；同一 `(blob, session, eventSeq)` 只计一次。

#### Scenario: 重复引用
- **WHEN** 同一事件被重放处理两次
- **THEN** `ref_count` 只增加一次

### Requirement: TTL 回收

无引用且过期的 blob SHALL 被周期性回收：先删 metadata，再删 chunks/CAS 文件；
回收 MUST 幂等，MUST NOT 删除仍被引用或上传未完成的对象。

#### Scenario: 孤儿清理
- **WHEN** blob `ref_count=0` 且 `expires_at < now()`
- **THEN** 其 chunks 与 CAS 文件被删除，重复回收不报错

### Requirement: 缩略图

系统 SHALL 为图片异步生成最长边 320px 的缩略图；原图保持不变；生成失败
MUST NOT 阻断上传与对话。

#### Scenario: 大图
- **WHEN** 上传 4000px 原图
- **THEN** 原图可下载，缩略图在后台生成并可单独获取

### Requirement: 续传边界正确

base64 续传 SHALL 在任意 4 的倍数 offset 上与全量编码的后缀逐字节一致；
fuzz 测试 MUST 覆盖 0、3 的倍数边界、chunk 边界与文件末尾。

#### Scenario: 随机 offset
- **WHEN** 随机选择 1000 个 4 的倍数 offset
- **THEN** 续传结果与全量 base64 的后缀完全一致
