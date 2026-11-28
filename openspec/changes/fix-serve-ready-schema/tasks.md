# Tasks: fix-serve-ready-schema

## 1. Reproduce

- [x] 1.1 修复后的 0.6.1 测试二进制首启：health 200，`/v1/ready` 持续 503
- [x] 1.2 日志定位 `missing tables: runtime_instances, handoffs`
- [x] 1.3 确认只有 `cmd/runtime` 调用 `handoff.NewPGRegistry`，`cmd/keel serve` 没有
- [x] 1.4 证据：`evidence/repro-before-fix.txt`、`evidence/repro-ready-503.txt`

## 2. Fix

- [x] 2.1 `cmd/keel/serve.go` 在 kernel migrate 后应用 `handoff.NewPGRegistry`
- [x] 2.2 失败路径返回 `handoff migrate: ...`，不静默继续
- [x] 2.3 `internal/producte2e` 的 `startServer` 等 `/v1/ready` 200，超时带日志
- [x] 2.4 `gofmt`、`go vet ./...`、`go test ./cmd/keel`、产品 E2E 编译通过

## 3. Verify

- [x] 3.1 重建 0.6.1 Windows 测试二进制，首启 `/v1/ready` 200
- [x] 3.2 `runtime_instances`、`handoffs` 存在（ready 200 即证明，日志无 missing）
- [x] 3.3 重启后 `/v1/ready` 仍 200，数据持久化验收继续
- [x] 3.4 `openspec validate fix-serve-ready-schema --strict` 通过

## 4. Release

- [ ] 4.1 与 `fix-windows-pg-start` 一起进 `v0.6.1`（version/tag/CI 由该 change 的 release 任务执行）
- [ ] 4.2 在 `keel-windows-verification` 记录 v0.6.1 的 ready 证据
- [ ] 4.3 验收通过后归档本 change；`openspec validate --specs`（记录既有警告基线）

## Evidence

- `evidence/repro.md` — 503 日志与 schema 归属
- `evidence/fixed-ready.txt` — 修复后 `/v1/ready` 200（0.6.1 测试二进制）
