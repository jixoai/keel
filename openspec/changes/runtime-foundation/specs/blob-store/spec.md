# blob-store

## ADDED Requirements

### Requirement: 先传文件，后引用

系统 SHALL 提供 `POST /v1/files` 接收二进制或 multipart 上传，返回 `file_id` 与稳定 URL。
对话消息 MUST 只引用 `file_id`/URL，MUST NOT 内嵌整图字节重复上传。

#### Scenario: 上传一次多处引用
- **WHEN** 同一 `file_id` 被多个会话消息引用
- **THEN** 存储中只有一份数据，`ref_count` 相应增加

### Requirement: 分块存储

大于 256KB 的图片/blob SHALL 以 64KB 分块存 PostgreSQL（`(tenant_id, blob_id, seq)` 主键）；
大于 1MB 的原图 SHOULD 落本地内容寻址存储（CAS，sha256 路径）。单行整图
（一次 materialize 整个 `bytea`）MUST NOT 使用。

#### Scenario: 大图读取
- **WHEN** 读取 10MB 图片
- **THEN** 按 64KB 分块流式读取，进程内存增量 ≤384KB（6 chunk 窗口）

### Requirement: 流式编码

对 base64 输出，系统 MUST 使用流式编码器（`base64.NewEncoder` + `io.Copy`），
MUST NOT 对整图调用 `EncodeToString`。

#### Scenario: base64 流
- **WHEN** 客户端请求 `GET /v1/files/{id}?format=base64`
- **THEN** 响应分块到达，首字节延迟 <100ms，峰值内存与图片大小无关

### Requirement: 生命周期与回收

系统 SHALL 维护 `ref_count`、`expires_at`，并周期性回收无引用且过期的 blob；
回收 MUST 幂等，MUST NOT 删除仍被引用或在途上传的对象。

#### Scenario: 孤儿回收
- **WHEN** blob 的 `ref_count=0` 且超过 TTL
- **THEN** 其分块与元数据被删除，重复回收不报错

### Requirement: 校验与去重

写入 SHALL 计算 sha256 并校验长度；相同内容 SHOULD 在租户内去重。

#### Scenario: 重复上传
- **WHEN** 同租户重复上传同一字节内容
- **THEN** 返回相同 `file_id`（或明确记录两个 id 指向同一内容）
