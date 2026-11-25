# PI Parity 只读审计（2026-11-25）

基线：`main` @ `eb2a82c`（代码基线 `ad92447`，v0.4.0 updater 发布点）。
方法：静态阅读 + `go list -deps`；未跑测试、未跑 Docker、未调用 Codex。

## 结论

1. 68 条未勾：**44 无需重新实现**（38 已等价交付 + 6 库已实现）、**7 不适用**、
   **10 部分**、**7 缺失**（去重后 6 个能力）。
2. 唯一“库有、产品没接”的 PI 能力是 hooks：`internal/hooks` 不在 `cmd/keel` 依赖图，
   租户 `hooks.commands` 只校验/计数，没有加载执行点。
3. 真缺口集中在：hooks 接线、turn/tool/compaction span 树、真实 MCP server E2E；
   其余是指标/OTLP/chaos/allowed-tools/usage ledger 等加固。
4. `docs/HOST.md` 声称 allowed-tools 已取交集、hooks 已接 loop、turn/tool 已是子 span，
   与代码不符；`docs/MATURITY.md` 的 Skills/MCP 等级也落后于实现。

## 能力对照（摘要）

| 能力 | Keel 现状 | 关键证据 | 级别 |
|---|---|---|---|
| Agent loop / 事件 / 工具 | 已交付；PI 用例与 conformance 套件在 | `internal/agentloop/manifest.go`、`internal/loop/conformance_test.go` | P2（缺录制流 golden） |
| 持久任务 / 压缩 | 不同形态；kernel runs/lease + checkpoint；无 CompactionTask | `internal/supervisor/policy.go`、`internal/compaction/conformance_test.go` | P2 |
| 扩展 hooks | **库已实现未接线**；`internal/hooks` 不在出货依赖图 | `internal/hooks/*`；`go list -deps ./cmd/keel` = 0 | **P0** |
| Skills | 已交付加载/渐进披露/租户库；缺 allowed-tools 执行期交集 | `internal/skills/skills.go`、`internal/skilllib/library.go` | P1 |
| MCP | 已交付 client/stdio/Streamable HTTP/OAuth/Apps；缺真实 server E2E | `internal/mcp/*`、`docs/PI-CONFORMANCE.md` | P1 |
| 遥测 | 只有 run span + provider span；无 turn/tool/compaction 子 span、无 message_* span event | `internal/agent/telemetry.go:35`、`internal/agent/provider_telemetry.go:97` | **P0** |
| Provider 文件/usage | 文件引用已交付；usage ledger/observed stream 无生产调用者 | `internal/provider/imagefiles.go`、`internal/store/usage.go:48` | P1 |
| 收口 | 2GB/1000 SSE 脚本、chaos 工具、报告在；nightly CI 是占位 | `scripts/acceptance-perf.sh`、`.github/workflows/ci.yml` | P1 |

## 68 条去向（按节）

| 节 | 未勾 | D 已交付/等价 | W 库已实现未接线 | P 部分 | M 缺失 | N 不适用 |
|---|---:|---:|---:|---:|---:|---:|
| §0 conformance 基座 | 5 | 4 | 0 | 1 | 0 | 0 |
| §1 observability | 7 | 4 | 0 | 2 | 1 | 0 |
| §2 recoverability | 9 | 7 | 0 | 1 | 1 | 0 |
| §3 compaction | 7 | 6 | 0 | 0 | 1 | 0 |
| §4 skills | 9 | 5 | 0 | 2 | 1 | 1 |
| §5 mcp | 9 | 4 | 0 | 2 | 2 | 1 |
| §6 hooks | 6 | 0 | 6 | 0 | 0 | 0 |
| §7 provider files | 3 | 3 | 0 | 0 | 0 | 0 |
| §8 收口 | 7 | 3 | 0 | 2 | 0 | 2 |
| §9 Owner 决策 | 6 | 2 | 0 | 0 | 1 | 3 |
| **合计** | **68** | **38** | **6** | **10** | **7** | **7** |

## P0

1. **文档真值**：HOST/MATURITY 三处超前声明；生态板 6 份 spec 已被归档 spec 取代，
   解冻会产生双真源。
2. **hooks 产品接线**：事件、命令钩子、决策、审计、wrapTool 库都在，但未进出货路径。
3. **遥测 span 树**：run span 下缺 turn/tool/compaction 子 span 与 `message_*` span event。

## P1

- turns/tool/context Prometheus 指标（固定基数）。
- OTLP adapter（默认关；当前 factory 永远 memory）。
- 真实 MCP server（filesystem）零改动 E2E + G4 证据。
- nightly 10 分钟 SIGKILL CI 接线；压缩 kill 格。
- `allowed-tools` 执行期交集与类型化拒绝。
- usage ledger 与 provider observed stream 接线。

## 推荐

保持 `keel-ecosystem` 冻结；新开 `keel-pi-parity-closure`：
T1 审计入库（本 change）→ T2 hooks 接线 → T3 span 树+指标 → T4 OTLP → T5 真实 MCP E2E。
不按 delivered 归档生态板，也不修改其 tasks.md。

## 未证实

- 所有测试/conformance 数字未在本次复跑，均引自文档/归档 evidence。
- OAR live Codex 无工件（只有 fake/simulated）；MCP env/headers 日志路径未逐条审计。
- PI pin `55a5c69` 与当前上游差异未核对。
