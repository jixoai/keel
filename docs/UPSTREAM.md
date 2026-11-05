# 上游代码来源

本仓**不是 pigo 的 fork**（Owner 2026-11-05 裁决）。我们只搬运必要的代码到自己的仓库，
按本仓架构改造，不跟随上游演进。

## pigo

- 仓库：https://github.com/smallnest/pigo
- 许可：MIT
- Pin commit：`891d1f372cefa92b5f5a104db20521238ba5a9fe`（2026-09-12，`test: restore RegisterLiveCommands three-arg call in think_test`）
- 搬运范围：`provider`、`runtime`（Agent loop）、`agenttool`、`compaction`、`agentcore`
- 不搬：CLI/TUI/REPL、session JSONL、trust、plugin/pkgmgr、selfupdate、内置 coding 工具

### 搬运纪律

1. 每个被搬运的文件保留原始 MIT 版权头，并在文件头加一行：
   `// Copied from pigo <commit>/<path> (MIT). Modified for fde2e-agent-runtime: <一句话>。`
2. 搬运即改造：去掉全局单例、环境变量直读、`os.Exit`、TUI 依赖。
3. 改造后的文件归本仓所有；上游更新不自动合并，只作参考。

## 其它参考（未搬运）

- pi（TypeScript）：无状态执行语义的实证来源，见 `docs/ARCHITECTURE.md` §3。
- pgrundev/pgx：PG 极致调参思路（AGPL，未使用其代码）。
- Deno celld / Cloudflare Durable Objects：设计参考，未使用。
