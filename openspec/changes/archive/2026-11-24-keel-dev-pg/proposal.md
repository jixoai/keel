# Proposal: keel-dev-pg

> 立项：2026-11-24（Owner 追问 Docker 依赖后确认：本机 PG 要零配置可用）。

## Why

keel 只依赖 PostgreSQL 18，但 quickstart 的第一步是 `docker run --name keel-pg`；没有 Docker
的开发者要自己 `initdb`、起端口、建库、拼 DSN，或者误以为必须先装 Docker。本机（Homebrew
PostgreSQL 18.7）其实完全够用。

## What Changes

1. **`scripts/dev-pg.sh`**：在仓库内管理一个**用户本地**的 PostgreSQL 18 集群，零 Docker：
   - `start`：数据目录默认 `.dev/pg`（gitignored），端口默认 `13741`，仅监听 `127.0.0.1`；
     `initdb` 一次（superuser `keel`，host 认证 `scram-sha-256`，随机密码存 `.dev/pg.pass` 0600），
     启动、等待就绪、建库 `keel`；
   - `stop` / `status` / `dsn` / `env`（可 `eval "$(scripts/dev-pg.sh env)"` 导出 `KEEL_DSN`）/
     `reset --yes`（停库并删数据目录）；
   - 版本门：`initdb` 主版本必须 ≥18；端口被非本集群占用时拒绝并给出占用者；
   - 不触碰用户已有的 PostgreSQL 实例/数据目录。
2. **`keel serve` / `keel admin` 的 DSN 缺失报错**加一句指路：`scripts/dev-pg.sh start`。
3. README 与官网首页 quickstart 改为**本机脚本优先**，Docker 作为备选一行；RUNBOOK 增
   「无 Docker 本地集群」小节；测试页注明 `eval "$(scripts/dev-pg.sh env)"` 后跑
   `KEEL_TEST_DSN=$KEEL_DSN go test ./...` 即可。

## Impact

- 新脚本 + `.gitignore` + README/site/RUNBOOK 文档 + 两处错误文案；不改运行时、测试或发布产物。
- 安全边界：集群只绑回环；密码随机、0600、不入库；数据目录在 `.dev/`（gitignore）。
