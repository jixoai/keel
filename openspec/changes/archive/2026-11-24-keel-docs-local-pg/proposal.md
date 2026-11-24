# Proposal: keel-docs-local-pg

> 立项：2026-11-24（Owner 追问「keel-pg？现在还需要依赖 Docker？」）。

## Why

`keel` 的运行依赖只有 **PostgreSQL 18**：单个 Go 二进制 + DSN 即可。但 README 与官网首页的
30 秒 quickstart 把 `docker run --name keel-pg` 写成第一步，且测试页只提 CI 的
`postgres:18` service 容器；读者因此合理地以为 Docker 是必需依赖。

事实是：

- 本机全部验证（76 个 Go 包、web/TUI、OAR E2E、活实例评审、R1 差分）都跑在本机
  PostgreSQL 18.7 上，零 Docker；
- Docker 只出现在三处可选位置：quickstart 的一次性本地 PG；少数验收脚本默认
  `KEEL_PG_CTR=keel-pg`（均支持 `KEEL_DSN` 覆盖）；`acceptance-perf.sh` 的 2GB cgroup
  电池（这一条确实要 Docker/Linux，故 E0 标平台受限）；
- 可选自托管 `docker-compose.yml` / `Dockerfile` 与二进制运行无关。

## What Changes

- README quickstart：加「不用 Docker」的等价路径（任意本机 PG 18 + `KEEL_DSN`），并注明
  `keel-pg` 仅为一次性本地库的便利；少数验收脚本的 Docker 默认值与覆盖方式。
- 官网首页（EN/ZH）quickstart：同上一段旁注；测试页（EN/ZH）写明「测试只需 PG，
  `KEEL_TEST_DSN` 指向本机即可；CI 的 postgres service 与 2GB cgroup 电池才涉及容器」。
- 不改任何代码/脚本默认值；只在文档里把「可选」与「硬需求」分开。

## Impact

- `README.md`、`site/docs/{index,core/extend/testing}.md` + `zh/`；站点四道门与部署。
- 不改变运行时、测试或发布产物。
