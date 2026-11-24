# Tasks: keel-db-auto

## 1. 自动初始化

- [x] 1.1 `cmd/keel/db.go`：抽取 `devDB.ensure`（初始化/启动/建库/DSN）与 `ensureAutoDevDB`。
- [x] 1.2 `keel serve`：`KEEL_DSN`/`--dsn` 为空且未关闭自动时调用 `ensureAutoDevDB`，stderr 只提示目录；
      `--no-auto-db` / `KEEL_NO_AUTO_DB=1` 时 exit 2 并指路。
- [x] 1.3 `keel admin`：`adminStore` 同样自动；显式 DSN 优先，关闭时清晰报错。
- [x] 1.4 帮助与 usage：`keel serve`/`keel admin`/`keel db` 说明自动行为与 opt-out。
- [x] 1.5 单测：`ensureAutoDevDB` 的默认目录/元数据路径；无 DSN 关闭自动的错误（不启动集群）。

## 2. 文档

- [x] 2.1 README 快速开始：build → `./keel serve --auth local-only`（首次自动建库），
      说明 `keel db status|stop|reset` 与 `KEEL_NO_AUTO_DB=1`。
- [x] 2.2 官网首页/核心页 EN+ZH：删除 `keel db start`/`eval` 步骤，改为 bootstrap/serve 自动建库。
- [x] 2.3 RUNBOOK：自动初始化小节；测试页保留显式 `keel db`（测试需 `KEEL_TEST_DSN`）。
- [x] 2.4 全站 390 代码块复测（quickstart 全块在视口内）。

## 3. 验证与收口

- [x] 3.1 无 DSN：`keel admin bootstrap` 自动建库 → `keel serve` health 200；无残留进程/端口。
- [x] 3.2 `--no-auto-db` 与 `KEEL_NO_AUTO_DB=1` 两种关闭路径 exit 2；显式 DSN 不触发自动。
- [x] 3.3 `go test ./cmd/keel`、`go vet ./cmd/keel`、`go build ./...`；site 四道门。
- [x] 3.4 部署后线上 EN/ZH 核对；验收报告改为自动初始化并重拍；视觉复核 PASS。
- [x] 3.5 归档、MAINLINE、父仓指针。

## 证据

- `evidence/auto-init-transcript.txt`：无 `KEEL_DSN` 的 `keel admin bootstrap` 自动初始化 →
  `keel serve`（local-only）/v1/health 200；`--no-auto-db` 与 `KEEL_NO_AUTO_DB=1` 均 exit 2；
  显式坏 DSN 只尝试该 DSN（connection refused，exit 1）且不创建本地目录；最后 reset。
- `go test ./cmd/keel`：新增 admin/serve 关闭自动的用例；`go vet`/`go build ./...` 通过。
- `pnpm --dir site build`：37 对/3314 链接/1471 引用全绿。
