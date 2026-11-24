## ADDED Requirements

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
