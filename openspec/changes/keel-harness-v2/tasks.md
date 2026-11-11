# Tasks: keel-harness-v2

> 顺序原则：先契约、再环境、再记忆、再放置、最后收口。每个 W 都可在上一个 W 的
> 契约冻结后并行推进；收口门（W11）全绿才归档。

## W0 契约与骨架

- [ ] 冻结本 change 的**全部 10 份 spec**（`harness-core` / `agent-loop-contract` / `layerfs` /
      `task-log` / `tool-surface` / `plugin-architecture` / `plugin-ui-extension` / `supervision` /
      `client-shell` / `plugin-system` 的 MODIFIED delta），
      后续实现不得偏离；偏离走新 change
- [ ] **kind 表**：每个新 kind 定 `class`（owner/edge/external/system）、`key`/fence、提交者；
      新增 `task.created|updated|attached|detached|closed|note`、`fs.write`、`fs.capture`、
      `fs.capture_failed`、`config.rotated`、`placement.joined|left`、
      `supervision.escalated`、`guardrail.tripped`、`resume.attempt`、`notification.ack|push_sent`；
      `run.finished` payload 增 `configHashBefore/After`；同步 `kindRegistry`、`internal/store`
      常量、转录/SSE/R1 规则
- [ ] **既有门承接**：扩展 `cmd/r1diff` 场景与 allowlist（每条附理由）、重新 pin 基线；
      `scripts/acceptance-all.sh` 必须全绿（E0）
- [ ] **工具面迁移**：按 `tool-surface` 的破坏性迁移表删除旧工具名（无 shim），加「终局工具集」
      快照断言；`agentcore.AgentTool` 缺的字段（幂等键/超时/取消/截断/错误码/内存档位）单列
      接口扩展任务
- [ ] **PI 偏离登记**：`docs/PI-PARITY.md` 写明 MCP 工具命名的偏离（理由/影响面/验收方式）；
      删除 `internal/mcp` 的 `mcp__<server>__<tool>` 适配与单测
- [ ] **契约冻结 spike**：`docs/GBASH-CONTRACT.md`（版本/协议帧/进程模型/许可证）与
      OAR 契约（record stream schema/typed 码表/进程组回收/与 `internal/externalgw` 的复用关系）；
      未冻结前 bash 与 `keel.oar` 的实现任务保持阻塞
- [ ] **ADR 0001**：layerfs Rust+gRPC 的推翻记录与证据门；CI 工具链、Dockerfile/compose、
      健康检查、端口登记（13610–13799）、SIGKILL sweep
- [ ] **多用户进程**：租户解析、`/v1/plugins`/triggers/工具面/设置面按租户过滤、装配隔离；
      `plugin_state(tenant_id, plugin_id, status, config_version, reason)` 与 `/v1/plugins` 过滤；
      `AGENTS.md` 修订为「默认单进程多租户；`user` 放置是显式 SKU」
- [ ] **内核键命名空间**：扩展 `KeyKind` 与 `CommitRequest`（`fs:`/`capture:`/`task:`/`config:`/
      `placement:`/`supervision:`/`guardrail:`/`resume:`/`notif-ack:`/`notif-push:`/`run:`），
      禁止复用 `tool:` 前缀；加「tool.result 后紧接 fs.write」负控单测
- [ ] **清单解析扩展**：`pluginpkg` 支持 `layer`（可选）、`provides.slots`、`contributes`、
      纯 chunk 包（无 `ui.entry`）、`plugin_state` 租户字段；`KnownFields(true)` 同步
- [ ] **catalog 唯一真源**：`catalog.json` 增 `keelPluginId/status/wave/dshGroups`，统一 status
      枚举（含 `partial`）；生成/比对 `COVERAGE.md`（对照 catalog）；比对脚本进 CI
- [ ] **RSS 采样器**：`scripts/acceptance-rss.sh` 采样后端进程 `VmRSS` 求和，接 `acceptance-all.sh`，
      断言 ≤500MB（目标 ≤300MB）；cgroup peak 单列
- [ ] **原生 target 可执行**：`web` 与 `macos`（`nsh package --target macos`）构建产物 + 截图；
      TUI 用 `nsh run --target tui` 真终端（0.5.0 ratatui 后端），测试用 `nsh test --target tui`
- [ ] **NSH 0.5.0 升级**：pin 0.5.0；Port Window API（`WindowRef`/`<Window>`/window kinds）；
      媒体加载 `fs.read`/`${pluginDir}` grant；`focus-visible:` 仅键盘；macOS `activationPolicy`
- [ ] **客户端壳**：单实例、托盘、全局快捷键、URL Scheme 深链、文件关联、启动隐藏/菜单栏模式；
      任务板/交付物可开独立窗口
- [ ] **系统集成**：系统通知（含 silent）＋ badge；通知点击深链；WebView `evaluate` 返回值、
      自有 URL scheme、请求头、incognito、摄像头/麦克风（不支持时显式降级）
- [ ] **更新与分发**：`nsh/updater` 自更新（check/download/install/restart）＋签名 OTA 包；
      试验期失败回滚；运行时变化走完整应用更新
- [ ] **长会话性能**：1000 条消息滚动保真、离屏增量创建、大表格避免重复布局、视图树消失释放内存
- [ ] **编辑器**：Composer RichEditor（HTML 导入/导出）；消息流式 Markdown；代码/Diff/终端独立字号；
      用 NSH `Terminal` 组件渲染后台输出
- [ ] 废止代码里的旧工具名注释与兼容红线：`internal/agentext/plugin.go`、`creation.go`、
      `subagent.go`、`internal/pluginpkg/manifest.go:227`
- [ ] `docs/HARNESS.md` 增补 Harness/内核边界、放置模型与多用户升级
- [ ] 架构测试：内核包不得 import placement/task/fs（沿用 `internal/architecture` 断言模式）

## W0.5 插件全景（对表 DSH）

- [ ] 冻结 `catalog.json` 的 id/status/wave；`plugin-inventory.md` 仅作设计期附录，不作为波次真值
- [ ] 五种 **layer**（kernel/harness/provider/ui/service）的装载边界与架构测试；
      承载 `kind` 仍是三种（in-process/sidecar/webview），两者正交
- [ ] 四层装配（base/mode/preset/config.toml）与 row id overlay 语义
- [ ] 能力 seam 清单（fs/shell/subprocess/exec-surface/terminal）与 capability matrix
- [ ] 不照搬清单写入 ADR：Cordis/typert、一能力一工具、70 个 UI 包、sandbox 族、DSH ssh
- [ ] 冻结扩展面声明格式：`layer` / `provides.slots` / `contributes` / `extensibility.json` +
      `EXTENSIBILITY.md`（创造模式的聚合输入）
- [ ] **catalog 对账**：以 `catalog.json` 为唯一机读真源，`COVERAGE.md` §A/§B/§C 逐行映射到
      `keelPluginId`；`goal`/`todo`/`plan` 并入 `keel.tasks`、
      `team` absent、`workspace` 并入 Group、`guard` 并入 `keel.supervisor`，逐行给出处置
- [ ] 为 v1 已交付的官方插件补 `extensibility.json` + `EXTENSIBILITY.md`（否则创造模式无法利用，
      且官方发布门禁会拒绝）

## W1 layerfs 服务（Rust + gRPC）

- [ ] file object：`Put/Get/Stat`，流式读写（分块，单请求内存上限固定）
- [ ] tree object：目录树按内容寻址；相同子树共享哈希
- [ ] 引用计数与 GC：无引用对象回收；并发安全的 refcount
- [ ] 配额：按 group/租户的存储字节上限，超限返回明确错误
- [ ] gRPC 契约：流式 Get/Put、取消传播、错误分类（not_found / quota / io）
- [ ] 压测：单进程 1000 并发流的 RSS 曲线；不整文件进内存的断言

## W2 Harness 放置与内核契约

- [ ] `groups` / `session_placement` 表与 lease 语义（join/leave/迁移）
- [ ] 三种策略：`pack`（默认）/ `user` / `pin`；策略在 Harness，不进内核
- [ ] 组内并发槽位（`max_concurrent_runs`）：满时排队 + ETA；用户配额在 DB 侧聚合
- [ ] 进程生命周期：启动、drain、接管、SIGTERM 排水；组 OOM 不扩散
- [ ] 注入式 run 契约：工具清单 + system sections + message-parts + invoke 回调
- [ ] run 快照：`configHash` + `anchorEventSeq` + `toolManifestHash` 写入 run 事件
- [ ] 外部内核能力矩阵与 typed rejection（沿用 v1 external 通道）

## W3 file-plugin 与 bash-plugin

- [ ] file-plugin：`read`（offset/limit/流式）、`write`、`edit`（精确匹配 + 唯一性），
      全部走 `file_manifest` + layerfs，不整文件进内存
- [ ] 写路径：每次写追加 `fs.write` 事件；路径规范化；`.git/objects` 默认不进 capture
- [ ] bash-plugin：gbash 适配（自定义命令、自定义文件系统、跨平台）
- [ ] `bash` 工具参数：`timeout`（参数而非 bash 命令）、超时转后台、返回终端信息与 pid
- [ ] JobManager：全局 + 每组上限；`nohup/jobs/ps` 视图；收养与孤儿回收
- [ ] 输出落 layerfs：只回 head/tail + blob 引用 + 退出码
- [ ] capture 默认 `per-tool-call`：stat 缓存 + 变更集，只对变了的文件算哈希
- [ ] 执行面适配器契约：`materialize(manifest)` / `capture(handle, since)`，先做本地实现

## W4 task-log（plan-plugin）与面板

- [ ] `task_cards` 行 + `task_events` **投影**（从账本重建，含重建一致性测试）；会话启动自动建初始卡
- [ ] 强制 `taskId`：所有工具调用缺 `taskId` 直接拒绝（含内部子代理）
- [ ] `tasks` 聚合工具：`create/update/attach/detach/close/list/show/note`
- [ ] 任务树投影：父子关系、跨会话 `parentTaskId` 拼接
- [ ] 资源关联：`task.attached{path, hash}` / `task.detached`；面板可打开对应 blob
- [ ] fork：`(eventSeq)` 原点；新会话 + 前缀引用 + 新卡（`parentTaskId`）
- [ ] 面板：任务图、事件流、资源预览；纯投影，不写额外真值

## W5 approve 与 notification

- [ ] `approve` 工具：`confirm/prompt/choice` 三模式 + 「其它」逃逸
- [ ] 逃逸值带 `validated:false` provenance，下游可区分
- [ ] 超时默认取安全侧；durable 决策（open/wait/answer/timeout）沿用 v1 decision 原语
- [ ] `notify` 工具与通知面板：多消费器（img 埋点 / URL 访问 / 客户端 ack）
- [ ] 消费是持久事实；有 ack 能力的消费者在场时即时消费；原生推送只做兜底
- [ ] 通知 URL 单次有效、不可猜；重启不重推

## W6 MCP 聚合工具

- [ ] `mcp` 聚合工具：`list/describe/call`，schema 发现与缓存
- [ ] 二进制内容走 content block，不走 stdout
- [ ] gbash 的 `mcp` CLI：与聚合工具共用同一个 registry（shim，不复制实现）
- [ ] 删除 `internal/mcp` 的 `mcp__<server>__<tool>` 适配与单测（PI-PARITY 已登记偏离）
- [ ] MCP server 增删不改变工具清单（清单哈希断言）

## W7 skills

- [ ] skill 文件结构：`SKILL.md` + 资源；内容 hash 去重、版本不可变
- [ ] `skills` 聚合工具：`ls`（带 6 位 hash）/ `view`（frontmatter + 精简正文）/ `manage`
- [ ] ChatInput `$` 补全：分组（技能/文件/任务），共享同一唤醒符
- [ ] `/learn`：把经历沉淀为技能（走 `skills.manage`）
- [ ] Settings `/settings/skills` + CLI `keel skills`（等价能力）
- [ ] 订阅源管理：信任标注、版本 pin、更新冲突可见
- [ ] 技能内脚本执行必须走 bash 策略，不在 skills 插件里直接执行

## W8 agent-plugin 与 fork

- [ ] 内部子代理：同进程协程，独立会话 + 独立任务卡（`parentTaskId`）
- [ ] `agent` 工具：`spawn/fork/resume/cancel/list/status`；fork 原点 `eventSeq`
- [ ] 子代理继承父的 `config.toml` 版本，随后可写自己的配置
- [ ] 并发槽位记账：父与子都占组槽位，超限排队
- [ ] 子代理返回：结构化结果 + 产物 blob 引用；父卡追加 `task.note`

## W9 oar-plugin（外部工具）

- [ ] sidecar 接入 OAR：runtime 能力矩阵、record stream（`seq` + `agentPath`）
- [ ] 外部工具渲染器：把 record stream 折叠成可读的调用/结果视图
- [ ] 不注入「请记录任务卡」；归属由 record stream 带外提供
- [ ] typed rejection：runtime 不支持的能力在启动前拒绝
- [ ] 进程回收：外部 runtime 退出/崩溃后的孤儿清理

## W10 triggers 对齐

- [ ] 沿用 v1 的 `cron/interval/webhook`；补 HMAC、去重键、漏跑补偿
- [ ] 每触发器并发上限；触发产生的会话自动建初始任务卡
- [ ] webhook 端口只绑回环或受控入口；记录审计

## W11 验收（收口门，判据见 `acceptance.md`）

**集成用例（各门的载体，必须先跑通）**

- [ ] 端到端：任务创建 → bash 执行 → capture → 面板可见 → fork 子代理 → 合并结果 → 卸载插件零残留
- [ ] 外部工具：OAR 接 Codex 或 Claude 至少一条真链路；专用渲染器可见；任务图不被污染
- [ ] 工具清单稳定性：增删 MCP server / 技能后清单哈希不变（除非 config 轮转）
- [ ] 真实模型 E2E：至少一条真 provider 链路（无凭据时用 fake provider），不得只测 happy path

**门（按 `acceptance.md` 的依赖顺序；W12 的护栏/创造/槽位项 MUST 先于本门完成）**

- [ ] **E0 既有门零回归**：`scripts/acceptance-all.sh` 全绿、R1 `unexpected=0`、allowlist 每条
      有理由、事件基线已重新 pin
- [ ] G0 契约门（终局工具集快照 + 迁移表逐名断言）· G1 状态机门（两台插件状态机 + 转移矩阵 +
      拒绝事实）先行；
      未绿则后续门不作为依据
- [ ] G2 恢复门：`kill -9` 与容器重启后全部非终态 run 自动续跑（零人工）；子代理继续原任务
- [ ] G3 幂等门 · G5 任务门（taskId 强制 / fork 精确 / 资源只记引用）
- [ ] G4 内存门：后端 RSS ≤500MB（目标 ≤300MB），组 OOM 不扩散，前端 VM 配额独立断言
- [ ] G6 插件门 · G7 UI 门 · G9 有界门
- [ ] G8 创造模式门：AI 现场写 Native 插件端到端（装→渲染→卸载无残留）
- [ ] G10 资源回收门：零孤儿进程/端口/VM + 回收证据
- [ ] G11 交付面门：HTML 图文 E2E 报告 + 改动文件 before/after 审计（UNCOVERED=0）+ 子代理交叉复核

## W12 插件补全（DSH 对表 + Owner 裁决，可与 W3–W9 并行）

**记忆卫生（W1–W2）**

- [ ] `keel.spill`：长文本溢出到存储，上下文只留引用
- [ ] `keel.compaction` 扩展：tool-result-pruner + image-offload
- [ ] `keel.checkpoint`：checkpoint 策略（何时落、保留多少）
- [ ] `keel.prompt-cache`：稳定前缀与缓存边界

**护栏与自动恢复（W1，延伸 `supervision` spec）**

- [ ] `keel.supervisor`：启动即扫描非终态 run 并**自动恢复**（崩溃 / Docker 重启 / 换代）
- [ ] `config.toml` 策略：`resume.mode=auto|manual|never`、`maxAttempts`、`backoff`、
      `onExhausted`、`providerFallback`；轮转后只影响下一次尝试
- [ ] 显式接口 `POST /v1/sessions/{id}/resume`（幂等，可带配置轮转）
- [ ] 子代理**可续跑**（中断后继续原任务，不重开）；父代理可改配置后恢复子代理
- [ ] 护栏项：重复调用提醒、单次超时、连续失败、额度/成本、内存压力
- [ ] 升级阶梯：supervisor → 父/主代理 → owner（`keel.notify`）→ `keel.super-agent`（若装）
- [ ] `keel.super-agent`（插件）：恢复 main-agent、并行多个 main-agent、结果收敛
- [ ] 每一次恢复/升级/护栏触发都是账本事实（谁、何时、为什么、结果）

**创造模式与槽位（W1，延伸 `plugin-ui-extension` spec）**

- [ ] `keel.creation`（内置）：聚合本机所有插件的 `extensibility.json` / `EXTENSIBILITY.md`
- [ ] 只读发现工具：AI 可查「本机装了什么、能怎么扩展」；目录与装配事实同源
- [ ] 脚手架 / 校验 / 审批安装 / 列举 / 删除；校验 fail-closed 且给字段级原因
- [ ] `keel.ui` 槽位机制：槽位注册与注销、贡献装载/卸载、schema 校验、目标降级
- [ ] `keel.ui` 声明基础槽位（`keel.ui.route` / `.nav` / `.action` / `.tool-renderer` /
      `.message-part` / `.sidebar` / `.settings-section` / `.surface`）；任务卡部件槽位由
      `keel.tasks` 声明为 `keel.tasks.widget`；
      第一方面板改为贡献物
- [ ] 四形态落地：`chunk`（NSH 动态导入，同 VM）、`package`（签名包，独立 VM + HostLimits，
      远小于默认 256MB）、`iframe`（兜底，显式标注）、`data`（导航项/动作 JSON，按槽位 schema）
- [ ] 桥：`keel.plugin.*` ↔ NSH `host.call`/`host.on`；能力按插件声明裁剪；类型化错误
- [ ] **验收：AI 现场写一个 Native 插件**（chunk 贡献），装→渲染→卸载无残留（web + `macos`）
- [ ] `creation` 工具始终存在；`keel.creation` 停用时 actions 返回 `plugin_disabled`，工具名不消失
- [ ] TUI 降级：`package`/`iframe` 槽位在终端显式不可用，`chunk` 可用

**产品完整度（W2）**

- [ ] `keel.session-title`、`keel.session-query`（检索/导出）
- [ ] `keel.deliverables`（交付物 + workspace changes）
- [ ] `keel.attachment`
- [ ] `keel.commands`、`keel.permission`、`keel.feedback`
- [ ] `keel.subprocess`、`keel.terminal`（provider）

**装配与运维（W2）**

- [ ] `keel.bundles`（base/mode 装配面）
- [ ] `keel.presets`（agent preset 覆盖）
- [ ] `keel.hmr`（插件热重载，不打断进行中的 run）
- [ ] `keel.telemetry`（指标 + OTel）
- [ ] `keel.testkit`（无密钥测试台 / mock-replay）
- [ ] `keel.credentials`、`keel.storage`、`keel.identity`

**能力接入**

- [ ] `keel.web-search`（复用 v1 已交付）
- [ ] `keel.oar`（外部运行时＝外部工具，专用渲染器，不进任务图）
- [ ] 登记（内核稳定后再接）：`keel.lsp`、`keel.browser-use`（fde2e-bu）、
      `keel.computer-use`（fde2e-cu）、`keel.document`、`keel.knowledge`、`keel.ptc`、`keel.workflow`

**UI 贡献（W1–W2）**

- [ ] `ui-task-board`、`ui-notifications`、`ui-jobs`、`ui-tool-renderer`、`ui-skills`
- [ ] `ui-subagent`、`ui-deliverables`、`ui-secret-mask`、`ui-creation`
- [ ] 登记：`ui-trajectory`

**明确不做**

- [ ] 不新增 `agent-team`、不新增 `goal`/`plan-mode`/`todo` 独立插件（并入 plan-plugin）、
      不照搬 DSH ssh 执行面、不引入 Cordis/typert
