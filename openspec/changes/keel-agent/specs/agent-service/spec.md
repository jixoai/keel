# agent-service

> `keel serve` 的产品服务面：AgentAPI + 静态 UI + 认证中间件。不再单独设 BFF。

## ADDED Requirements

### Requirement: 单一服务、同源

`keel serve` SHALL 在同一 origin 提供 AgentAPI（`/v1/*`）、静态 UI（`web/` 构建产物）与
SSE；浏览器 MUST NOT 直接持有 keel 的 `X-Tenant-ID`/admin key。认证 MUST 在服务中间件层
（首版本地口令，环境变量注入；后续接网络中间件），内核仍只认 `X-Tenant-ID`。

#### Scenario: 同源访问
- **WHEN** 浏览器打开 `keel serve` 根路径
- **THEN** 返回 NSH web 构建；所有 API 同源，无 CORS

### Requirement: SSE 原样透传

`GET /v1/sessions/{id}/events/stream` MUST 原样透传 keel 的帧（`id`/`event`/`data`、心跳、
排水帧），支持 `Last-Event-ID`/`after`；MUST NOT 引入 WebSocket 作为第二传输。

#### Scenario: 断线续传
- **WHEN** 浏览器带 `Last-Event-ID` 重连
- **THEN** 帧序列与直连 keel 一致，无重复无跳号

### Requirement: 错误映射

服务 MUST 透出 keel 语义：401（未认证）、409（状态冲突/外部会话）、429（配额/准入）、
503（后端不可用/排水）；MUST NOT 把 4xx 吞成 200。

#### Scenario: 排水期间
- **WHEN** keel 处于 drain
- **THEN** 新写请求得到 503 与可读原因；SSE 收到排水帧

### Requirement: 静态资源与缓存

静态 UI MUST 带内容哈希与长缓存；`index.html` MUST `no-cache`；SPA 路由回退到 `index.html`；
日志 MUST 脱敏（无密钥/消息正文）。

#### Scenario: 刷新深链
- **WHEN** 直接访问 `/sessions/<id>` 并刷新
- **THEN** 返回 `index.html`，客户端按路由恢复会话
