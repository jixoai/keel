## ADDED Requirements

### Requirement: 外部运行投影读取

外部会话（`state_owner=external`）的 OAR 记录 MUST 能以一份只读投影交给客户端渲染，
投影文档 MUST 是 `keel.oar.render-projection/1`（与 `internal/oar/projection.go` 同一模型），
客户端 MUST NOT 自行折叠第二套记录模型。

`GET /v1/sessions/{id}/external/projection` MUST：

- 只读、无请求体；会话可见性沿用既有租户/归属校验；
- 从该会话的 backend 会话句柄定位 OAR journal（`session-<ref>.ndjson`），
  返回 `oar.ProjectionJSON`；
- 非外部会话 MUST 404（typed `not_external_session`）；
- 部署未配置 journal 目录 MUST 501（typed `oar_not_wired`），MUST NOT 用空投影冒充可用；
- journal 存在但为空 MUST 返回同 schema 的空投影（`items`/`agents` 为空数组）；
- journal 损坏 MUST 5xx（typed），MUST NOT 静默吞；
- MUST NOT 写任何事件、任务卡或账本行。

#### Scenario: 有记录的外部会话
- **WHEN** 一个外部会话的 OAR journal 已有 call/result/status 记录
- **THEN** 投影接口返回 200 与同 schema 文档；`items` 按 seq 折叠，`agents` 带 rollup，
  `simulated` 与 runtime 原样保留

#### Scenario: 非外部会话
- **WHEN** 调用者对普通 Bot 会话请求投影
- **THEN** 返回 404 `not_external_session`，不返回空投影

#### Scenario: 未接 OAR 的部署
- **WHEN** 部署没有配置 `-oar-journal-dir` / `KEEL_OAR_JOURNAL_DIR`
- **THEN** 返回 501 `oar_not_wired`

#### Scenario: 跨租户
- **WHEN** 调用者用另一个租户的外部会话 id 请求投影
- **THEN** 返回 404，响应体不含该会话的记录
