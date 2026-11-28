# Tasks: keel-plugin-split

## 0. Plugin storage contract

- [ ] plugin kernel：tenant-scoped plugin_state/config/install/dependency status
- [ ] domain store interface + PG adapter + conformance；board/superagent 各用自己的 store
- [ ] attachment/telemetry/commands/UI/MCP/skills 明确不新增独立 domain DB
- [ ] no SQLite；保留 adapter 边界

## 1. Board plugin

- [ ] board 插件化；暴露 task/channel/thread/append/query 服务
- [ ] board 存储/API 不 import superagent、不 import 旧 bridge
- [ ] board 加载/卸载/禁用测试

## 2. Superagent plugin

- [ ] superagent 插件化；ticket/assignment/attention/outbox/subscription/cursor/report/lease 自有存储
- [ ] `requires: [board]`；board 缺失 → PENDING
- [ ] 删除 `internal/superagentboard` bridge/mirror 和双向直接依赖
- [ ] 通过 board service 读写任务面；无 board 表访问

## 3. Breaking integration

- [ ] 重建 board/superagent 表；不保留旧兼容层/双写
- [ ] 插件依赖拓扑装载/反序卸载测试
- [ ] E2E：board 插件任务 → superagent ticket/assignment → board 回写
- [ ] `go test -p 1 ./...`、nsh check、OpenSpec strict
- [ ] 独立子代理复审 P0/P1=0；archive
