# Tasks: keel-docs-local-pg

## 1. README

- [x] 1.1 quickstart 加「不用 Docker」等价路径：任意本机 PostgreSQL 18 + `KEEL_DSN`；
      注明 `docker run --name keel-pg` 只是一次性本地库的便利。
- [x] 1.2 注记少数验收脚本默认 `KEEL_PG_CTR=keel-pg`（可用 `KEEL_DSN`/`KEEL_TEST_DSN` 覆盖）；
      `acceptance-perf.sh` 的 2GB cgroup 电池确实需要 Docker/Linux（E0 平台受限原因）。

## 2. 官网

- [x] 2.1 首页 EN/ZH quickstart：加本机 PG 的等价第一步与「Docker 可选」旁注。
- [x] 2.2 测试页 EN/ZH：测试只需 PG（`KEEL_TEST_DSN`）；CI 的 postgres service 与
      2GB cgroup 电池才涉及容器；不要暗示读者必须装 Docker。

## 3. 验证与收口

- [x] 3.1 `pnpm --dir site build` 四道门全绿；部署成功。
- [ ] 3.2 归档（docs-only）。
