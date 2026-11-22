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
>
> **状态（2026-11-21）**：C2 团队画布 lane 收口——团队卡片、captain 槽、成员库/新建 Agent、
> Team CLI 能力面板已完成（`settings-teams.test.tsx` 25 例 + `evidence/team-canvas-2026-11-21/`
> 浏览器 390/1280/缺队长 9 张、8 视口 pass、manifest 9/9）；画布仍缺 design A6 的「默认工具/模型」
> 与成员 label/description 编辑，故 P5 该行不勾（画布按 partial 计）。
>
> **状态（2026-11-21 · C3）**：B 轴复核的 P1 数据丢失已修——PUT 按 agentId/inline id 合并
> `editing.members`，续写 CLI 写的 label/description/inline；空 agentId + inline 席位不再落成
> `{agentId:""}`。P2 同修：Team CLI 面板 403 用专用权限面。`settings-teams.test.tsx` 27 例、
> 全量 TUI 220 例全绿（`evidence/team-canvas-2026-11-21/gates/`）。
>
> **状态（2026-11-21 · AC1/P7 验收）**：启动选择三段 E2E 通过（Standard 选模型、Team-only 选团队
> → captain 默认模型、运行中队列切换只影响下一次 run；`evidence/start-selection-20261121-105043/`），
> Team/member 运行时 E2E 在当前 schema 复跑通过（`evidence/runtime-p7-2026-11-21/`）。视觉复核 62 张
> 真实 Chrome 截图（390/1280）零溢出，定向 TUI 69/69；报告 `evidence/p7-acceptance-2026-11-21/report.html`。
> P0/P1 为 0；P2 三项：画廊 `Progress` 缺件（manifest 注明等裁决）、390 缺队长卡片拥挤、fixture 无
> team-tasks 回退。未做项：P3 团队会话引用保护、P6 speech/decision 运行时；P5 画布 defaults/成员
> label 编辑已由 C4 补齐（见下一条状态）。
>
> **状态（2026-11-21 · C4）**：团队画布补齐 design A6 的团队级默认工具/模型覆盖
> （`defaults.tools` + `defaults.modelOverride`）与成员席位 label/description 编辑、成员替换
> （保留席位文本；inline 席位同样可编辑）。保存继续走既有 team PUT 契约并合并未编辑字段，
> `defaults` 总是整对象发送，未编辑字段不会被 PUT 静默清空（清空默认走同一路径，未单列门禁用例）。
> `settings-teams.test.tsx` 32 例、
> 全量 TUI 225 例、check/build/package 全过；真实 Chrome 390/1280 截图与测量
> `evidence/canvas-defaults-2026-11-21/`。P5 画布行据此勾选。

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
- [x] CLI：`keel model provider list|show|add|update|remove|test`、`keel model catalog`
      （已交付：读面 `provider list|show`（+ `model providers|list|show` 别名）、写子集
      `provider add|update|remove`（整文档读-改-写，PUT /v1/settings/models，revision 守卫）、
      真实探测 `provider test`（POST /v1/providers/{id}/test，服务端解析凭据并发一次最小
      model-list 请求；无凭据 needs_auth、失败 unreachable，不伪造成功）、`catalog`
      （解析目录 + `--refresh` 上游实取）；证据 `evidence/model-cli-2026-11-21/`）
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
- [x] CLI：`keel agent list|show|create|update|delete|archive|start|stop`（start/stop：`cmd/keel/agentteam_runtime.go`；
      start = session + 带 `selection.agentId` 的 input，stop = 按最新 selection/captain 扫活会话后 cancel；
      archive = `cmd/keel/agentteam.go` 的 `runAgentArchive`；
      证据 `cmd/keel/agentteam_test.go` 运行时用例 + `evidence/cli-tail-2026-11-20/agent-start.txt`/`agent-stop-live.txt`）
- [x] 能力树声明 agent CLI；删除/归档引用保护
- [x] Agent 库 UI 组件与编辑器

## P3 AgentTeam 插件（agent-team-plugin）

- [x] Team 表/配置文件：captain 必选唯一（实现为单一 `agent_teams` 文档——成员随文档，
      不建 `agent_team_members` 表，避免文档与行两套真值；见 store 注释）
- [x] Team 配置是 DB-backed 文件；revision/hash；执行绑定在 attempt 启动时冻结（实现为 per-attempt 冻结：`internal/servicehost/binding.go` 每次 attempt 读文档，`RunSummary` 记录 agentId/teamId/teamRevision/teamHash/promptHash；证据 `evidence/runtime-20261114-211425/captain-summary.json`：teamRevision=1、teamHash=058a0c57…、promptHash=sha256:2db85c81…）
- [x] 成员 0..N；成员是 Agent 引用/inline 定义；成员 label/description 可编辑
- [x] Team CLI：`keel team list|show|create|update|delete|archive|member add|remove|save|start`（save/start：
      `cmd/keel/agentteam_runtime.go`；save = YAML/JSON 文档（必须带 id）POST/PUT 并剥离服务器字段，
      archive = `cmd/keel/agentteam.go` 的 `runTeamArchive`；
      start = session + 带 `selection.teamId` 的 input；证据 `evidence/cli-tail-2026-11-20/team-save-*.txt`、
      `team-show-export.json`、`team-start.txt`、`team-stop-live.txt`）
      > 遗留裁决（2026-11-21）：`team member list|save` 未实现，已从 cli_specs 能力树、
      > `TeamCLI` 词表、spec/manifest 一并移除，不保留假声明；如确实需要该能力，另立 change。
- [x] Team CLI 能力开关：关闭后拒绝并返回 typed permission error（线上 403 team_cli_disabled 实证）
- [x] 队长自组装：成员增删只改 team 配置，不污染 model/agent provider
- [x] Team 删除/归档引用保护（2026-11-21 交付；运行中修改不影响当前 run [x] 见下）
      > **实现**：`sessions` 增加派生列 `bound_agent_id`/`bound_team_id`/`bound_seq`/`bound_at`
      > （`internal/store/schema.sql` 会话段；RLS 随 sessions 既有策略，不新增表）。提交路径
      > `POST /input`、`/retry`、兼容边 `POST /events` 与 agentext `TurnStarter`
      > （trigger/spawn/manual fire）统一调用 `internal/sessionbinding.Record`，按
      > `bound_seq <= 事件 seq` 条件写，并发下最新 input 胜；`selection` 仍只在
      > `user.message` 账本，该列只是派生索引，不参与 run 装配。模型-only selection 清空绑定，
      > 无 selection 的输入不改绑定。子代理 spawn 的子会话因此同样参与引用保护。
      > 删除/归档守卫在 `internal/httpapi/agents.go`：Team 用 `SessionsReferencingTeam` →
      > 409 `team_in_use`；Agent 合并 `TeamsReferencingAgent` + `SessionsReferencingAgent` →
      > 409 `agent_in_use`；`references` 中会话项带 `kind:"session"`/state/runStatus/archived，
      > 会话 references 有界（最多 20 条 + `total`/`truncated`），CLI 渲染 `... and N more`。
      > 新增 `POST /v1/agents/{id}/archive`、`POST /v1/teams/{id}/archive` 与
      > `keel agent|team archive`；YAML manifest 与 cli_specs 均声明 archive。
      > Agent 归档采用 lifecycle CAS（`UpdateAgent(expectedArchived)` + 归档位只读），
      > 并发 PUT 不会复活已归档 Agent，过期写返回 409 `agent_changed`。
      > **语义**：未归档会话无论 run 状态都算引用；已归档会话不算引用，但若仍持有
      > pending/running run 仍算（归档不取消 run）；解绑 = 归档会话或发送只选模型的输入。
      > 证据 `evidence/session-ref-2026-11-21/`。
- [x] 运行中修改不影响当前 run（per-attempt 解析：编辑只影响下一次 attempt，`internal/agent/runner.go` 的 Bindings seam）
- [x] 能力树声明 team CLI 子命令与 callable_by
- [x] Team 卡片/成员管理 UI（settings › 团队）

## P4 会话启动与内核注入

- [x] 会话 `mode` = standard|team；standard 保存 model 绑定，team 保存 teamId
      （selection 随输入发送；2026-11-21 起 `sessions.bound_agent_id/bound_team_id` 持久化
      最新 selection 的派生绑定，供引用保护读取；执行仍按账本 per-attempt 解析）
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
- [x] 团队卡片、团队画布、captain 槽、成员库/新建 Agent、Team CLI 能力面板
      （C4 补齐画布剩余编辑面：团队级 `defaults.tools`/`defaults.modelOverride`、成员席位
      label/description（inline 席位同样）、替换成员保留席位文本；证据
      `settings-teams.test.tsx` 与 `evidence/canvas-defaults-2026-11-21/`）
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
> 保存前跑 decision Preflight。证据 `evidence/media-wiring-2026-11-21/`。仍未调用真实厂商。
>
> 2026-11-21 M3（S2）：speech 与 decision 的运行时消费面已接。`speech_tts`/`speech_stt`
> 由租户 tts/stt provider 构造，TTS 音频入 layerfs，STT 读 layerfs 引用并回
> transcript/segments；`decision_ask` 由租户 decision provider 构造，按保存 binding 的
> enforce/shadow + failPolicy 执行，缺凭据 typed `missing_credentials`。媒体结果以既有
> `tool.result.details.media` 携带 layerfs 引用与客观 usage（零价格），未扩内核 kind；
> 工具级（含 adapter seam）panic → retryable `provider_unavailable`。证据
> `evidence/speech-decision-2026-11-21/`。仍未调用真实厂商，跨进程 provider 插件隔离
> 仍归插件层。

- [x] LLM 适配器与现有模型配置迁移
- [x] 图像/视频适配器：Hermes 静态参数（image: `prompt`/`aspect_ratio`；video: `prompt` 等）+ keel 扩展参数
      （`negative_prompt`/`size`/`n`/`reference_image_urls` 等）；产物入 layerfs
      （`image_generate`/`video_generate` 工具 schema + OpenAI-images/视频网关 HTTP wire；
      资产字节写 layerfs CAS 并返回 `sha256:` 引用，结果/工具输出不内联 base64；
      usage 只计 Images/Videos/Bytes/InputTokens；失败结果保留 prompt/参数可重试。
      M2 接线：租户 image/video provider → `internal/mediawiring` → `servicehost` 工具面；
      `agentcore.ToolRunScope` 的 session 作为 layerfs 归属；记录型 provider E2E 断言
      wire 的 Authorization/body、blobId 引用、usage，以及缺凭据时 typed
      `missing_credentials` 且零 wire 请求；设置写后新 generation 的工具改用新 provider；
      provider 返回的资产 URL 默认拒绝非公网地址（含每跳重定向，防 SSRF），
      设置写的 decision binding Preflight 拒绝全树 http(s) URL）
- [x] 语音适配器：TTS 文本→音频；STT 音频→transcript/时间戳；音色/语言参数
      （M3：`speech_tts` 从租户 tts provider 构造，文本→音频写 layerfs 并只回
      `blobId`/mime/size/duration + 字符/字节/毫秒 usage；`speech_stt` 读 layerfs
      `blob_id`、按 stt provider 走 multipart wire，回 transcript/segments 与输入音频引用；
      voice/language/format 为显式参数；缺凭据 typed `missing_credentials` 且零 wire 请求；
      工具级 panic 隔离；真实 run + 记录型 HTTP fixture 覆盖 TTS/STT。
      人类 UI 的 demo 路由仍归 `internal/httpapi/media_audio.go`，不是租户运行时）
- [x] 决策适配器：System One 0.2.0；TypeSafe/Cloudflare Clef/OpenRouter/OpenJev；
      noul/choice/score（box 仅 TypeSafe）、能力预检、shadow/enforce、fail-closed 默认
      （M3：`decision_ask` 工具消费租户 decision provider；保存 binding 的 state/questions
      可作默认输入，策略只取 binding 的 mode/failPolicy，模型不能改；enforce 失败回
      typed `provider_unavailable` 且无答案，shadow 失败回 `shadow:true`/failure/无答案；
      不合成 noul:0/choice:"unknown"/confidence:0；缺适配器/凭据明确失败。
      真实 run + 记录型 HTTP fixture 覆盖成功/enforce 失败/shadow 失败；仍未调用真实厂商）
- [x] Provider 崩溃隔离；usage 计量不写价格
      （M3：媒体工具 boundary 双层隔离——adapter `guard` 包 provider 调用，工具
      `recoverToolPanic` 包 adapter/seam（含 Store/Executor panic）；均回 retryable
      `provider_unavailable`，不拖垮同 run 的其它工具。`Usage` 无 price/cost/currency 字段，
      上游 cost 在 adapter 边缘丢弃；媒体结果用既有 `tool.result.details.media` 记录
      layerfs 引用与 usage，未新增内核 media kind。跨进程 provider 插件隔离仍由插件层承担，
      不在本 lane）

## P7 验收

- [x] 创建 Agent：名字/描述/头像/默认模型/工具/AGENTS.md；identicon 同 seed 稳定
- [x] 创建团队：只配 captain 可启动；captain 必选唯一；缺 captain 阻止启动
- [x] 队长 CLI 开启时可 `keel team member add`；关闭后命令消失并返回权限错误
- [x] 队长增删成员只改 team 配置；model/agent provider 配置无变化
- [x] Standard 选模型启动；Team 选团队启动；运行中切换只影响下一次 run
      （三段 E2E `evidence/start-selection-20261121-105043/`：Standard `kite-standard-b`；
      Team-only `{teamId}` → captain 默认模型 + revision/hash；switch 三段 run.finished 依次
      standard→team→standard，run 2 的 inputSeqs=[1,2] 不含 run 2 在飞时排队的第三条 input（seq5），该输入由 run 3 以 standard 消费）
- [x] 工具列表来自注册源；引用已不可用的 toolId 只剔除并留诊断、不阻断 run（`internal/agent/runner.go` toolsForRun + `internal/agenttool/registry.go` Subset；UI/CLI 面的卸载同步仍未验）
- [x] 旧模型预设已删除，旧 preset 不能启动新 run；旧会话历史仍可读
- [x] 凭据零泄漏（API/日志/事件/CLI 输出/UI DOM/team 配置）
- [x] 组件画廊与组合页面视觉复核通过；390px 无溢出
      （62 张真实 Chrome 截图 390/1280，document scrollWidth == innerWidth，0 越界元素；
      定向 TUI 69/69；报告 `evidence/p7-acceptance-2026-11-21/report.html`。画廊 42 件中 41 件渲染；
      `Progress` 是 manifest 自注「proposal 列了、tasks C1–C5 没列、等裁决」的缺件、无产品路径
      import，按 P2 记；另记 390 缺队长卡片拥挤与 fixture team-tasks 回退两项 P2，均无 P0/P1）

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
