# Proposal: keel-db-cli

> 立项：2026-11-24（Owner 反问：数据库为什么要依赖 sh 脚本，为什么不用 `keel db` 统一管理）。

## Why

`keel-dev-pg` 交付的 `scripts/dev-pg.sh` 只是补洞的权宜形态，作为最终产品面不成立：

- **只在仓库检出里可用**：通过发布包/安装的 `keel` 二进制拿不到这个脚本，quickstart 却把它当第一步；
- **只支持 POSIX shell**：Windows 用户无法使用；`eval "$(...)"` 也是 shell 方言；
- **与 CLI 割裂**：`keel` 已有 `serve/admin/doctor/...` 的帮助与错误体系，数据库管理却藏在 `scripts/` 里，
  用户不会去那里找，`keel --help` 也不提；
- **重复实现**：脚本里的版本门、端口检查、密码生成、DSN 拼装本应是可测试的 Go 代码，而不是不可单测的 shell。

## What Changes

1. **新增 `keel db` 子命令**，统一管理本机开发用 PostgreSQL 18 集群（仍不依赖 Docker）：
   - `keel db start`：未初始化则 `initdb`（superuser 默认 `keel`，随机密码写入 0600 文件），
     启动、等待就绪、创建数据库，输出 DSN；已运行则幂等输出 DSN；
   - `keel db stop` / `status` / `dsn` / `env`（输出 `export KEEL_DSN='…'` 供 `eval`）/
     `reset --yes`（停止并删除本工具创建的数据目录）；
   - 默认数据目录 `$KEEL_HOME/dev-pg`（未设置时 `~/.keel/dev-pg`），`--dir` 覆盖；
     端口/库名/用户名默认 `13741`/`keel`/`keel`，初始化时写入元数据文件，后续命令读取；
   - 安全边界：仅监听 `127.0.0.1`；密码随机、0600；`initdb` 主版本必须 ≥18；
     端口被占拒绝；`reset` 只删除带 `PG_VERSION` 的自有目录，不碰用户其它目录。
2. **删除 `scripts/dev-pg.sh`**（Alpha 零兼容，不保留 wrapper），移除 `/.dev/` gitignore。
3. **文档与报错改口径**：README、RUNBOOK、官网首页/测试页（EN+ZH）改为
   `go build -o keel ./cmd/keel` → `./keel db start` → `eval "$(./keel db env)"`；
   `keel serve`/`keel admin` 缺 DSN 时提示 `keel db start`。
4. **验证**：Go 单测覆盖版本解析/DSN/元数据/reset 保护；本机全流程实测
   （冷启动、幂等、status/env、bootstrap+serve health 200、端口占用、reset 重建）；
   站点四道门 + 部署；验收报告改为 `keel db` 并重拍 390 特写。

## Impact

- 代码：`cmd/keel/db.go`（新）、`cmd/keel/main.go`、`cmd/keel/{serve,admin}.go`、单测；不改运行时。
- 文档：README、RUNBOOK、官网 6 个页面；删除一个脚本。
- 取代已归档的 `keel-dev-pg` 的脚本形态；其证据保留为历史，不再作为交付入口。
