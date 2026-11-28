# Tasks: fix-windows-pg-start

## 1. Reproduce and record

- [x] 1.1 Windows 11 实机复现 `keel.exe` 无参首次启动卡死（PG ready，13750 未监听）
- [x] 1.2 用 `keel db stop` + `keel db start` 复现（20s 超时，PG 仍启动）
- [x] 1.3 记录根因：`pg_ctl start` 的 `CombinedOutput` 管道被 postmaster 继承
- [x] 1.4 证据归档到 `evidence/`（诊断、复现输出、PG 日志）

## 2. Fix

- [x] 2.1 新增 `startPGDaemon`：`pg_ctl` stdout/stderr 写 `<dev-pg>.ctl.log`，`Run()` 不用管道
- [x] 2.2 `startDevDBCluster` 改用 `startPGDaemon`；失败错误带 ctl/PG 日志路径
- [x] 2.3 新增跨平台回归测试：fake pg_ctl 派生继承 stdio 的 5s 子进程，start 必须 <2s 返回
- [x] 2.4 `gofmt -l`、`go vet ./...`、`go test ./cmd/keel` 通过

## 3. Windows regression

- [x] 3.1 WSL 交叉构建 Windows 测试二进制，运行新增回归测试
- [x] 3.2 构建 `0.6.1` Windows 测试二进制（外部 PG/web 目录即可），干净 `KEEL_HOME`
- [x] 3.3 `keel db start` 在 15s 内返回；`dev-pg` 数据库存在；PG 继续监听 13741
- [x] 3.4 无参 `keel.exe` 启动后 `/v1/health`、`/v1/ready` 200，13750 监听
- [x] 3.5 记录修复前后对照输出到 `evidence/`

## 4. Review, release, verify

- [ ] 4.1 `cmd/keel/main.go` version → `0.6.1`；发布说明只写实际验证范围
- [x] 4.2 `openspec validate fix-windows-pg-start --strict` 通过
- [ ] 4.3 独立复审（子代理）P0/P1 清零
- [ ] 4.4 提交/推送 main；tag `v0.6.1`；CI release workflow 成功；不重跑 v0.6.0
- [ ] 4.5 下载 v0.6.1 Windows 资产，SHA-256 校验，在真实 Windows 重跑启动/探针/持久化
- [ ] 4.6 更新 `keel-windows-verification` 的验收结论与证据
- [ ] 4.7 验收通过后归档本 change；`openspec validate --specs`（记录既有警告基线）

## Evidence

- `evidence/repro-windows-0.6.0.md` — v0.6.0 卡死现象与根因
- `evidence/before-after.txt` — 修复前后 `keel db start` / 无参启动对照
- `evidence/regression-test.txt` — 跨平台测试与 Windows 测试二进制输出
- `evidence/release-v0.6.1.md` — tag/Release/SHA/CI/Windows 复验
