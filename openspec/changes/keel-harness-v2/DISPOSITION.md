# keel-harness-v2 归档处置表（DISPOSITION）

归档日期：2026-11-24。口径：本 change 的 W11 收口门与 Owner 选定的处置（G3 修复、G1 矩阵、G2 可测格、容器格标平台受限）已完成；
下方 50 个未勾任务**不被本归档宣称完成**，逐条给出「本轮关闭 / 替代撤回 / 平台受限 / 后续 change」处置。

## 门级结果

| 门 | 结果 | 证据 |
|---|---|---|
| G0 契约 | PASS | e0-conformance-final（1129/0/2）、architecture 16 项、catalog、toolsurface |
| G1 状态机 | PASS | evidence/g1-coverage.md（30 台机器，0 gap）、state-facts.mjs、kindRegistry、C1/A1 矩阵 |
| G2 恢复 | PASS | TestCaptureCrashBeforeCommitRecoversExactlyOnce、kill9/subagent 恢复套件 |
| G3 幂等 | PASS | 47a96d0；provider_exhausted_no_fallback；decision 409；fs.write 幂等 |
| G5 任务 | PASS | 100k capture 1 条事件、fork 资源、taskid gate、tasklog 套件 |
| G4 内存 | PARTIAL | 峰值 468MB ≤500MB；300MB 目标未达；workload 56/64；组 OOM/cgroup 平台受限 |
| G6 插件 | PASS | g6-g9-g10.md（安装/启停/卸载零残留、HMR 回滚、catalog 103 条） |
| G7 UI | PASS（web/TUI） | nsh check 0；TUI 397/398（唯一失败＝设置页 raw JSON，前存在）；web 398 headless 4 项布局差异不计入门 |
| G9 有界 | PASS | 工具输出有界/队列 ETA/组上限/100k capture 1 事件；RSS 曲线平台受限 |
| G10 资源回收 | PASS | 零孤儿进程/端口、SIGKILL sweep、vertical §1.7 正负控 |
| G8 创造模式 | Deferred | 端到端未跑通；webview E2E 已有 |
| G11 交付面 | PASS | 报告（9 步/9 断言/8 图）+ 审计（94 文件，UNCOVERED=0）+ 子代理复核 + ADR 反向记录 |
| E0 既有门 | BLOCKED（平台） | 原生电池：conformance 1129/0/2、mcp-apps 5/5、external 15/15、core 7/8（fake MCP marker 时序）；容器电池需 Docker（本机禁用） |

## 未勾任务处置（50 项）

| 行 | 任务 | 处置 | 说明 |
|---|---|---|---|
| 22 | **既有门承接**：扩展 `cmd/r1diff` 场景与 allowlist | 后续 change | 未开始/部分；见行内对账注记 |
| 27 | **AgentTool 接口扩展**：`agentcore.AgentTool` 缺的字段 | 后续 change | 未开始/部分；见行内对账注记 |
| 41 | **ADR 0001**：layerfs Rust+gRPC 的推翻记录与证据门；CI 工具链、Dockerfile/compose、  | 替代/撤回 | ADR 0001 反向记录：Rust+gRPC 未实现，Phase A Go CAS 交付；Rust 专属 CI/Docker 随 W1 gRPC 延后 |
| 43 | **多用户进程**：租户解析、`/v1/plugins`/triggers/工具面/设置面按租户过滤、装配隔离；  | 后续 change | 未开始/部分；见行内对账注记 |
| 56 | **RSS 采样器**：`scripts/acceptance-rss.sh` 采样后端进程 `VmRSS` 求和，接 `acceptanc | 平台受限/部分 | 本机无 Linux cgroup 采样器；RSS 以 /proc 采样与 capture 事件数作结构证据 |
| 58 | **原生 target 可执行**：`web` 与 `macos` | 平台受限/部分 | web 已交付；macOS target 需签名/公证环境，延后 |
| 60 | **NSH 0.5.0 升级**：pin 0.5.0；Port Window API | 替代/撤回 | 已升级到 NSH 0.6.0（f577c51），0.5.0 跳过 |
| 62 | **客户端壳**：单实例、托盘、全局快捷键、URL Scheme 深链、文件关联、启动隐藏/菜单栏模式； | 后续 change | 未开始/部分；见行内对账注记 |
| 64 | **系统集成**：系统通知 | 后续 change | 未开始/部分；见行内对账注记 |
| 66 | **更新与分发**：`nsh/updater` 自更新 | 后续 change | 未开始/部分；见行内对账注记 |
| 68 | **长会话性能**：1000 条消息滚动保真、离屏增量创建、大表格避免重复布局、视图树消失释放内存 | 后续 change | 未开始/部分；见行内对账注记 |
| 69 | **编辑器**：Composer RichEditor | 后续 change | 未开始/部分；见行内对账注记 |
| 73 | `docs/HARNESS.md` 增补 Harness/内核边界、放置模型与多用户升级  | 后续 change | 未开始/部分；见行内对账注记 |
| 74 | 架构测试：内核包不得 import placement/task/fs | 后续 change | 未开始/部分；见行内对账注记 |
| 113 | 配额：按 group/租户的存储字节上限，超限返回明确错误 | 后续 change | 未开始/部分；见行内对账注记 |
| 114 | gRPC 契约：流式 Get/Put、取消传播、错误分类 | 替代/撤回 | 无 proto/gRPC；同接口扩展实现保留 |
| 115 | 压测：单进程 1000 并发流的 RSS 曲线；不整文件进内存的断言 | 平台受限/部分 | 1000 并发流曲线需 Linux cgroup（E0 容器电池平台受限） |
| 131 | 进程生命周期：启动、drain、接管、SIGTERM 排水；组 OOM 不扩散  | 平台受限/部分 | 组 OOM 隔离需 cgroup；drain/handoff/SIGTERM 已有测试，组 OOM 不扩散未验 |
| 334 | gbash 的 `mcp` CLI：与聚合工具共用同一个 registry | 后续 change | 未开始/部分；见行内对账注记 |
| 370 | 子代理继承父的 `config.toml` 版本，随后可写自己的配置  | 后续 change | 未开始/部分；见行内对账注记 |
| 477 | 外部工具：OAR 接 Codex 或 Claude 至少一条真链路；专用渲染器可见；任务图不被污染 | 替代/撤回 | OAR 真链路未接；W9 延后 |
| 490 | **E0 既有门零回归**：`scripts/acceptance-all.sh` 全绿、R1 `unexpected=0`、allowli | 后续 change | 未开始/部分；见行内对账注记 |
| 523 | G4 内存门：后端 RSS ≤500MB | 平台受限/部分 | G4：峰值 468MB ≤500MB 上限；300MB 目标未达（PARTIAL）；runsActiveMax=56/64 |
| 525 | G8 创造模式门：AI 现场写 Native 插件端到端 | 平台受限/部分 | G8 创造模式：端到端未跑通，Deferred（见 acceptance 处置） |
| 527 | G11 交付面门：HTML 图文 E2E 报告 + 改动文件 before/after 审计 | 本轮关闭 | 本轮已交付 |
| 537 | `keel.checkpoint`：checkpoint 策略 | 后续 change | 未开始/部分；见行内对账注记 |
| 550 | `keel.super-agent` | 后续 change | 未开始/部分；见行内对账注记 |
| 555 | `keel.creation` | 后续 change | 未开始/部分；见行内对账注记 |
| 566 | 四形态落地：`chunk` | 后续 change | 未开始/部分；见行内对账注记 |
| 594 | `keel.attachment`  | 后续 change | 未开始/部分；见行内对账注记 |
| 595 | `keel.commands`、`keel.permission`、`keel.feedback`  | 后续 change | 未开始/部分；见行内对账注记 |
| 596 | `keel.subprocess`、`keel.terminal` | 后续 change | 未开始/部分；见行内对账注记 |
| 600 | `keel.bundles` | 后续 change | 未开始/部分；见行内对账注记 |
| 601 | `keel.presets` | 后续 change | 未开始/部分；见行内对账注记 |
| 607 | `keel.telemetry` | 后续 change | 未开始/部分；见行内对账注记 |
| 608 | `keel.testkit` | 后续 change | 未开始/部分；见行内对账注记 |
| 609 | `keel.credentials`、`keel.storage`、`keel.identity`  | 后续 change | 未开始/部分；见行内对账注记 |
| 616 | `keel.oar` | 替代/撤回 | keel.oar 随 W9 延后 |
| 631 | `ui-task-board`、`ui-notifications`、`ui-jobs`、`ui-tool-renderer`、`ui-sk | 后续 change | 未开始/部分；见行内对账注记 |
| 632 | `ui-subagent`、`ui-deliverables`、`ui-secret-mask`、`ui-creation` | 后续 change | 未开始/部分；见行内对账注记 |
| 633 | 登记：`ui-trajectory` | 后续 change | 未开始/部分；见行内对账注记 |
| 647 | fileplugin settlement 锁 map 无在途回收 | 后续 change | 未开始/部分；见行内对账注记 |
| 655 | RF-1 主仓挂 submodule + PROJECT-MAP 登记 | 后续 change | 未开始/部分；见行内对账注记 |
| 656 | RF-2 为 loop 补单测 | 后续 change | 未开始/部分；见行内对账注记 |
| 657 | RF-3 loop 的持久任务接线 | 后续 change | 未开始/部分；见行内对账注记 |
| 658 | RF-4 缩略图异步生成 | 后续 change | 未开始/部分；见行内对账注记 |
| 659 | RF-5 RLS 可选启用 + 非 owner 角色 | 后续 change | 未开始/部分；见行内对账注记 |
| 660 | RF-6 每租户配额 | 后续 change | 未开始/部分；见行内对账注记 |
| 661 | RF-7 HTML 报告 + 公网链接 | 本轮关闭 | 本轮已交付 |
| 662 | RF-8 子代理交叉复核 | 本轮关闭 | 本轮已交付 |

## 后续入口

- 未完成项不新开占位 change；重启时从本表与 `openspec/changes/archive/<date>-keel-harness-v2/tasks.md` 逐条认领。
- W11 收口补充（RF-1..RF-8）中 RF-1/2/3/4/5/6 的处置见上表；RF-7/RF-8 本轮关闭。
- 报告与公网链接：https://local-kzf-keel.fde2e.com/report.html（审计同目录 audit-coverage.md）。
