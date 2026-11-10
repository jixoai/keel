# 测试与验收

Keel 对「完成」的定义基于证据：一个门要么留下原始输出，要么这个断言就不该说。本页列出各门、每门证明
什么，以及复跑命令。

## 1. 日常门（任何提交之前）

```bash
gofmt -l .                 # 必须零输出
go vet ./...
KEEL_TEST_DSN="$KEEL_TEST_DSN" go test -race ./...
go build ./...
```

这正是 CI 所跑（`.github/workflows/ci.yml`：gofmt 零输出、vet、带 `postgres:18` service 的 race 测试、
build；另有 `docker build -t keel:ci .` 与定时触发的 chaos 任务）。本地等价命令见
`docs/RUNBOOK.md:359`。集成测试（blob/HTTP）在 `KEEL_TEST_DSN` 未设置时自动 skip——由此得出一条推论：
**没有 DSN 的绿灯证明的少得多**。

## 2. 一条命令跑完整套

```bash
KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence \
  scripts/acceptance-all.sh
```

`scripts/acceptance-all.sh` 串行、**fail-fast** 地跑全部门，并把原始证据落进 `$KEEL_EVIDENCE_DIR`
（`scripts/acceptance-all.sh:1`）：

| 门 | 证据文件 | 证明什么 |
|---|---|---|
| `go test -race ./...` | `race-full.log` | 全仓 race 干净 |
| PI conformance | `conformance.json`、`conformance-summary.txt` | loop 仍与钉住的 PI 行为套件一致 |
| R1 差分 | `r1-live.txt`、`r1/` | 默认装配在同输入、同库上与基线二进制逐字一致 |
| R1 负控 | `r1-negative.txt` | 差分门**能**失败（陈旧规则探针必须 FAIL；不会失败的门不是门） |
| 无状态 E2E | `e2e-8of8.json` | 幂等、SSE 游标、blob 续传、agent 回复、模型调用中 kill、工具执行中 kill、100×2000 事件连续（空库） |
| SIGTERM 排水 | `drain-smoke.txt` | 排水期健康探针可达、新写 503、SSE `drain` 帧到达、进程退出、重启后恰好一条回复 |
| in-flight 输入 | `inflight.txt` | claim 之后落账的输入被真正回答（工具恰好执行一次） |
| 外部网关冒烟 | `external-smoke.log` | 真实 runtime + PG + 参考 sidecar 的 15 步，以 `SMOKE PASS (15 steps)` 与 0 残留收尾 |
| 混沌 kill 模式 | `perf/chaos-kill.json` | 10 分钟随机 SIGKILL：每条输入被回答一次、run 状态收敛 |
| 混沌 handoff 模式 | `perf/chaos-handoff.json` | 边崩边接班，以 `runtime_instances`/`handoffs`/`owner_epoch` 为接管证据 |
| 2GB / 1000 会话 | `perf/final-1000.json` | cgroup 峰值在 2GB 之内、0 硬流错误、429 归入准入反压 |
| 双进程 handoff RSS | `perf/handoff-dual.json` | 两进程窗口（旧的排水 + 新的接管）也在预算内 |

让证据可信的两条纪律：

- **每次运行前先截断工件**。在同一目录重跑绝不留下两次运行的拼接输出——陈旧工件就是会撒谎的工件
  （`scripts/acceptance-all.sh:52`）。
- **每个门独占数据库**。会 TRUNCATE/DROP 的工具拒绝跑共享库：`cmd/e2e -reset` 要求专用库，`r1diff`
  要求库名含 `r1diff`，压测工具要求库名含 `e2e/test/smoke/perf/accept`（`docs/RUNBOOK.md:182`）。
  一次打到共享库上的旧 schema 迁移，就足以删掉别的线程正在使用的行——runbook 里登记了这次事故。

## 3. 单独跑各个门

```bash
# 无状态 E2E（7 个场景，真实 SIGKILL）
go build -o /tmp/keel-e2e ./cmd/e2e
KEEL_DSN="$KEEL_TEST_DSN" /tmp/keel-e2e -bin /tmp/keel-runtime -port 13745

# loop conformance（PI 套件口径）
scripts/conformance.sh

# 重构平价：基线二进制 vs 候选，同剧本规范化比对
bash scripts/build-r1-baseline.sh e7361f8 /tmp/keel-r1-base
go run ./cmd/r1diff -old-bin /tmp/keel-r1-base -new-bin /tmp/keel-r1-cand \
  -dsn 'postgres://…/keel_r1diff?sslmode=disable' -out /tmp/r1 \
  -port-old 13782 -port-new 13783 -allow openspec/changes/archive/keel-service-graph/r1-allow.json

# 排水 / in-flight / 2GB 压测 / 双进程 handoff / 混沌
KEEL_DRAIN_BIN=/tmp/keel-runtime bash scripts/acceptance-drain.sh
KEEL_INFLIGHT_BIN=/tmp/keel-runtime bash scripts/acceptance-inflight.sh
bash scripts/acceptance-perf.sh 1000 60s
bash scripts/acceptance-handoff-rss.sh
go build -o /tmp/keel-chaos ./cmd/chaos && /tmp/keel-chaos -bin /tmp/keel-runtime \
  -dsn 'postgres://…/keel_chaos_e2e?sslmode=disable' -port 13795 -sessions 8 -duration 10m \
  -kill-every 20s -mode kill
```

R1 基线由**固定 commit** 构建（`build-r1-baseline.sh e7361f8 …`），不是从 `main` 构建——`main` 会移动，
基线不能动。

## 4. 我的改动需要哪些门？

| 你改了 | 最低门槛 |
|---|---|
| 某包内部，不涉装配 | `gofmt`/`vet` + `go test -race ./...` |
| 默认装配、内核或 HTTP 契约 | 完整 `scripts/acceptance-all.sh`（R1 差分强制） |
| 事件词汇或提交校验 | R1 差分**加** E2E 与混沌门——转录/SSE 规则随 kind 一起移动 |
| 外部 provider 集成 | `go test -race ./internal/gateway ./internal/externalgw` + `scripts/external-gateway-smoke.sh` |
| provider 适配器 | provider 测试 + 真实模型 E2E 路径（`docs/REAL-MODEL-E2E.md`） |
| 只改文档 | 以上都不需要；站点构建门（`pnpm --dir site build`）适用 |

## 5. 成熟度阶梯

每个能力的门定义在 `docs/MATURITY.md:8`：**G0** 规格、**G1** 单机功能（fake provider、无网络）、
**G2** 崩溃一致（每个 await 点 kill -9，进 CI）、**G3** 可观测（每 run 一条 `run.finished`、固定基数
指标、结构化日志无敏感内容）、**G4** PI 对齐（PI 录制流回放一致，真实 `SKILL.md`/MCP/hook 产物零改动
可用）、**G5** 规模与运维（2GB / 1000 会话 / 64 并发 run、10 分钟随机 kill、排水/就绪、文档 + 示例 +
安全评审）。

其中两条规则影响你怎么汇报工作：

- **不得跳级**。G2 未过就不算「可恢复」，G3 未过就不算「可运维」；`docs/MATURITY.md:37` 同时带诚实
  缺口列。
- **G4 用真实上游产物**——真实 `SKILL.md`、真实 MCP server、真实 hook 配置——不用自造样例
  （`docs/MATURITY.md:51`）。

## 6. 汇报一次验收

本项目对证据的交付标准是带逐步截图与断言的 HTML 报告（本站自身的 change 用的就是这套纪律）。跑运行时
验收时，请把原始 `$KEEL_EVIDENCE_DIR` 与摘要放在一起：JSON/LOG 工件才是让断言可核的东西，而
`acceptance-all.log` 里每条结论行都点名它背后的工件。
