# Deliverables（交付物 / 工作区变更投影）

本 spec 定义 `keel.deliverables`：会话文件清单（`file_manifest`）的只读投影。交付物 MUST 是
清单行（path、hash、size、mode、updatedSeq、lastTaskId）加上服务端给出的预览链接；MUST NOT
有第二个文件真值、MUST NOT 在客户端重算哈希。实现真值：`internal/fileplugin`（清单）、
`internal/harnessapi/session_query.go`（`GET /v1/sessions/{id}/deliverables`）、
`web/src/data/deliverables.ts` 与 `web/src/screens/session-status-pane.tsx`（界面）。

## ADDED Requirements

### Requirement: 交付物是 file_manifest 的投影

`GET /v1/sessions/{id}/deliverables` MUST 读取该会话自己的 `file_manifest`，MUST 为每一行返回
`{path, hash, size, mode, updatedSeq, lastTaskId?, previewUrl}`。`hash` MUST 是清单里的内容
哈希；`previewUrl` MUST 指向同一会话的 `GET /v1/sessions/{id}/file-preview?path=&hash=`，
MUST NOT 是客户端拼接的路径。接口 MUST 按 path 升序、keyset 分页（`limit` 默认 100、最大
500，`after` 是上一页最后一行的 path 游标）；会话 MUST 先按租户校验存在性（404），
部署没有清单 seam 时 MUST 503。

#### Scenario: 一页清单
- **WHEN** 会话清单有 `docs/a.md`（10 字节）与 `src/b.ts`（20 字节）
- **THEN** 返回两行，按 path 排序；第一行的大小、哈希、mode、updatedSeq、lastTaskId 都等于
  清单行，`previewUrl` 带该行的 path 与 hash

#### Scenario: 跨租户
- **WHEN** 调用者请求另一个租户的会话
- **THEN** 回答 404，MUST NOT 返回该会话的路径或哈希

#### Scenario: 分页
- **WHEN** 清单有 5 行且 `limit=2`
- **THEN** 第一页 2 行并带 `nextCursor`；跟随游标的第二页从第 3 行开始，无重复

### Requirement: 交付物列表与预览链接

右侧状态栏 MUST 提供「交付物 · 工作区变更」列表：每行 MUST 显示 path、大小与哈希（哈希可
在显示层缩短，完整值仍在行数据里）；点击一行 MUST 通过清单的预览读打开该 path 的**有界**
内容，并 MUST 说明内容是否仍与行哈希一致。列表 MUST 覆盖加载/空/错误/权限不足/成功五态；
MUST 提供刷新与「继续加载」；MUST NOT 用空列表冒充读取失败。读取失败 MUST 显示服务端原因，
权限不足 MUST 单独成态。

#### Scenario: 列表与预览
- **WHEN** 一个会话的清单有 `src/composer.tsx`
- **THEN** 列表显示该 path、大小与哈希；点击后显示有界内容与「与引用一致」

#### Scenario: 空工作区
- **WHEN** 会话清单没有行
- **THEN** 显示「这个会话还没有交付物」，不显示伪造行

#### Scenario: 权限不足
- **WHEN** 交付物读取返回 403
- **THEN** 显示「权限不足」态并有服务端原因，不显示普通错误态或空态
