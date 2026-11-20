# MCP Apps + events 验收总表（G0–G5）

- 日期：2026-11-20
- 代码快照：`55a795e`（P6G/P6D 合入后；其后仅为证据/任务板提交）
- 结论：G0–G5 均已跑通；P0/P1 清零。P3.6 浏览器权限授予与第二 origin 为已登记的
  上游/后续依赖，不在本 change 假装完成（见 §7）。

## G0 — 规格与接口

| 项 | 证据 |
|---|---|
| 两份 spec 共 28 条 requirement 逐条对表：25 已实现 / 3 偏离（A5 第二 origin+NSH flags、A7 teardown 已接受、A12 permissions 上游依赖） | `evidence/p6/docs/parity.md` |
| `openspec validate feat-mcp-apps-events --strict` | valid |
| PI 偏离登记 | `docs/PI-PARITY.md` §4.3 |

## G1 — 单机端到端

| 项 | 证据 |
|---|---|
| 真实 `keel serve` + 真实 stdio MCP server：app 目录/`/view` CSP/代理 `tools/call`/`tools/list_changed` 换代 | `cmd/e2e -scenario mcp-apps`；`evidence/p6/crash/run1/`、`run2/` |
| 真实浏览器：登录→会话→工具行→iframe 挂载→握手→tool-input/result→View→Host `tools/call`→全屏/退出 | `scripts/mcp-apps-e2e.sh`；`evidence/p6/browser/final2/`（P6V 视觉修复后，report.html + 11 步截图；历史 final/）；公网 `https://local-gaubee-reports.fde2e.com/keel/mcp-apps-events/browser/report.html` |
| 降级面：握手超时 final 5009ms 文本兜底/重试（早 lane 5011ms，同一断言）、CSP 真实阻断未声明域名 | `scripts/mcp-apps-degrade-e2e.sh`；`evidence/p6/degrade/final2/`（P6V 后）；公网 `https://local-gaubee-reports.fde2e.com/keel/mcp-apps-events/degrade/report.html` |
| TUI 文本兜底 | `nsh test --target tui` 179/179（含 mcp-app/TUI 用例） |

## G2 — 崩溃一致

`scripts/mcp-apps-crash.sh` 连续 2 轮 PASS，每轮 3 个可证明的 SIGKILL 点：

| 点 | 断言 |
|---|---|
| G2a app 文档取回后、tool.result 提交前 | 重启后 seq [1]→[1..6]，1 个 user.message、1 个 tool.result，details.app 保留 |
| G2b listen 重连中 | 账本 [1,2,3] 重启前后不变且连续；新进程重新 listen 并 ack；目录重算同 appId/hash |
| G2c tool.result 提交前 | at-least-once 执行，账本恰好 1 个 tool.result；审计无半行/孤儿 |

证据：`evidence/p6/crash/run1|run2/`（`assertions.json`、`kill-points.json`、`comparisons.json`、日志）。

## G3 — 可观测

- 六族齐全：`mcp_servers{state}`、`mcp_surface_generation`、`mcp_app_fetch_total{result}`、
  `mcp_app_bridge_frames_total{method,direction}`、`mcp_listen_reconnects_total`、`mcp_mrtr_rounds`。
- 100 vs 1000 会话的 mcp_* series 均为 13 且完全一致（不随会话增长）。
- 审计扫描：G3 13 行；崩溃矩阵 run1 54 行、run2 57 行，均无消息正文/token/URL query/CR-LF/坏 UTF-8。
- `keel serve` 现导出 run/queue/stream 指标（`cmd/keel/serve.go` +8 行，G5 需要）。
- 证据：`evidence/p6/crash/run*/audit-*.json`、`metrics-*.txt`；`evidence/p6/perf/metrics-*.prom`。

## G4 — PI 对齐

- `./scripts/conformance.sh`：**1077 pass / 0 fail / 1 skip**（helper process；P5 基线 1071 → 修复/新增 6）。
- legacy 断言未改；modern era/MRTR 行为见 `evidence/p5/`。
- 边界：CI 无 vendored 官方 `ext-apps` example server，使用同协议形态 fake MCP server；
  按 design §5.2 允许的替代方式记录，不声称真实 ext-apps server 已验收（`parity.md` §3）。

## G5 — 规模/混沌

`scripts/mcp-apps-perf.sh` PASS（2GB cgroup，专用 DB/网络，10 分钟混沌）：

| 指标 | 实测 | 门槛 |
|---|---|---|
| 会话 / run | 1000 会话；maxActive 64；1000 run 完成、0 失败 | 1000 / 64 |
| app 目录 | 100/1000/64-run 快照 canonical 均 4281B、hash 相同 | 不随会话增长 |
| mcphost heap | 31.233MB @100/@1000/64-run（delta 0） | 不线性增长 |
| 缓存 LRU | 10 代 ×20 app 后 61.57MB | ≤64MiB |
| 桥流量 | 12001 req / 60.0019s = 200.0/s；0×429/5xx；p99 56.06ms | 200/s ≥60s |
| cgroup 内存 | 416.5MiB peak | <1.8GiB |
| 混沌 | 15 SIGKILL + 14 listen 断流；38 次重连；最大恢复 951ms；退避上限 30.014s | ≤30s 上限 |
| 指标 | 六族 13 series，100/1000 一致 | 固定基数 |

证据：`evidence/p6/perf/`（summary.json/txt、perf.json、heap/pprof、metrics、audit、catalog）。

## 6. 最终门禁（合并树）

| 命令 | 结果 |
|---|---|
| `go build ./...` / `go vet ./...` | EXIT 0 |
| `KEEL_TEST_DSN=... go test -p 1 -count=1 ./...` | EXIT 0（55 包 ok） |
| `./scripts/conformance.sh` | 1077 / 0 fail / 1 skip |
| `cd web && nsh check` | EXIT 0 |
| `nsh test --target tui` | 179/179 |
| `nsh build` / `nsh package --target web` | EXIT 0 |
| `scripts/mcp-apps-e2e.sh`（最终树） | PASS（真实 Chrome，11 步；P6V 视觉修复后 final2） |
| `scripts/mcp-apps-degrade-e2e.sh`（最终树） | PASS（handshake + CSP；final2） |
| `scripts/mcp-apps-crash.sh`（最终树） | PASS 2/2（每轮 3 kill） |

最终运行原始输出：`evidence/p6/gates-final/`（go-test-all / conformance / tui / browser-e2e /
degrade-e2e / crash）。

## 7. 已接受偏离 / 未完成项（不冒充）

- **P3.6 浏览器权限授予**：NSH 0.4 WebView 无 per-permission `allow` 控制、单 origin 沙箱无
  `allow-same-origin`；camera/mic/geolocation/clipboardWrite 不授予、不声明
  `hostCapabilities.sandbox.permissions`。破坏性工具逐次确认已交付。需 NSH 上游/第二 origin
  后续 change。
- **A5 第二 origin 双 iframe**：`KEEL_MCP_APP_ORIGIN` 仅原生基址；web 同源 opaque frame。
- **A7 teardown 1s 等待**：NSH 组件卸载同批移除 iframe，无法真实等待；发送后立即销毁，
  已被 P6E 记录为接受偏离（无数据/安全影响）。
- **A12 permissions**：同 P3.6。
- **P6V 视觉修复**：全屏 Dialog 100%/fill、iframe flex 增高、epoch 时间隐藏、降级态隐藏全屏按钮
  （P6A1/P6A2a final2 已复跑；vision 复审见 P6.4）。
- **MRTR 轮次环境变量**：固定 5，`KEEL_MCP_MRTR_MAX_ROUNDS` 未实现（冻结 spec 只要求默认预算）。
- 其余 Non-Goal 见 tasks.md 末尾 (a)–(e)。

本文件不含 DSN 或密钥。
