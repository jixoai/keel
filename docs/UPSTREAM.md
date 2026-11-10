# 上游来源与对齐基线

本仓**不是任何上游的 fork**。Keel 是 **PI 的 Go 重构**：接口、事件语义与生态产物以 PI 为
唯一参照；代码按 Keel 的运行时（PostgreSQL / SSE / 多租户 / 2GB）重写。

## PI（接口与生态基线）

- 仓库：https://github.com/earendil-works/pi
- 许可：MIT
- Pin commit：`55a5c6914570e2ae31ca36297654483bbbf46883`（2026-11-05）
- 参照：`packages/agent`（loop/tool/event）、`packages/durable`（task/compaction/extension）、
  `packages/mcp`（MCP 客户端，含 `src/oauth` 全量 OAuth）、`packages/telemetry`（span/metric）、
  `packages/coding-agent`（skills/extensions/compaction 宿主）
- OAuth 运行时替换：PI 的 JSON 状态文件 / proper-lockfile → PostgreSQL 加密行 +
  独立锁池 advisory lock；接口与事件语义不变（见 `internal/mcp/oauth`）
- 对齐矩阵与 conformance 计划：`docs/PI-PARITY.md`
- 原则：契约必须一致；实现可以不同；不复制代码，不跟随上游自动合并。

## pigo（Go 实现参考）

> pigo 是 PI 的 Go 移植，作为 `provider` / loop / `agenttool` / `compaction` 的**实现参考**
> 被搬运并改造；它不是接口标准的来源——标准以 PI 为准。

- 仓库：https://github.com/smallnest/pigo
- 许可：MIT
- Pin commit：`891d1f372cefa92b5f5a104db20521238ba5a9fe`（2026-09-12，`test: restore RegisterLiveCommands three-arg call in think_test`）
- 搬运范围：`provider`、`runtime`（agent loop）、`agenttool`、`compaction`、`agentcore`
- 不搬：CLI/TUI/REPL、session JSONL、trust、plugin/pkgmgr、selfupdate、内置 coding 工具

### 搬运纪律

1. 每个被搬运的文件保留原始 MIT 版权头，并在文件头加一行：
   `// Copied from pigo <commit>/<path> (MIT). Modified for keel: <一句话>。`
2. 搬运即改造：去掉全局单例、环境变量直读、`os.Exit`、TUI 依赖。
3. 改造后的文件归本仓所有；上游更新不自动合并，只作参考。

## 其它参考（未搬运）

- pi（https://github.com/earendil-works/pi）：契约与生态基线，见 `docs/PI-PARITY.md`。
- pgrundev/pgx：PG 极致调参思路（AGPL，未使用其代码）。
- Deno celld / Cloudflare Durable Objects：设计参考，未使用。
