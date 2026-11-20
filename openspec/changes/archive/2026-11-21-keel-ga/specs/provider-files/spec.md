# provider-files

## ADDED Requirements

### Requirement: 持久层只存 blobId

对话消息中的图片 SHALL 以 `blobId`（可选 `mime`、`width`、`height`）持久化；
MUST NOT 把 base64 写进事件表。

#### Scenario: 同图多轮引用
- **WHEN** 同一图片被多个会话/多轮引用
- **THEN** 事件表只出现 blobId，存储中只有一份内容

### Requirement: 出站形态由能力决定

provider adapter SHALL 声明 `SupportsImageURL` / `SupportsFileID` / `MaxImageBytes`；
发送图片时按 `file_id` > `url` > `base64` 选择，MUST 在超过 `MaxImageBytes` 时降级或拒绝。

#### Scenario: 仅支持 base64 的 provider
- **WHEN** 目标 provider 不支持 URL/file_id
- **THEN** Keel 流式读取 blob 并编码，内存不随图片大小增长

### Requirement: provider file 缓存

上传到 provider 的文件 SHALL 以 `(sha256, provider, workspace)` 缓存 `file_id/uri`；
带 TTL 的 handle 过期后 MUST 懒刷新；引用归零后可回收。

#### Scenario: 48h 过期
- **WHEN** Gemini handle 超过 48h 后再次使用
- **THEN** 重新上传并更新缓存，不影响会话
