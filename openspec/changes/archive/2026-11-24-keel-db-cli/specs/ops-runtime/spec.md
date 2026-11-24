## ADDED Requirements

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
