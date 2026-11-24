# Tasks: keel-db-cli

## 1. CLI

- [x] 1.1 `cmd/keel/db.go`：`keel db start|stop|status|dsn|env|reset --yes`，`--dir/--port/--database/--user`；
      默认目录 `$KEEL_HOME/dev-pg` → `~/.keel/dev-pg`；元数据（port/user/db）落盘，后续命令读取。
- [x] 1.2 安全与健壮性：PG≥18 版本门；仅 127.0.0.1；随机密码 0600；端口占用拒绝；
      已初始化时参数不一致报错；`reset` 只删带 `PG_VERSION` 的自有目录。
- [x] 1.3 `cmd/keel/main.go` 注册 `db` 与 usage；`serve/admin` 的 DSN 缺失提示改为 `keel db start`。
- [x] 1.4 `cmd/keel/db_test.go`：版本解析、DSN 拼装、默认目录、元数据往返、reset 拒绝外来目录。

## 2. 移除脚本与改文档

- [x] 2.1 删除 `scripts/dev-pg.sh`，移除 `.gitignore` 的 `/.dev/`。
- [x] 2.2 README quickstart：build keel → `./keel db start` → `eval "$(./keel db env)"`。
- [x] 2.3 RUNBOOK「本地起 PostgreSQL」：`keel db` 命令与 `--dir`、reset 安全边界。
- [x] 2.4 官网首页/测试页/核心页 EN+ZH：`./keel db start` / `./keel db env`，390 代码块完整可读。
- [x] 2.5 全仓 `grep dev-pg.sh` 清零（归档 change 的历史证据除外）。

## 3. 验证与收口

- [x] 3.1 `go test ./cmd/keel`、`go vet ./cmd/keel`、`go build ./cmd/keel`。
- [x] 3.2 集群全流程实测：start（幂等）→ status/env → bootstrap + serve `/v1/health` 200 →
      stop → 端口占用拒绝 → reset → 重建；证据转录（密码/token 脱敏）。
- [x] 3.3 `pnpm --dir site build` 四道门；部署后线上 EN/ZH 实测。
- [x] 3.4 验收报告改为 `keel db`（6 步/8 图），390 特写重拍；视觉复核 PASS。
- [x] 3.5 归档（spec delta 进 `ops-runtime`）、MAINLINE 更新、父仓指针。

## 证据

- `evidence/keel-db-transcript.txt`：`keel db start`（冷启动/幂等）→ `status`/`dsn`/`env` →
  bootstrap + serve `/v1/health` 200 → stop → 端口占用 exit 1 → reset → 重建 → status → reset；
  DSN 密码与 setup token 已脱敏。
- `cmd/keel` 单测：版本解析、DSN、元数据 0600、默认目录、reset 拒绝外来目录/缺 `--yes`、help。
- `pnpm --dir site build`：37 对页面、3314 链接、1471 引用（含 EN 零中文）全绿。
