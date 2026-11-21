# Tasks: keel-agent-team-v1

> 先冻结 Agent / AgentTeam / 插件能力树契约，再实现插件与 UI。UI 先交付部件
> （Agent 卡/编辑器、工具选择器、团队画布、启动模式、能力树），再组合设置页与会话启动。
>
> **状态（2026-11-14）**：P0 冻结、P2/P3 的产品面（HTTP/CLI/UI/插件实体）、P4 的两种启动模式
> 已交付并有线上 E2E 证据（见文末「交付记录」）；P1 的写命令、P4 的 run 快照/内核注入、
> P5 的剩余打磨项为未完成项（未勾选即未完成——不得在报告里称"已就绪"）。
>
> **状态（2026-11-20）**：P2/P3 的 CLI 尾项（`agent start|stop`、`team save|start`）已交付，
> 真库 E2E 证据 `evidence/cli-tail-2026-11-20/`；Team 删除的会话引用保护仍阻塞（见 P3）。

## P0 契约与设计冻结

- [x] 冻结 `user-requirements.md` 原话记录与裁决映射（模型预设 → AgentTeam；语音 TTS/STT；
      Agent 名字/头像/默认模型/工具/AGENTS.md；团队队长必选；CLI 能力树）
- [x] 冻结 Agent schema：id/name/description/avatar/defaultModel/tools/systemPrompt/
      skills/permissions/teamCli（`internal/agentteam`）
- [x] 冻结 AgentTeam schema：captain（必选、唯一）、members（0..N）、team 配置文件
      （DB-backed）、teamCli 能力开关、session 快照
- [x] 冻结插件能力树 schema：path/summary/description/args_schema/output_schema/
      permissions/danger/idempotent/callable_by/examples（`internal/pluginpkg/cli.go`）
- [x] 冻结会话启动模式：standard（选模型）| team（选团队）
- [x] 冻结工具注册源：toolId/来源插件/schema/权限/danger/callable_by
- [x] 冻结 team 配置是 DB-backed 文件、CLI 修改 team 配置、不污染 model/agent provider
- [x] **Owner 1A：旧模型预设直接删除，不做迁移/兼容层**；删除旧表/API/UI 与单模型下拉的旧配置路径
      （旧单列 UI 已被「当前默认卡 + provider 卡片」两级视图取代，见 before/after 截图 B1）
- [ ] 部件级设计：Agent 卡/编辑器、头像、工具选择器、团队画布、成员卡、Team CLI 面板、
      会话启动模式、插件能力树先过视觉验收（设计稿已落 `design/`，Owner 视觉验收未做）

## P1 模型插件（model-plugin）

- [x] `provider_types` / provider 实例 / 模型目录 / 参数 schema / 健康检查（沿用原设计）
- [ ] CLI：`keel model provider list|show|add|update|remove|test`、`keel model catalog`
      （已交付：只读子集 `keel model providers|list|show` + **写子集 `keel model provider add|update|remove`
      （整文档读-改-写，PUT /v1/settings/models，revision 守卫）与只读探测 `keel model provider test`**；
      未做：`model provider list|show`（读面已在 `model providers`/`model show`）、`model catalog`）
- [x] CLI 与 UI 共用 schema/租户边界；凭据只接受引用
- [x] Provider 删除保护：被 Agent/Team 引用时拒绝并列出引用方
      （写路径 409 provider_in_use + references（agent.defaultModel / team.defaults.modelOverride /
      inline member defaultModel）；真库实证；未被引用时按文档条目移除，"禁用"（visibility hidden）仍可行）
- [x] 能力树声明 model CLI 子命令（声明 == 已实现子集，双向由测试锁死）

## P2 Agent 插件（agent-plugin）

- [x] Agent 表与 API：`/v1/agents`；租户隔离、归档、引用保护
- [x] 默认模型绑定：只是默认，可被会话/任务覆盖
- [x] 工具能力：从工具注册源选择；`agent_tools` 保存 toolId+开关+配置
- [x] 系统提示词：inline / AGENTS.md 文件引用进入 run：`internal/agent/binding.go`（per-attempt 解析）+ `internal/host/host.go`（`RenderSystemPrompt` 保留动态段）+ `internal/servicehost/binding.go`（文件引用按部署根解析、越界拒绝、读取失败降级）；证据 `go test ./internal/agent/ ./internal/host/ ./internal/servicehost/` + `evidence/runtime-20261114-211425/wire-requests.jsonl`（captain 请求的 system 以 agent 文本开头，插件段 `<plugin_standard>` 仍在）
- [x] 头像：确定性 identicon（seed）、上传/URL 覆盖、失败回退
- [x] 名字/描述/标签可编辑；默认值只是建议
- [x] CLI：`keel agent list|show|create|update|delete|start|stop`（start/stop：`cmd/keel/agentteam_runtime.go`；
      start = session + 带 `selection.agentId` 的 input，stop = 按最新 selection/captain 扫活会话后 cancel；
      证据 `cmd/keel/agentteam_test.go` 运行时用例 + `evidence/cli-tail-2026-11-20/agent-start.txt`/`agent-stop-live.txt`）
- [x] 能力树声明 agent CLI；删除/归档引用保护
- [x] Agent 库 UI 组件与编辑器

## P3 AgentTeam 插件（agent-team-plugin）

- [x] Team 表/配置文件：captain 必选唯一（实现为单一 `agent_teams` 文档——成员随文档，
      不建 `agent_team_members` 表，避免文档与行两套真值；见 store 注释）
- [x] Team 配置是 DB-backed 文件；revision/hash；执行绑定在 attempt 启动时冻结（实现为 per-attempt 冻结：`internal/servicehost/binding.go` 每次 attempt 读文档，`RunSummary` 记录 agentId/teamId/teamRevision/teamHash/promptHash；证据 `evidence/runtime-20261114-211425/captain-summary.json`：teamRevision=1、teamHash=058a0c57…、promptHash=sha256:2db85c81…）
- [x] 成员 0..N；成员是 Agent 引用/inline 定义；成员 label/description 可编辑
- [x] Team CLI：`keel team list|show|create|update|delete|member add|remove|save|start`（save/start：
      `cmd/keel/agentteam_runtime.go`；save = YAML/JSON 文档（必须带 id）POST/PUT 并剥离服务器字段，
      start = session + 带 `selection.teamId` 的 input；证据 `evidence/cli-tail-2026-11-20/team-save-*.txt`、
      `team-show-export.json`、`team-start.txt`、`team-stop-live.txt`）
      > 遗留（2026-11-20）：`internal/agentteam/cli_specs.go` 另声明 `team member list|save`（本 task 行未列），
      > 当前 CLI 未实现；属既有能力树与实现差异，未在本轮尾项内扩散。
- [x] Team CLI 能力开关：关闭后拒绝并返回 typed permission error（线上 403 team_cli_disabled 实证）
- [x] 队长自组装：成员增删只改 team 配置，不污染 model/agent provider
- [ ] Team 删除/归档引用保护（未做：需要候选 B 的 sessions 绑定列，见 RUNTIME-PLAN §b）；运行中修改不影响当前 run [x]（per-attempt 解析：编辑只影响下一次 attempt，`internal/agent/runner.go` 的 Bindings seam）
      > **阻塞（2026-11-20 核查）**：`sessions` 无 agent/team 绑定列（`internal/store/schema.sql:19-27`，
      > 只有外部会话 6 列 `:34-40`），绑定仅存在于 `user.message.payload.selection`；删除保护依赖
      > `keel-harness-v2` W2-1 的 `groups`/`session_placement`（未实现：`openspec/changes/keel-harness-v2/RECONCILIATION.md:158`）
      > 或候选 B 的独立 change。本轮不抢建；Agent 侧被 team 引用的 409 保护已在 E2E 第 6 段实证，
      > Agent/Team 的**会话**引用保护同此阻塞。
- [x] 能力树声明 team CLI 子命令与 callable_by
- [x] Team 卡片/成员管理 UI（settings › 团队）

## P4 会话启动与内核注入

- [x] 会话 `mode` = standard|team；standard 保存 model 绑定，team 保存 teamId
      （selection 随输入发送；teamId 为会话级内存标记，未持久化——见 2026-11-14 记录）
- [x] Standard：选模型/默认 Agent 启动；不要求团队
- [x] Team：选有效团队启动；captain 为主 Agent；缺 captain 阻止启动（按钮禁用+原因）
- [x] run manifest 冻结 agent/team/prompt hash：`run.finished` 摘要携带 agentId/teamId/teamRevision/teamHash/promptHash（`internal/agent/runner.go`，形状由 `internal/kernel/validate.go` 校验）；证据 `evidence/runtime-20261114-211425/captain-events.json`
- [x] 运行中切换模式/团队只影响下一次 run：绑定随输入事实（selection）携带、resolver 每 attempt 各解析一次；`internal/agent/binding_test.go`（resolver 每次 attempt 调用）+ E2E 成员 run 用成员自己的提示词
- [x] 成员复用既有 Agent/run 机制：`agentext.SpawnRequest.AgentID` → 子输入 `selection.agentId`，子 run 走同一 runner/装配（`internal/agentext/subagent.go`）；证据 `evidence/runtime-20261114-211425/member-events.json`（子输入 selection.agentId=agent_kite_member，子 run 的 wire system 是 KITE-RUNTIME-MARK-8）
- [x] 会话 header：Standard 显示模型；Team 显示团队名+captain+成员入口（团队名已显示）
      （`web/src/screens/session-team.tsx`：标题第二行 Standard=当前模型 / Team=团队名+队长+模型；
      宽屏 chip 显示完整团队名，单列 chip 收为 icon/短文字；成员入口打开只读名单，
      含加载/空/错误/403 四态。证据 `evidence/ui-tail-2026-11-21/`：浏览器 390/1280 截图 +
      `session-team.test.tsx`、`App.tui.test.tsx` 的 390/宽屏用例）

## P5 设置与 UI

- [x] 完整设置页：模型 / Agents / AgentTeam / 工具 / 能力 / 客户端 / 插件 / 高级
- [x] Agent 卡片、Agent 编辑器、头像生成、工具注册源选择器、AGENTS.md 编辑器
      （AGENTS.md 文件引用编辑已补：`settings-agents.tsx` 用「内联文本 / 文件引用」两个显式来源页签，
      写既有 `systemPrompt: {kind:"file", ref}` 字段；本地校验空值/绝对路径/越界，服务端拒绝原样显示；
      编辑回填、保存后卡片事实同步。证据 `settings-agents.test.tsx` 与
      `evidence/ui-tail-2026-11-21/screenshots/07-agent-editor-file-390.png`、
      `08-agent-editor-file-error-390.png`、`13-agent-editor-file-1280.png`）
- [ ] 团队卡片、团队画布、captain 槽、成员库/新建 Agent、Team CLI 能力面板
- [x] 会话启动模式选择（Standard/Team）、团队选择器、缺 captain 阻塞态
- [x] 插件能力树 UI（命令/子命令/描述/权限/danger/callable_by/复制）
      （权限列表与复制动作已补：plugins.tsx 的 CliCapabilityRow 渲染 manifest 的 permissions 原样，
      复制走 Clipboard.writeText 并把平台结果回显在行内；TUI 断言含真剪贴板回读）
- [x] 五态（空/加载/错误/权限不足/成功）、390px 单列、键盘可达
      （Agents、团队、团队名单逐页覆盖空/加载/错误/403/成功，写路径另测保存中/保存失败；
      390px 由真实 Chrome 在 fixture 构建上量 `scrollWidth == innerWidth`，见
      `evidence/ui-tail-2026-11-21/checks.json`；键盘沿用 kit Dialog/Tabs/Input/Button 的
      既有组件用例 `src/ui/kit/components/overlay.test.tsx`、`form.test.tsx`）
- [x] shadcn 官方组件 + solid-glass；禁止手写仿制
- [x] 组件画廊先行；组合页面后验收

## P6 Provider 适配器

> 2026-11-21 M1：四 family 适配器库落在 `internal/mediaadapter`，证据
> `evidence/media-adapters-2026-11-21/`。产品装配（插件注册、凭据解析、设置页保存预检接线）
> 不在本 lane，尚未接线；未调用真实上游，wire 由 fake/记录型 provider 验证。
>
> 2026-11-21 M2：装配已接。`internal/mediawiring` 从 generation 的租户 models 文档与同一凭据
> 顺序构造 adapter；`servicehost.ProductOptions.MediaTools` 把 image/video 工具注册进每次
> host 装配（设置写后跟随新 generation）；`ValidateMedia` 在 `PUT /v1/settings/models`
> 保存前跑 decision Preflight。证据 `evidence/media-wiring-2026-11-21/`。仍未调用真实厂商，
> 也未写 media 事件；speech 与 decision 的运行时消费面仍未接。

- [x] LLM 适配器与现有模型配置迁移
- [x] 图像/视频适配器：Hermes `image_generate`/`video_generate` 形态；产物入 layerfs
      （`image_generate`/`video_generate` 工具 schema + OpenAI-images/视频网关 HTTP wire；
      资产字节写 layerfs CAS 并返回 `sha256:` 引用，结果/工具输出不内联 base64；
      usage 只计 Images/Videos/Bytes/InputTokens；失败结果保留 prompt/参数可重试。
      M2 接线：租户 image/video provider → `internal/mediawiring` → `servicehost` 工具面；
      `agentcore.ToolRunScope` 的 session 作为 layerfs 归属；记录型 provider E2E 断言
      wire 的 Authorization/body、blobId 引用、usage，以及缺凭据时 typed
      `missing_credentials` 且零 wire 请求；设置写后新 generation 的工具改用新 provider）
- [ ] 语音适配器：TTS 文本→音频；STT 音频→transcript/时间戳；音色/语言参数
      （库与 wire 已完成：`SpeechAdapter` + `/audio/speech`、`/audio/transcriptions`；
      **M2 未接线**：没有任何 host tool、`/v1/media/audio` 或 run 消费面，运行时尚不可用；
      人类 UI 的 demo 路由仍归 `internal/httpapi/media_audio.go`）
- [ ] 决策适配器：System One 0.2.0；TypeSafe/Cloudflare Clef/OpenRouter/OpenJev；
      noul/choice/score（box 仅 TypeSafe）、能力预检、shadow/enforce、fail-closed 默认
      （**M2 部分接线**：库 + descriptor/`Preflight` 已完成；`PUT /v1/settings/models` 在保存
      含 decision provider 的文档时跑 adapter/model/协议与 binding Preflight，字段错误可见；
      **运行时缺口**：没有 decision 工具或端点消费答案，run 不能发起决策；仍未调用真实厂商；
      不合成 noul:0/choice:"unknown"/confidence:0）
- [ ] Provider 崩溃隔离；usage 计量不写价格
      （`guard` 与 `Usage` 已由 M2 工具路径复用：in-process panic → retryable
      `provider_unavailable`，Usage 无价格字段且丢弃上游 cost；**缺口**：跨进程/provider
      插件隔离仍由 provider 插件层承担，media 事件未写，usage 目前只在工具结果里）

## P7 验收

- [x] 创建 Agent：名字/描述/头像/默认模型/工具/AGENTS.md；identicon 同 seed 稳定
- [x] 创建团队：只配 captain 可启动；captain 必选唯一；缺 captain 阻止启动
- [x] 队长 CLI 开启时可 `keel team member add`；关闭后命令消失并返回权限错误
- [x] 队长增删成员只改 team 配置；model/agent provider 配置无变化
- [ ] Standard 选模型启动；Team 选团队启动；运行中切换只影响下一次 run（前两半已验收）
- [x] 工具列表来自注册源；引用已不可用的 toolId 只剔除并留诊断、不阻断 run（`internal/agent/runner.go` toolsForRun + `internal/agenttool/registry.go` Subset；UI/CLI 面的卸载同步仍未验）
- [x] 旧模型预设已删除，旧 preset 不能启动新 run；旧会话历史仍可读
- [x] 凭据零泄漏（API/日志/事件/CLI 输出/UI DOM/team 配置）
- [ ] 组件画廊与组合页面视觉复核通过；390px 无溢出（390px 已验，组合页视觉复核未做）

---

## 交付记录（2026-11-14）

**本轮交付**（commit 见主仓指针）：

1. **Agent/Team 持久化 + HTTP + CLI**：`internal/store/agents.go`（`agents` / `agent_teams`
   文档表 + 不兼容旧表的启动期对账迁移）、`internal/httpapi/agents.go`（10 条路由，
   队长权限头 `X-Keel-Actor-Agent`，409 引用保护）、`cmd/keel/agentteam.go`（`keel agent` /
   `keel team`，口令经 env 不落 flag、不打印）。
2. **三个插件实体**：内置 `keel.model`（3 条 CLI 能力）/ `keel.agent`（5 条）/ `keel.team`
   （7 条）manifest（`internal/pluginpkg/builtin/keel.*.yaml`）+ 空核 `agentext` 插件工厂；
   `GET /v1/plugins` 原样携带 `provides.cli`；插件页渲染能力树（危险级/幂等/可调用方徽标）。
   声明 ↔ 实现双向由测试锁死（`TestCapabilityPluginsDeclareTheirCLI` + `cmd/keel/model_test.go`）。
3. **`keel model` 只读 CLI**：`model providers|list|show`（与设置页同一解析投影）。
4. **团队会话启动模式**：选择器「标准模式 | 团队模式」；选团队 = 队长默认模型成为会话
   selection + `store.sessionTeam` 标记；header chip 显示「团队 · <名>」；无可用队长模型
   的团队置灰并给原因；手动改选模型退出团队模式。
5. **缺陷修复（本轮实证）**：插件 `provides` 按对象渲染（原 `.join` 会是 toString 崩溃）；
   插件状态词表对齐服务端（enabled|installed|disabled|failed|invalid|uninstalled——修复前
   全部显示"未知状态"）。

**线上证据**（13760 真库 + 真 CLI，输出逐字）：

```
$ keel agent create --name 评审队长 --provider s2api --model deepseek-v4.1-flash --team-cli --team-cli-commands member.add
created agent agent_39d0778767ad45eca4998a9cbea87d9a (评审队长)
$ keel team create --name 评审小组 --captain agent_39d0…
created team team_5a7e2f741b884f80862dff945eee8a80 (评审小组) captain=agent_39d0… revision=1
$ keel team member add team_5a7e… agent_06d8… --label 初稿评审
added agent agent_06d8… on team team_5a7e… (members=1 revision=2)
$ keel team member remove team_5a7e… agent_06d8… --as agent_39d0…
keel: server refused (403 team_cli_disabled) … member.remove is not available to the captain (disabled)
$ keel agent delete agent_06d8…        # 已入队成员
keel: server refused (409 agent_in_use) … referencing team: team_5a7e… 评审小组
$ keel model providers                 # 8 个 provider；model list 10 个模型；show 输出完整 JSON
```

`GET /v1/plugins` 六个插件（`keel.agent` 5 / `keel.model` 3 / `keel.team` 7 条 cli 能力）已实测。

**截图**（同目录）：`01-settings-agents.png`、`02-settings-teams.png`、`03-settings-plugins.png`、
`04-plugin-team-cli-tree.png`、`05-picker-team-mode.png`、`06-team-session-chip.png`；
before 对照：`B1-before-model-settings.png`、`B2-before-plugins.png`、`B3-before-picker.png`。

**明确未完成**（不得宣称完成）：P1 写命令、P2 start/stop 与 prompt 注入 run、P3 save/start 与
session 快照、P4 run manifest 冻结与成员运行拓扑、P5 能力树权限列表/复制、P6 图/视/音/决策适配器。
