# Proposal: fix-serve-ready-schema

## Why

修复 Windows 首次启动的 `pg_ctl` 管道阻塞后，实机启动仍无法通过 `/v1/ready`：
健康探针 200 后 readiness 持续 503，日志为
`store: ready: missing tables: runtime_instances, handoffs`。

根因：`store.CheckReady` 的 `requiredTables` 包含 handoff 审计表，但
`runtime_instances`/`handoffs` 只在 `internal/handoff/schema` 中创建；
`cmd/runtime` 启动时调用 `handoff.NewPGRegistry` 应用该 schema，而产品壳
`cmd/keel serve` 从未调用。结果是任何 fresh 产品数据库的 `/v1/ready` 永远 503，
不只 Windows。发布门 / 产品 E2E 只等 `/v1/health`，因此没有拦住。

证据：本 change 的 `evidence/repro-before-fix.txt`、`evidence/repro-ready-503.txt`；
Windows 11 实机，修复后的 0.6.1 测试二进制。

## What Changes

- `cmd/keel/serve.go`：在 kernel migrate 之后调用
  `handoff.NewPGRegistry(ctx, st.Pool())`，应用幂等 handoff schema；与
  store/kernel 共用 advisory lock，失败即 boot 失败。
- `internal/producte2e` 的 `startServer`：health 200 后继续等 `/v1/ready` 200，
  超时输出服务日志；把“ready 是 schema 合同”纳入产品 E2E。
- `docs/RUNBOOK.md`：首启验收必须同时检查 `/v1/health` 与 `/v1/ready`，并说明
  handoff 表由两个入口共同拥有。
- 与 `fix-windows-pg-start` 一起进 `v0.6.1`；不改 `v0.6.0` tag/资产。

## Impact

- 代码：`cmd/keel/serve.go`、`internal/producte2e/vertical_w11_test.go`。
- 运行时：产品壳首次启动会多一次幂等 handoff schema migration（与 `cmd/runtime`
  相同的表与 advisory lock）；已有数据库无变化。
- 发布：`v0.6.1`（由 `fix-windows-pg-start` 的 release 任务统一执行）。
