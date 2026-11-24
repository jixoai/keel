# ops-runtime Specification

## Purpose

Keel 单机运行时的部署与运维契约：2GB 内存档、1000 会话/并发 run 的资源边界、drain/handoff、SIGKILL 恢复与运维可观测口径。

## Requirements

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

### Requirement: 本地开发数据库 CLI

`keel db` SHALL 统一管理本机开发用 PostgreSQL 18 集群，不依赖 Docker 或 shell 脚本。
子命令 MUST 包含 `start|stop|status|dsn|env|reset --yes`；默认数据目录
`$KEEL_HOME/dev-pg`（未设置时 `~/.keel/dev-pg`），`--dir` 可覆盖。

集群 MUST：

- 只监听 `127.0.0.1`；
- 使用随机密码，密码文件权限 0600；
- 拒绝 PostgreSQL 主版本 <18；
- 在端口被其它进程占用时拒绝启动；
- `reset` 只删除带 `PG_VERSION` 的自有数据目录，MUST NOT 删除其它目录；
- 已初始化后端口/库名/用户名不一致时 MUST 报错，MUST NOT 静默改用新值。

#### Scenario: 冷启动

- **WHEN** 本机还没有集群且执行 `keel db start`
- **THEN** 初始化并启动集群、创建数据库，输出可直接用于 `KEEL_DSN` 的 DSN

#### Scenario: 重复启动

- **WHEN** 集群已在运行且再次执行 `keel db start`
- **THEN** 不重复初始化，幂等输出同一 DSN

#### Scenario: 端口占用

- **WHEN** `127.0.0.1:<port>` 已被其它进程监听
- **THEN** `start` 以非零码退出并提示端口占用，不启动集群

#### Scenario: 重置保护

- **WHEN** `--dir` 指向一个非本工具创建的非空目录
- **THEN** `reset --yes` 拒绝删除并报错

### Requirement: 本地数据库自动初始化

`keel serve` 与 `keel admin` 在 `KEEL_DSN`/`--dsn` 未设置时 SHALL 自动确保本机开发用
PostgreSQL 18 集群可用（缺则初始化、停则启动、缺库则建库）并继续执行；提示只包含数据目录，
MUST NOT 把密码写入日志。

一旦显式设置了 `KEEL_DSN`/`--dsn`，这些命令 MUST 只使用显式 DSN，MUST NOT 自动初始化。
`--no-auto-db` 或 `KEEL_NO_AUTO_DB=1` 时，无 DSN MUST 以清晰错误退出，并提示
`keel db start` 或设置 `KEEL_DSN`。

#### Scenario: 首次 serve

- **WHEN** `KEEL_DSN` 未设置且本机没有开发集群，执行 `keel serve`
- **THEN** 自动初始化并启动本地集群，服务正常启动

#### Scenario: 显式 DSN

- **WHEN** `KEEL_DSN` 指向一个已有 PostgreSQL
- **THEN** 不创建、不启动本地开发集群，直接使用该 DSN

#### Scenario: 关闭自动

- **WHEN** 设置 `KEEL_NO_AUTO_DB=1` 且未设置 `KEEL_DSN`
- **THEN** `keel serve` 以非零码退出并提示设置 DSN 或运行 `keel db start`
