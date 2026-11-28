# Proposal: keel-windows-verification

## Why

v0.6.0 的 Windows amd64 资产由 CI 构建并随 Release 发布，但没有在真实 Windows 机器上启动、
安装或运行过；`keel-release-closure` 的发布证据明确写着 **“Windows is built, not
runtime-verified”**。本 change 负责真实 Windows 实机验收与证据，不修改已发布的 v0.6.0。

实机验收发现 **v0.6.0 Windows 首启不可用**：

1. **P0**：无参启动后 PostgreSQL 已 ready，但 `127.0.0.1:13750` 永不监听；
   `keel db start` 20s 超时。根因是 `pg_ctl start` 用 `CombinedOutput`，postmaster
   继承管道写端导致父进程等 EOF。修复 change：`fix-windows-pg-start`。
2. **P1**：修复 P0 后，fresh `keel serve` 的 `/v1/ready` 持续 503
   `missing tables: runtime_instances, handoffs`；产品壳没有应用 handoff schema。
   修复 change：`fix-serve-ready-schema`（影响所有平台，不只 Windows）。

因此 v0.6.0 不得对外称为 Windows 已验证；两个修复随 **v0.6.1** 发布，本 change 在
v0.6.1 真实 Windows 资产上重跑验收并给出最终结论。

## What Changes

- 记录 v0.6.0 Windows 实机验收：资产 SHA-256/大小与 `SHA256SUMS.txt` 及 GitHub
  digest 一致；`keel.exe version` 输出 `keel 0.6.0`（windows/amd64）。
- 记录 v0.6.0 失败证据：无参首启卡死、`keel db start` 超时、`/v1/ready` 503；
  详见 `evidence/defects.md`。
- 跟踪两个修复 change 与 v0.6.1 发布；不改 v0.6.0 tag/资产。
- 在 v0.6.1 真实 Windows 资产上重跑：无参首启、`/v1/health`、`/v1/ready`、
  `/v1/capabilities`、Web 首页/静态资源、会话/消息、完整停机重启持久化、
  `KEEL_BOARD_ENABLED=1` 看板任务持久化、含空格/中文路径、系统集成观察、
  `keel update --check`。
- 证据保存命令输出与截图；`docs/RUNBOOK.md` 增补 Windows 验收与首启排障命令。
- 若 v0.6.1 仍有缺陷：继续记录并另开修复；不覆盖 v0.6.0 或 v0.6.1 已公布资产。

## Impact

- 代码：本 change 不改产品代码；修复在 `fix-windows-pg-start` 与
  `fix-serve-ready-schema`。
- 发布：v0.6.1（tag/CI 由 `fix-windows-pg-start` 的 release 任务执行）。
- 证据：`evidence/defects.md`、`evidence/commands/`、`evidence/screenshots/`、
  `evidence/windows-verification.md`。
