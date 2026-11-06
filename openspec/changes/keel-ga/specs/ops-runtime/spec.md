# ops-runtime

## ADDED Requirements

### Requirement: CI 绿门

每次推送 SHALL 运行：`go build ./...`、`go vet ./...`、`go test -race ./...`、
容器构建。集成测试用服务容器（PG 18）。

#### Scenario: PR 检查
- **WHEN** 提交 PR
- **THEN** 上述门全部通过才可合并

### Requirement: 健康与就绪

系统 SHALL 提供 `/v1/health`（进程存活、bootId）与 `/v1/ready`（PG 可达、迁移完成）。
就绪失败 MUST 返回 503。

#### Scenario: 数据库不可达
- **WHEN** PG 连接失败
- **THEN** `/v1/ready` 返回 503，负载均衡器摘除实例

### Requirement: 指标

系统 SHALL 提供 Prometheus 文本指标，固定基数：流/队列/租约/PG/运行时；
MUST NOT 以 streamId/sessionId 作为标签。

#### Scenario: 抓取
- **WHEN** Prometheus 抓取 `/metrics`
- **THEN** 返回当前计数与直方图，series 数不随会话数增长

### Requirement: 优雅排水

关闭时服务 SHALL 先向 SSE 客户端发送 `event: drain`（含 `retry_after_ms`），
按速率关闭连接，再停止接受新连接；MUST NOT 一刀切断开。

#### Scenario: 发布
- **WHEN** 实例收到 SIGTERM
- **THEN** 客户端在数秒内收到 drain 并重连到其他实例，不丢事件
