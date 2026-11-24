# Tasks: keel-dev-pg

## 1. 脚本

- [x] 1.1 `scripts/dev-pg.sh`：`start|stop|status|dsn|env|reset --yes`；数据目录 `.dev/pg`、
      端口 13741、仅回环、随机密码 0600、PG 主版本 ≥18、端口占用拒绝、不碰用户既有实例。
- [x] 1.2 `.gitignore` 增加 `/.dev/`。
- [x] 1.3 `cmd/keel/{serve,admin}.go` 的 DSN 缺失报错加 `scripts/dev-pg.sh start` 指路
      （错误串未被测试断言，改后跑 cmd/keel 测试）。

## 2. 文档

- [x] 2.1 README quickstart：本机脚本优先，Docker 一行备选。
- [x] 2.2 官网首页 EN/ZH：同口径；测试页给 `eval "$(scripts/dev-pg.sh env)"` 的测试命令。
- [x] 2.3 `docs/RUNBOOK.md`：新增「无 Docker 本地集群」小节（start/env/stop/reset 与安全边界）。

## 3. 验证与收口

- [x] 3.1 脚本实测：start → status/env → `keel serve` 健康 200 → stop；重复 start 幂等；
      reset --yes 后可重建；端口被占时拒绝。
- [x] 3.2 site build 四道门；README/site 文案生效。
- [x] 3.3 归档（skip_specs）。

## 证据

- `evidence/dev-pg-transcript.txt`：start（幂等）→ status/env → `keel admin bootstrap` →
  `keel serve` `/v1/health` 200 → stop → 重复 start → 端口被占拒绝（exit 1）→
  `reset --yes` → 重建 → status → `reset --yes`；DSN 密码与 setup token 已脱敏。
- `pnpm --dir site build`：37 对页面、3314 链接、1471 引用（含 EN 页零中文门）全绿。
