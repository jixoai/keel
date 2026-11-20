# Keel 残留任务盘点与推进计划

> 日期：2026-11-14 · 口径：**任务板（tasks.md）只是索引，真值 = 代码 + 证据**。
> 完成标准对齐 `docs/MATURITY.md` 的 G0–G5 闸门（规格 → 单机 → 崩溃一致 → 可观测 → PI 对齐 → 规模/运维）。

## 0. 一个必须先说清的口径问题

`keel-harness-v2` 的 tasks.md 是 0/142 —— 这不代表 142 项都没做，而是**任务板从未维护**：
其中 W0（契约）、W2（内核契约）、W3（file/bash 插件）、W4（task-log）、W7（skills）等大量内容
已经以代码/测试/spec 形式落地。同理 `runtime-foundation` 的「主仓挂 submodule + PROJECT-MAP 登记」
已核实完成（`.gitmodules` + `PROJECT-MAP.md:36`）。
因此 **Wave 1 的第一步是「任务 ↔ 代码对账」**：逐项标注 已做/未做/被替代，更新勾选，避免重复实现。
对账之前的任何「完成度百分比」都不可信。

## 1. 现状总览（2026-11-19 更新：4 个 change 已归档）

| change | 勾选/总数 | 残留性质 | 状态 |
|---|---|---|---|
| keel-agent-team-v1 | 46/61 | 15 项：CLI start/stop、team save/start、文件引用编辑器、五态/390 复核、媒体适配器 4、验收 3 | 运行注入已收口（复核 B1–B4 修复完毕）；尾项入 Wave 3 |
| keel-session-surfaces-v1 | 23/24 | 1 项=成员结果进 captain 时间线 | 随 W2.4 task-log 收口 |
| provider-secret-direct-config | 7/8 | 1 项=真库 E2E（同名 secret 命中，需假上游） | 待排期 |
| keel-ga | 36/43 | §2/§3/§5 已交付；剩 §1 压缩文档、§6 验收 5 | tenancy/provider-files/blob-lifecycle 已交付（blob 独立复核中） |
| keel-harness-v2 | 43/146 | **W2.1–W2.5 + seam + jobcap + aclack 已交付**；余下 W2 放置与快照、W3 尾项（含新增的删除对账/capture 策略覆写/并发 capture 定序）、W5–W7、验收 | **主战场（A 轴 8.5 无 P0/P1；B 轴 8/10 两 P1 已关闭；blob-lifecycle lane 在跑）** |
| ~~feat-mcp-apps-events~~（已归档 2026-11-20） | 44/50 | 设计 + T0.1–T0.4 + P1 + P2.1–P2.8 + P3.1–P3.5/P3.7/P3.8 + P4 + P5 + P6.1–P6.3/P6.5 已交付（P5 复核修复合入 `b8ca79b`，A/B P0/P1 清零；G0–G5 证据 `evidence/p6/ACCEPTANCE.md`）；P6.4 已通过（reviewer 9/10 + vision）；剩余 P3.6（NSH per-permission allow 上游依赖）与 5 个已登记 Non-Goal | **已归档**（`2026-11-20-feat-mcp-apps-events`）；剩余 P3.6 上游依赖与 5 个 Non-Goal 留后续 change |
| runtime-foundation | 40/47 | **冻结历史板**（文首明示勿再勾选）：残留已并入 `keel-service-graph` | 只作历史对照 |
| keel-ecosystem | 冻结历史板 | **去向已清**（对账）：已承接 41 / 孤儿 16 / 废弃 8 / 转 keel-ga 3 | 孤儿项已并入下列 Wave |
| ~~fix-session-delete-soft-archive~~ | 11/11 | — | **已归档 2026-11-19** |
| ~~keel-demo-provider-v1~~ | 23/23 | 真实麦克风桥接为已知设计缺口（非阻塞） | **已归档 2026-11-19** |
| ~~keel-trace-triage-v1~~ | 15/15 | NSH resize 整树重建 DOM 为仓外负结果（backlog） | **已归档 2026-11-19** |
| ~~keel-ui-polish-v1~~ | 11/11 | — | **已归档 2026-11-19** |
| keel-service-graph（已归档） | 89/91 | 2 个已知缺口：DSH sidecar 参考适配器 pin、gateway 故障 scenario | 缺口并入 Wave 3/4 |
| keel-tenancy-usage（已归档） | 29/29 | tenant-config / usage-ledger / provider-telemetry；**不含** api_keys/auth/RLS/配额 | 完成 |

## 2. 去重归并（多 change 重复项只算一次）

- **RLS / 每租户配额**：runtime-foundation ↔ keel-ga §2 → 归 **keel-ga tenancy-quotas** 执行。
  注：`keel-tenancy-usage`（已归档 29/29）只做了租户配置/用量账本/telemetry，
  api_keys、`internal/auth`、provisioning、RLS、带宽/存储配额仍是真缺口；run/流配额已由
  `keel-service-graph` 接线（internal/quota），kernel 侧 token 用量账本已存在。
- **Provider 文件（blobId/adapter/缓存）**：keel-ecosystem §7 ↔ keel-ga §3 → 归 **keel-ga provider-files**。
- **归档遗留缺口**（无活跃任务板承接，本计划接管）：
  `keel-service-graph` 的「DSH sidecar 参考适配器」→ 并入 Wave 4 插件波；
  其「gateway 故障 scenario（crash-loop/大帧取消/stderr 洪泛）」→ 并入 Wave 3 可恢复性。
- **Agent/插件补全**：keel-harness-v2 W8/W12 与 agent-team 插件面部分重叠 → 对账后合并归属。
- **媒体适配器**（图/视/音/决策）与 ecosystem 的「能力」不同轴 → 独立清单（Wave 2）。

## 3. 计划（波次；每波开头对账，结尾 G 级证据 + 勾选 + 提交）

> 2026-11-14 对账后重排：真缺口 TOP10 集中在 **W0/W3/W4/W5/W7/W12**（恒定工具面、task-log、
> layerfs、bash-plugin、approve/notify），即「依赖注入注册面」本身——按 Owner 口径，
> 这是主干而不是插件尾巴。Wave 2 因此独立为「注册面主干」。

### Wave 0 — 运行注入收口（进行中）
- 线 A：RUNTIME-PLAN P1→P6（selection 词表 → PromptResolver → 工具面过滤 → captain/成员 → run 快照/前端）
- 同名密钥回退（provider-secret-direct-config 待实现项）
- 门：wire 标记 `KITE-RUNTIME-MARK-7/8` + 全量 Go/TUI 绿 + tasks 勾选 + 提交推送

### Wave 1 — GA 底座（并行 2 条后端线）
1. **keel-ga tenancy-quotas**：api_keys 表 + internal/auth + provisioning + RLS + 带宽/存储配额 + 跨租户测试
   （run/流配额与 token 用量账本已有，只补差）
2. **keel-ga provider-files**：ImageRefContent/事件只存 blobId/能力位/resolver(file_id>url>base64)/Gemini·OpenAI·Anthropic 适配/缓存/内存上限测试

### Wave 2 — 注册面主干（DI spine；TOP10 #1-#5）
3. **恒定工具面与聚合工具**：read/write/edit/bash/tasks/approve/notify/mcp.call/skills/agent/creation…
   （`mcp__` 方言删除，PI 偏离登记；工具面注册 = 插件 provides）
4. **bash-plugin + JobManager**：gbash 契约、timeout→后台、head/tail→blob、作业收养（skills 脚本执行的依赖）
5. **task-log**：task_cards/task_events 投影、强制 taskId、tasks 工具、fork
6. **layerfs（Go CAS 路线 Phase A）+ file-plugin**：CAS/tree/refcount/capture/写穿；Rust+gRPC 保留为
   同接口的扩展实现（裁决见 §5.1-5）
7. **approve + notify**：三模式审批、validated provenance、多消费器、URL 生命周期
- 门：G1-G2（fake 全链 + 固定崩溃矩阵进 CI）
- **派工定界（2026-11-15）**：spec 已齐（本 change `specs/tool-surface|task-log|layerfs|plugin-*`，
  strict 校验过），**不另立 change**；按下列阶段推进——
  - W2.0 `docs/GBASH-CONTRACT.md` 契约 pin（bash 开工硬前置；研究 spike 进行中）；
    OAR 契约随 W9 前另派
  - W2.1 工具面骨架**单线先行**：11 工具注册面 + action 闭集路由 + 清单哈希 + 迁移快照断言 +
    `mcp__` 适配删除 + PI-PARITY 偏离登记（`spawn_subagent` 按 harness-v2 迁移表并入
    `agent.spawn`，不留过渡名；§5.1-1 已按此更新）
  - W2.2–W2.5 在 W2.1 合入后**并行分线（文件分区）**：bash+JobManager / layerfs+file /
    task-log / approve+notify
  - 顺序依赖：W2.2 待 W2.0；W2.3 复用 blob（Wave 1 线 B 之后）；W2.4/W2.5 的 store/agentapi
    增面待 Wave 1 收口

### Wave 3 — 运维与产品尾项（并行 3 条）
8. **blob-lifecycle**：缩略图异步 + 事件引用计数 + TTL sweeper + base64 fuzz + 文档
9. **agent-team 尾项**：CLI `agent start/stop`、`team save/start`、AGENTS.md 文件引用编辑器、五态/390、组合页视觉复核
10. **媒体适配器**：图 → 决策 → 音 → 视（provider 崩溃隔离 + usage 计量不写价格）

### Wave 3.5 — MCP Apps + MCP events（P1–P5 已交付；P6 验收进行中）

- **已交付**：`internal/mcp` 的 `_meta.ui`/visibility、`resources/*`/`prompts/*` 客户端方法
  （P1）；`internal/mcphost` 的 app 目录/文档缓存/CSP/租户语义与前端 View（P2）；
  宿主代理 `tools/call`、`resources/read`、`ui/message`、`ui/open-link` 与持久审计（P3）；
  通知分类、`subscriptions/listen`、去抖 republish、generation 与 `mcp.surface.changed`
  （P4）；现代 era/`server/discover`、`resultType`/MRTR、legacy 服务端请求等价路径、
  `ui/update-model-context` 与 `/v1/mcp/servers` 的 era 可观测（P5，合入 `4a0d6d3`）。
- **P6 进行中**：P6.1 G0–G5 证据与崩溃/规模矩阵、P6.2 浏览器 E2E（每步截图 + HTML 报告）、
  P6.3 降级面 E2E、P6.4 独立子代理交叉复核、P6.6 archive/提交推送。
  P6.5 文档（`docs/HOST.md` MCP 段、`docs/PI-PARITY.md`、`site/docs/zh/harness/mcp-apps.md`
  与站点双语门禁要求的 `site/docs/harness/mcp-apps.md`、本计划）已交付；G0/G4 逐条对表见
  `openspec/changes/archive/2026-11-20-feat-mcp-apps-events/evidence/p6/docs/parity.md`。
- **未完成/上游依赖**：P2.8 的独立降级 E2E、P3.5 的浏览器全屏切换 E2E 归 P6；P3.6
  camera/microphone/geolocation/clipboardWrite 的授予依赖 **NSH per-permission allow**
  （0.4 无该控制面），当前诚实不授予、不假装声明；`KEEL_MCP_APP_ORIGIN` 的第二 origin
  双 iframe 形态仍暂缓（当前只作为桌面/移动 WebView 的绝对文档基址）。
- **本轮文档对表暴露的代码缺口（不得勾销）**：MCP Apps 客户端能力声明未实现
  （`io.modelcontextprotocol/ui` 未进 `ClientCapabilities.extensions`/`experimental`）；
  `_meta.ui.domain` 解析后未随 `AppSpec` 目录下发；`ui/resource-teardown` 在 web 卸载路径
  未真正等待。详见 `evidence/p6/docs/parity.md` 的偏离/未实现行。

### Wave 4 — 可观测/恢复/压缩（ecosystem P1 + 孤儿项）
observability（指标/spans 接入、OTLP adapter 默认关）→ recoverability（generation 幂等/task 状态机/checkpoint
回放/10min 混沌 + SG-2 gateway 故障格含「双租户同名工具」「provider 图片内容」）→ compaction（手动 compact 端点、token 账）

### Wave 5 — 生态产品面 + MCP 标准（含 16 孤儿项）
skills 产品面（`/skill:name`、租户注册表 hash 不可变、allowed-tools 执行期交集、热更新）·
hooks 补缺口（已基本实现）· MCP 产品面（per-tenant manager、env/headers 加密、健康检查/退避、破坏性默认审批、
真实 server G4）· **MCP Apps + MCP events（Wave 3.5：P1–P5 已交付，P6 验收进行中）** ·
SG-1 DSH sidecar 适配器

### Wave 6 — 插件 UI 槽位与放置模型（W12 剩余 + W2 放置）
keel.ui 四形态（inline/fullscreen/pip/panel 槽位）、TUI 降级、AI 现场写插件（Native chunk）·
groups/session_placement/lease、pack/user/pin、max_concurrent_runs+ETA

### Wave 7 — GA 验收与收口
1000 会话/64 并发 run/2GB/SIGKILL 10min 混沌/交叉复核（子代理）/每能力文档/全部 change archive/主仓指针

## 4. 本机部署现状（今天调整）

- **模型通道 = 阿里百炼订阅**：`magpie / bailian-token-plan/qwen3.8-flash`（唯一可见 provider）。
  其余 7 个内置 provider（s2api/a6api/deepseek/anthropic/openai/openrouter/ollama）已逐个
  `--visibility hidden`（revision 17，可逆）——它们是陈旧声明，本部署从未有过对应 key，
  不应再要求用户补 key。
- **租户密钥库已启用**（`KEEL_SECRET_KEY` 在 gitignored `.env.local`）：
  设置 › 密钥 可直接保存凭据，provider 用 `secret:NAME` 引用；该链路已端到端实证
  （假上游断言 Bearer 命中，`run.finished=done`）。
- `serve-local.sh` 同时支持 `*_KEY` env 透传（云端部署形态不受影响）。

## 5. Owner 裁决（2026-11-14）

**架构第一原则（Owner 原话提炼）**：「插件的本质，就是依赖。一切皆插件，本质就是在实现一套
围绕依赖注入搭建起来的系统」——插件注册 = 功能模块注册；hooks / tools / providers / skills
都是同一注册面上的能力，不是可选项。本计划据此排序：**先补注册面（plugin/DI 基座），再补能力**。

| # | 裁决 | 落地含义 |
|---|---|---|
| 1 | Skill 可执行脚本：**默认允许**（可选安全模式） | skills-plugin **依赖** bash-plugin；无 bash 则脚本不可运行（按能力缺失呈现），不在 skills 内造执行器 |
| 2 | MCP v1 **含 stdio** | 与 PI 对齐；进程管理归 mcp 插件 |
| 3 | Hooks **必须完备** | hooks 是插件暴露的能力、DI 架构的基础功能；「做不做」不是问题，「随插件一起完备」才是要求 |
| 4 | 可观测 **全面支持 OTLP，默认关** | 配置开启；架构不留死路 |
| 5 | 决策模型（decision 家族）**必须支持** | 主流标准模型之一，作为 provider 能力族一等公民，提前到 Wave 2 |
| 6 | 稳定 SDK：暂不抽（主代理决策） | 内部 API 先行，GA 前再评估 |
| 7 | PI pin：跟随上游（主代理决策） | 每次升级独立 change：pin + conformance + PI-PARITY |
| 8 | 媒体适配器顺序（主代理决策） | 图 → 决策 → 音 → 视 |
| 9 | **Alpha 阶段零兼容**（Owner 指令 2026-11-14，记住！） | 不考虑任何向下兼容；发现任何架构异常**推进到底完全重做**，不留兼容层/迁移垫片（如工具面迁移直接删除 `mcp__` 方言）；Owner 提出的需求一律 T0/T1，不再回问优先级 |

## 5.1 对账冲突裁决（2026-11-14，主代理）

对账 `RECONCILIATION.md §4` 提出 5 项归属/口径冲突，裁决如下：

1. **`spawn_subagent` 去留（2026-11-19 W2.1 更新）**：按 harness-v2 迁移表**删除**，
   语义并入 `agent` 聚合工具的 `spawn` action（子输入 `selection.agentId` 的 binding 传播不变）；
   团队模式 R3 的自动保留原语改为 `agent`。不再保留 `spawn_subagent` 过渡名。
2. **`mcp__` 命名**：以 **harness-v2 W0 为准**——终局工具面 = 恒定聚合工具（`mcp.call`），
   `mcp__<server>__<tool>` 适配删除；与 PI 的差异按 W0 既有机制在 `PI-PARITY.md` 登记为
   **有意偏离**（理由：恒定工具面是 DI 注册与可替换性的前提）。
3. **agent-team Non-Goal 口径**：保持在该 change 范围内生效，不扩散；harness-v2 要覆盖时
   以自身条目/新 change 记录（如「成员输出实时投影」仍属独立 change）。
4. **run 快照归属**：RUNTIME-PLAN P6 先落 agent/team/prompt 快照字段；harness-v2 W2-6 记
   「部分承接」，剩余 = 全量 `configHash` 冻结，留在 W2 波完成。
5. **layerfs 路线**：**Phase A = Go 原生 CAS/tree**（复用现有 blob store，补 tree manifest/
   refcount/capture），与 file-plugin、bash-plugin 同波交付；Rust+gRPC 保留为**同接口扩展实现**
   （spec 保留 gRPC 边界），等规模需要时再引入，不现在上第二运行时。

## 5.x 复核 P2 backlog（2026-11-20，Wave 2 最终 A/B）

- **kernel `fs.capture` 跨 lane dedupe**：`Lookup` 无 lane 参数，加 `sys:` 前缀属查询契约变更；当前靠
  调用方键纪律（`job:` 前缀），仅本会话自伤的恶意 toolCallId 碰撞面，无跨租户/提权。
- **notify 多实例窗口**：`ClaimPush` 后另一实例 ack 仍会推一条已消费通知（URL 已失效）；多实例部署前
  在 store 层复查 status。
- **`mcp.call` arguments 契约归属**：object 强制只在 toolsurface seam；`internal/mcp` 直连/externalgw
  未来调用方需自带同等防线（接口注释声明）。
- **通知表租户配额**；模型侧 `ack` 语义（tool-surface spec 明列 ack，注意力层非授权门，产品决策待定）。
- **`Materialize` `O_NOFOLLOW`**（fork/materialize 生产接线前必补）；**`shellScope` 无 run scope
  回落 boot 租户**（多租户 pack 前改类型化拒绝）。
- **residue 脚本**补扫 `keel_agent_test_*` 遗留库（B P2-4）。
- **MCP Apps CSP 公共后缀判定**（P2 复核残项，2026-11-20）：`internal/mcphost` 已拒绝单标签
  通配（`*.com`/`*.net`/`*.localhost`）与 loopback/私网/全数字主机；但完整 PSL 判定未做，
  `https://*.co.uk` 这类「两级以上仍是公共后缀」的通配仍会被当作子域授予。当前成本高于收益
  （需要内嵌 PSL 或网络依赖），登记待 P6 之后评估；其它替代 IP 编码（十六进制/八进制主机）
  同理，仅全数字形式被拒。

## 6. 今日已到账（证据锚点）

- 三次推送：`9d023c1`（Agents/团队产品面 + 三插件 + 团队启动模式）、`59790cf`（插件/模型尾项 + 会话软删除）、`8783372`（demo/trace 收尾 + 运行注入方案）
- 门禁：Go 全量绿 · `nsh check` · TUI 106/106
- 真库 E2E：agents/teams CLI 全链（403/409 权限与引用保护）· 会话删除 UI 全路径（37→36）· 直接配置凭据（DIRECT-CONFIG-OK）· 六插件能力树
- 审计报告（公网）：`https://local-gaubee-keel-harness.fde2e.com/report.html`
- 正在跑：运行注入实现（线 A，P1→P6）
- **MCP Apps 双 origin 沙箱域开通**（2026-11-14）：`https://mcp-app.fde2e.com/` +
  `https://<app>.mcp-app.fde2e.com`（证书/路由/frpc/浏览器渲染全链验收；证据
  `openspec/changes/archive/2026-11-20-feat-mcp-apps-events/evidence/domain-provision.md`）
- **2026-11-19**：4 个 change 归档（`1daa977`）；复核修复 `09399f1` + 修复后 E2E 证据
  `2b1c1e7` 已推送；**W2.1 工具面骨架开工**（slow-agent 后台，11 恒定工具 / action 闭集 /
  清单哈希 / 旧名快照断言 / `mcp__` 方言删除 / PI-PARITY 偏离登记）
