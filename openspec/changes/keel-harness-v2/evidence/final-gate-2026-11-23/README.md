# 最终主线门（2026-11-23）

Change: `keel-harness-v2` · 代码基线: `main` `7366f44`
（`a549e87` + W11 + W3-config + W2 + W12-product + nsh 0.6.0 升级 + 两个测试/输入修复）

环境: macOS arm64 / Go 1.27.4 / PostgreSQL 18.7（本地单实例，**无 Docker**）/
nsh 0.6.0 (f577c51 nightly)。

## 结果

| 门 | 结果 | 证据 |
|---|---|---|
| `go build ./...` + `go vet ./...` | PASS | 无输出 |
| `KEEL_TEST_DSN=… go test -p 1 -count=1 ./...` | **PASS：76 包 ok / 0 FAIL** | `go-full.txt` |
| R1 差分（pin `e7361f8`，scenario `r1-basic-v2`） | **PASS**：152 叶子路径 / 103 allowed / **0 unexpected** / 77/77 规则 | `r1-live.txt` |
| R1 负控 1（同 dump 两侧） | **FAIL（stale allow rules）**，符合要求 | `r1-negative-same.txt` |
| R1 负控 2（注入 `runStatus.body.status` done→failed） | **FAIL**：`UNEXPECTED /runStatus/body/status` + stale rules，符合要求 | `r1-negative-mutation.txt` |
| `node scripts/plugin-catalog.mjs --check` | **OK (103 entries, §A/§B/§C mapped, §H current)** | 命令输出 |
| `openspec validate keel-harness-v2 --strict` | **valid** | 命令输出 |
| `cd web && nsh check` | **exit 0 / 0 errors**（0.6.0 升级 + speech tooLarge 处理 + surface gate 移到 app 根） | `nsh-check.txt` |
| `cd web && nsh test --target tui` | **395/396 通过**；唯一失败＝既有 settings raw-JSON 输入用例 | `tui-full.txt` |
| `scripts/acceptance-all.sh` | **不可完整运行**：DB reset 已走本地 `psql`，但 drain/inflight/perf/handoff/rss 仍要 `docker exec` + Linux cgroup v2 | 见 W11 `README.md` §2.3 |

## 口径与已知缺口

- R1 的两次负控都**按预期失败**；正跑 0 unexpected，说明 W12 的 session 读没有动到 v1
  runtime 的可观测契约。
- `tui-full.txt` 的唯一失败是既有的
  `TUI: 设置页——模型分区两层结构…`：原始 JSON 编辑器在 TUI 宿主上无法稳定接收
  含 `:` 的长文档（逐段 `type`、逐字符 `type`、`paste` 三种方式分别丢字符或不同步
  signal；0.4.0/0.6.0 都失败）。该用例路径不经过任何本次改动文件；属于既有测试/宿主
  输入路径问题，另行修。
- `internal/agent` 的 `TestCancelAfterCommittedAnswerIsNotAborted` 曾在本机全量里偶发
  “Fail in goroutine after …”，已修（poller 由 `t.Cleanup` 关闭并 join，不再在
  goroutine 里 `t.Fatal`），本文件的全量是修复后结果。
- `internal/servicehost` 的 `TestApplyModelsKeepsTheWorkerClaiming` 有 10s 级等待，在
  多任务并发（R1 + 全量 + TUI 同时跑）时曾超时；单跑与本次 idle 全量均 PASS。全量门
  应在机器空闲时跑。
- **W11 收口门：G3 已修（`47a96d0`），G1/G2 仍未绿**（见 `evidence/w11-gates/README.md`）：
  G1 缺逐状态机转移矩阵/拒绝事实（静态 state→fact 与 kindRegistry 已补，转移矩阵仍开）；
  G2 的容器重启 E2E 无 Docker 不可测、capture 崩溃格缺失。
  `keel-harness-v2` 的归档要等 G1/G2 补齐或 Owner 明确接受缺口，本次未归档。

## 清理

`keel_final_gate` / `keel_final_r1diff` / `keel_w12_fix` 等临时库已删除；R1 的
`/tmp/keel-r1-base`、`/tmp/keel-final-r1` 与 dump 已删除；无 keel serve / nsh 残留进程。
