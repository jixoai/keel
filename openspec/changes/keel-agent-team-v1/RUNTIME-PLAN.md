# Runtime Plan: 会话选择 → run 装配（agent / team 生效）

> 本文只做**勘察与方案**，不含产品代码改动。所有证据取自
> `9d023c1`（worktree `keel-task-harness-v2`）+ 真库 13760 实测（2026-11-14）。
>
> 核心断层一句话：**Agent/Team 配置只到 HTTP/UI 层，`run` 装配完全不读它**——
> `selection` 里塞不进 `teamId`/`agentId`（400 unknown key），即使塞得进
> `servicehost` 也只用它解析 model，Agent 的 `systemPrompt` / `tools[]` /
> 团队 captain 关系在 `newPromptFactory` 与 `newToolsFactory` 里没有任何读者。

---

## a. 现状装配链路（file:line 证据）

### a.1 从 claim 到执行：一条 run 的骨架

| 步骤 | 证据 | 说明 |
|---|---|---|
| claim 循环 | `internal/agent/worker.go:192`（`w.Leases.Claim`） | 一个 session 一个 run row，`FOR UPDATE SKIP LOCKED` |
| 服务图 pin | `internal/agent/worker.go:245-306`（`w.Binding.Acquire` → `bound.Runner()`） | run 钉住一个 loadout generation；reload 不影响在飞 run |
| 取 runner | `internal/agent/worker.go:350`（`runner.RunAttempt(runCtx, lease)`） | 之后的 prompt/tools 全部来自这个 runner |
| 载入日志 | `internal/agent/runner.go:917`（`loadEvents`，分页 500） | 全量 committed events |
| 解析 model | `internal/agent/runner.go:327` → `internal/agent/model.go:87`（`activeModel`） | **这是唯一读 selection 的地方** |
| 取工具表 | `internal/agent/runner.go:339`（`tools := r.registry()`） | 见 a.3 |
| 组装 AgentContext | `internal/agent/runner.go:513-517` | `SystemPrompt: r.systemPrompt()`、`Tools: tools.List()` |
| 执行 loop | `internal/agent/runner.go:518`（`loop.StartRun`） | 之后每次 provider 请求都用这份 context |

### a.2 系统提示词从哪来（PromptSink / PromptSections）

装配点唯一：`internal/servicehost/services.go:190`（`newPromptFactory`），
其中 `services.go:221-224`：

```go
preamble := cfg.Tenant.Agent.SystemPrompt   // 租户文档的 systemPrompt
if preamble == "" {
    preamble = cfg.Prompt.Preamble          // 进程级 --system-prompt
}
```

- 之后 `services.go:229` `host.New(host.Config{Preamble, Sections, SkillRoots, FileReadTool, ExtraTools, IncludeBuiltins: true})`。
- 渲染：`internal/host/host.go:153`（`Host.SystemPrompt`）→ `internal/host/prompt.go:37`
  （`BuildSystemPrompt`）：preamble 原文 + 每个 section 包 `<name>…</name>` + skills 索引。
- runner 侧只有一个读者：`internal/agent/runner.go:102`（`systemPrompt()`）→ `runner.go:514`。
  它在 **每次 attempt 开头读一次**，之后整个 attempt 内不变（`Assembly` 接口只有
  `SystemPrompt()/Tools()` 两个方法，`internal/agent/runner.go:32-35`）。
- 动态段（插件）通路：`internal/agentext/plugin.go:115`（`type PromptSink`）→
  `cmd/keel/serve.go:693`（`Prompt: func(sections)`）→
  `internal/servicehost/host.go:687`（`RefreshProductPrompt`）→ `host.go:704`
  （`refreshAssembly`）→ `host.go:98`（`Host.RefreshSections`）。
  也就是说：**插件能在运行期改 prompt，但 Agent 配置没有对应的 sink**。
- 结论：今天进 prompt 的只有「租户文档 systemPrompt/preamble + tenant sections +
  mcp_servers + 插件 sections + skills 索引」。Agent.`systemPrompt`
  （`internal/agentteam/agent.go:176-183`）**没有任何读者**。

### a.3 工具注册表怎么组装（ToolSink / plugin runtime）

| 层 | 证据 | 内容 |
|---|---|---|
| 唯一 registry | `internal/host/host.go:58`（`buildRegistry`） | builtin(clock/echo) + `cfg.ExtraTools` + skill tool |
| prompt 服务持有 | `internal/servicehost/services.go:190-249` | 构造时把 MCP tools、external tools、`cfg.Product.ExtraTools` 拼进 `ExtraTools` |
| tools 服务只是视图 | `internal/servicehost/services.go:269-277`（`newToolsFactory` → `toolsService{prompt}`） | 不建第二个 registry |
| 插件动态置换 | `internal/agentext/plugin.go:108`（`ToolSink`）→ `cmd/keel/serve.go:689` → `servicehost/host.go:669`（`RefreshProductTools`） | 插件启停即生效 |
| 执行期查询 | `internal/agenttool/tool_executor.go:106`（`cfg.Registry.Get(call.Name)`） | 未知工具 = 工具结果里的错误，不是 run 失败 |
| registry 能力 | `internal/agenttool/registry.go:49/71/80/102` | 只有 `Register/Get/List/Validate`，**没有 `Filter/Clone/Subset`** |

结论：`Agent.tools[]`（`internal/agentteam/agent.go:165-172`，`toolId` + `enabled` + `config`）
**没有任何读者**；registry 也没有「按名单取子集」的 API，所以过滤必须新增（见 c.2）。

### a.4 每条输入的 selection 如何校验与解析

链路：

1. `POST /v1/sessions/{id}/input`：`internal/httpapi/server.go:291`，
   body 只有 `text/requestId/delivery/selection`（`server.go:302-313`）；
   `server.go:325-330` → `vetSelection`（`server.go:391-400`）→
   `Config.ValidateSelection`（`server.go:56`，`cmd/keel/serve.go:341` 绑定
   `selectFromCurrentGeneration(rt)`，`serve.go:798-806`）。
2. `servicehost/models.go:260`（`ValidateSelection`）→ `models.go:261`
   **`modelcfg.ValidateSelectionFact(raw)`**。
3. 真门：`internal/modelcfg/catalog.go:283`

   ```go
   var selectionKeys = []string{"providerId","modelId","reasoningLevel","providerType","surfaceKind","options"}
   var selectionOptionKeys = []string{"reasoningLevel"}
   ```

   `checkSelectionKeys`（`catalog.go:309-353`）拒绝未知键、重复键、`options` 里的未知键。
   **同一词表也被租户文档的 `selection` 使用**：`internal/modelcfg/tenant.go:69`。
4. commit：selection 原样进 `user.message.payload.selection`
   （`internal/httpapi/input_commit.go:63-68`）。
5. 解析：`internal/agent/model.go:65`（`runSelection`）取**最新一条带 selection 的
   `user.message`**；`model.go:101` 交给 `ModelResolver.Resolve`；
   `servicehost/models.go:386-402`（`resolveActive`）语义是
   「fact 有 modelId 则整体覆盖；只有 providerId 则报错；只有 reasoningLevel 则叠加默认」。
6. retry 也走同一门：`internal/httpapi/retry.go:102-109`。

**真库实测（13760，`KEEL_*` 从 `.env.local` 读取，口令未打印）**：

```
POST /v1/sessions/{id}/input {"text":"probe1","selection":{"modelId":"deepseek-v4.1-flash","teamId":"team_x"}}
→ 400 {"error":"selection: unknown key \"teamId\""}

POST /v1/sessions/{id}/input {"text":"probe2","selection":{"modelId":"deepseek-v4.1-flash","agentId":"agent_x"}}
→ 400 {"error":"selection: unknown key \"agentId\""}
```

同时实测到一条**可用旁路**：`/input` 的 typed body 会丢弃未知顶层字段，
但 `POST /v1/sessions/{id}/events`（`internal/httpapi/server.go:498`，raw payload 直通
`kernel.Edge.Commit`）会**原样保留**顶层未知键——读回事件：

```
seq 1 user.message ['requestId','selection','text']                    <- 走 /input
seq 2 user.message ['agentId','requestId','selection','teamId','text']  <- 走 /events
```

kernel 侧只做形状校验（`internal/kernel/validate.go:350-399`：
`text/requestId/delivery/selection/retryOf`，其余字段「unknown fields stay allowed」）。

### a.5 今天随输入可见 / 不可达的 Agent/Team 字段

| 字段 | 数据存储 | run 可见性 | 证据 |
|---|---|---|---|
| `Agent.id/name/description/avatar` | `agents` 文档表 | 不可达（纯展示） | `internal/store/agents.go:36-50` |
| `Agent.defaultModel` | 同上 | **只能被前端换算成 selection 的 modelId**（间接生效） | `web/src/screens/model-picker.tsx:118-130`（`captainSelectionOf`）、`:171` |
| `Agent.systemPrompt` | 同上 | **不可达** | 无读者（a.2） |
| `Agent.tools[].toolId/enabled/config` | 同上 | **不可达** | 无读者（a.3） |
| `Agent.skills` | 同上 | **不可达**（host 只扫磁盘 `SkillRoots`） | `services.go:225-228` |
| `Agent.teamCli` | 同上 | 仅在 HTTP 成员增删时校验 | `internal/httpapi/agents.go:65-67`、`:74-82` |
| `Team.captain/members` | `agent_teams` 文档（单文档，无成员表） | **不可达** | `internal/store/schema.sql:538-548` |
| `Team.defaults.tools/modelOverride` | 同上 | **不可达** | `internal/agentteam/team.go:101-106` |
| `Team.capabilities.teamCli` | 同上 | 同 `Agent.teamCli` | `internal/agentteam/agent.go:281-294` |
| Team 的 revision/hash | `agent_teams.revision/content_hash` 列 | 不可达（无 run manifest） | `internal/store/agents.go:52-71` |

**唯一已生效的间接链路**：前端在选团队时把 captain 的 `defaultModel` 换算成
`selection.modelId`（`web/src/screens/model-picker.tsx:157-173`），因此「团队会话
跑的是队长的模型」这个事实今天只在前端成立，服务端完全不知道有团队。

### a.6 session 是否有可扩展位

- `sessions` 表只有 `tenant_id/session_id/next_seq/state/created_at/updated_at`
  + 外部会话 6 列（`internal/store/schema.sql:19-46`）：**没有文档/blob 列**。
- `POST /v1/sessions`（`internal/httpapi/server.go:265-289`）只接受 `sessionId`，
  调 `Store.CreateSession`（`internal/store/store.go:259`，`INSERT ... VALUES ($1,$2)`）。
- 仓库里 **不存在** `/v1/sessions/{id}/agent` 或 `/team` 端点（grep 为空），
  proposal.md 里写的这两条 API 从未实现。
- 前端 `sessionTeam` 是纯内存 signal（`web/src/data/store.ts:269`、`:652`、`:1056`），
  刷新即丢——这就是「切团队后重开会话团队没了」的根因。
- kernel 侧可用于「会话级配置」的既有载体只有 4 个：session 行本身、`events`、
  `runs` 行（含 `loadout_id/loadout_revision/loadout_generation/loadout_owner_epoch`，
  `internal/kernel/schema/0005-loadout-revision.sql`）、`checkpoints`。

---

## b. 会话 → (agent|team) 绑定的最小契约：3 个候选

三个候选都满足同一条不可让渡的约束：**绑定必须落到「已成事实的输入」上**，
因为 `run` 的 model 解析读的是「最新一条带 selection 的 user.message」
（`internal/agent/model.go:65-82`），只有随输入进来才天然获得
「本 input 自己那次 attempt 生效、切团队只影响下一次 run」这条 spec 语义
（`openspec/changes/keel-agent-team-v1/specs/agent-team/spec.md:63-99`）。

### 候选 A（推荐）：扩展 `selectionKeys`，加 `agentId` + `teamId`

**形状**

```jsonc
// user.message.payload.selection
{ "providerId":"s2api", "modelId":"deepseek-v4.1-flash",
  "agentId":"agent_39d0…", "teamId":"team_5a7e…" }
```

`agentId` 与 `teamId` 同现时 `teamId` 优先（团队决定 captain），`agentId`
用于「标准模式下显式选一个 Agent」。

**要改的文件**

| 文件 | 改动 |
|---|---|
| `internal/modelcfg/catalog.go:283` | `selectionKeys` 加 `"agentId"`、`"teamId"` |
| `internal/modelcfg/catalog.go:355-380`（`decodeSelection` 的目标结构） | 加两个字段 |
| `internal/modelcfg/catalog.go:266-276`（`ParseSelection`） | 无改动（走 `decodeSelection`） |
| `internal/modelcfg/types.go:244-250`（`Selection`） | 加 `AgentID`/`TeamID`（`omitempty`） |
| `internal/modelcfg/tenant.go:57-73` | 无改动（同一个 `selectionKeys` 就自动放行），但**需明确决策**：租户文档要不要允许写 `teamId`？建议显式拒绝（见风险 R2） |
| `internal/servicehost/models.go:248-255`（`Resolve`）、`:386-402`（`resolveActive`） | `resolveActive` 必须**忽略**而非报错这两个键；补充：带 `agentId` 时用 agent 的 `defaultModel` 兜底 |
| `internal/servicehost/models.go:260`（`ValidateSelection`） | 校验扩展为「`agentId`/`teamId` 必须在本租户存在且未被归档、团队必须已 Seal」 |
| `internal/httpapi/server.go:302-313` + `internal/httpapi/input_commit.go:63-68` | 无改动（selection 已 verbatim 透传） |
| `web/src/data/contract.ts:84-92`（`ModelSelection`） | 加 `agentId?`/`teamId?` |
| `web/src/screens/model-picker.tsx:171-172` | 选团队时把 `teamId` 一并写进 selection（今天只写 modelId） |

**要改/新增的测试**

- `internal/modelcfg/catalog_test.go:478`（`TestValidateSelectionFact`）：
  加 `{"modelId":"m","teamId":"t"}` 通过、`{"teamId":"t","agentId":"a"}` 通过、
  仍拒 `{"teamName":"x"}`。
- 新增 `internal/servicehost` 测试：`Resolve` 遇带 `agentId/teamId` 的 fact 不报错且
  model 解析结果不变；`ValidateSelection` 对不存在/归档 agent、未 Seal team 返回
  带字段路径的 400。
- 新增 `internal/httpapi` 测试：`/input` 带 `selection.teamId` 返回 201（今天 400）。
- `web` 侧 harness（`web/src/screens/settings-teams.harness.tsx` 同族）断言发送 payload。

**openspec delta 影响面**

- `specs/agent-team/spec.md`（MODIFIED `会话启动模式` / `团队快照与引用保护`：
  把「会话保存 teamId」明确为「input.selection.teamId」）
- `specs/agent-registry/spec.md`（ADDED：`selection.agentId` 的解析与覆盖优先级）
- 新增 `specs/session-binding/spec.md`（或并入 agent-team）：
  绑定载体 = `user.message.payload.selection`，含 `agentId/teamId` 词表

**优点**：零 schema 迁移；天然 per-input 语义（spec 的「运行中切换只影响下一次 run」
自动成立）；retry 自动携带（`retry.go:102-109`）；前端改动最小。
**缺点**：`selection` 的词表从「模型身份」被扩成「执行身份」，概念上要重新定义
（`catalog.go:278-283` 的注释必须改写）；`modelcfg` 是「纯数据包、不 import kernel」
（`internal/architecture/boundary_test.go:66-81`），所以「agent/team 是否存在」的校验
只能在 `servicehost`（有 store 可达性）做，`modelcfg` 只放行键。

### 候选 B：session 文档字段 + 新端点

**形状**：`sessions` 加一列 `binding jsonb`（或 `agent_team_document`），
新增 `PUT /v1/sessions/{id}/binding`，`worker` 在 claim 后读 session 行取绑定。

**要改的文件**

- `internal/store/schema.sql:19-27`：`ALTER TABLE sessions ADD COLUMN binding jsonb`
  （幂等，仿 `0005-loadout-revision.sql` 写法；注意 `store.Migrate` 是单文件 schema.sql，
  `internal/store/store.go:24`、`:142`，所以直接加在 schema.sql 尾部更符合现状）
- `internal/store/store.go:259`（`CreateSession`）+ 新增 `Get/SetSessionBinding`
- `internal/httpapi/server.go:265-289`（`createSession` 接受 `agentId/teamId`）+
  新端点路由
- `internal/agent/model.go:65-82`：`runSelection` 之后还要读一次 session 绑定
  → **必须新增一个 seam**（runner 目前只有 `Reader/ReadMarker/Models`，
  没有读 session 行的接口）
- `internal/kernel/seams.go`：新增 `SessionBinder` 接口（或复用 `Reader`）
- `internal/servicehost/services.go`：把 seam 注入 runner（`runner.Binding` 名字已被
  loadout pin 占用，需新字段名如 `runner.SessionBinding`）

**测试**：store 层 CRUD、`CreateSession` 带 binding、worker 读到绑定、删除保护
（session 引用 team 时禁删，`spec.md:87-90`）。

**优点**：概念清晰（「会话属于某团队」一个字段），一个 session 一个绑定天然唯一；
未来做「会话详情页显示团队」无需扫事件。
**缺点**：要动 schema + kernel seam + runner 字段，改动面比 A 大 3~4 倍；
「运行中切换只影响下一次 run」需要额外规则（写 session 行即刻生效，
会打到**正在跑的** run —— 必须再加「run 开始时快照」逻辑）；
session 行无写入路径可复用，`CreateSession` 是 `ON CONFLICT DO NOTHING`
（`internal/store/store.go:259-266`），绑定无法通过它更新。

### 候选 C：专用事件 kind（`session.binding`）

**形状**：新增 kernel kind `session.binding`（ClassEdge），payload
`{agentId, teamId, revision, contentHash}`，随输入顺序进日志；
`runSelection` 旁边加 `runBinding(events)` 取最新一条。

**要改的文件**

- `internal/kernel/kernel.go:78-123`（kind 常量）、`internal/kernel/validate.go:45-77`
  （kindRegistry，注意 `run.started/run.failed` 的保留注释 `validate.go:32-39`）、
  `internal/kernel/validate.go:107-135`（`allKinds` 列表，有 pin 测试）
- `internal/store/events.go:6-9`（`EventUserMessage` 等常量对表）
- `internal/httpapi/server.go:521-525`（edge whitelist：`EdgeKinds` 是 ClassEdge 全集，
  加 kind 会自动放行 —— **需确认**这是否期望）
- `internal/agent/model.go`：新增 `runBinding`

**优点**：绑定与 transcript 完全解耦（不会像 A 那样污染 selection 词表）；
历史可审计。
**缺点**：要改 kernel 契约表（有 pin 测试 + R1 parity gate 风险，见
`validate.go:32-39` 的明确警告）；前端要多一次 commit 才能开跑（两次写、两处幂等）；
且 `DecodeMessages` 的 `default`（`internal/agent/messages.go:103-105`）要确认不参与上下文。

### 团队快照冻结放哪一层

三个候选共用同一裁决：**冻结在「输入事实」上，落在 `run.finished` 的 summary 里**。

- 冻结时机：`servicehost` 在 `Resolve` 阶段（`internal/servicehost/models.go:248`）
  读 agent/team 文档的那一刻就是快照点——因为该函数每次 attempt 各调一次
  （`internal/agent/model.go:101`），所以「改动只影响下一次 attempt」自动成立。
- 落盘位置：`RunSummary`（`internal/agent/runner.go:146-178`）已有
  `Provider/Model/ConfigHashBefore/ConfigHashAfter`，加
  `AgentID/TeamID/TeamRevision/TeamHash/PromptHash` 五个字段即可；
  kernel 的 `validateRunFinishedPayload`（`internal/kernel/validate.go:404-487`）
  只校验已知字段形状、未知字段放行（`validate.go:411-413` 注释），
  **但 `provider/model/configHash*` 是显式 string 校验（`validate.go:438-446`），
  新字段建议同样加进去**。
- 真实快照内容（captain/成员/prompt hash）不进 `run.finished`（会很大），
  只进 `event` 日志的一等事件或直接计算 hash；团队文档本体已在
  `agent_teams.document` + `revision` + `content_hash` 里（`internal/store/agents.go:52-71`），
  `AgentTeam.Hash()`（`internal/agentteam/team.go:264`）已可复算，所以
  **只需要把 (revision, hash) 钉进 summary，就足以证明「这个 run 用的是哪一版团队」**。

### 推荐：候选 A

理由三条：

1. **最小改动 + 最大语义正确**：`selection` 已经是「这条输入的执行身份」，
   把 `agentId/teamId` 加进去是同一概念的扩展，不是第二个概念；per-input 语义
   直接满足 spec 的「运行中切换只影响下一次 run」，无需额外快照逻辑。
2. **真库已验证可行**：`/events` 直通顶层未知键的实测（a.4）证明 kernel 不会拦；
   需要动的只有 `modelcfg` 的封闭词表（一处常量 + 两个结构体），
   是三个候选里唯一**不碰 schema、不碰 kernel kind 表**的方案。
3. **失败模式可控**：未知 `agentId/teamId` 的拒绝点在 `servicehost.ValidateSelection`
   （submit-time 400），与今天 model 不可解析的 400 体验一致
   （`internal/httpapi/server.go:325-330`），不会引入异步失败。

候选 B 留作「会话详情/引用保护」需求的自然落点（`spec.md:87-90` 的
「团队被会话引用时 MUST NOT 硬删」今天无法判定，因为 session 不知道自己属于谁）；
建议**先做 A，把 B 的 `sessions` 列作为后续独立 change**。

---

## c. run 装配注入点（file:line）

注入点一律选「**run 级、per-attempt**」的位置，因为 `Models ModelResolver`
（`internal/agent/runner.go:76`、`internal/agent/model.go:57-59`）已经证明了这个模式：
resolver 在 `runner.go:327` 每次 attempt 调一次，天然满足「改动只影响下一次 run」。

### c.1 systemPrompt：注入到 PromptSink 链的位置与优先级

**不要**走 `agentext.PromptSink`（`internal/agentext/plugin.go:115`）——那是**插件生命周期**
驱动的（启停才变），而 agent 选择是 per-input 的；塞进去会导致「进程级全局 prompt 被
某次会话的选择污染」。

**正确位置**：`internal/agent/runner.go:514` 的 `SystemPrompt: r.systemPrompt()`，
把它换成 `SystemPrompt: prompt`，其中 `prompt` 来自新增的 per-run resolver：

| 改动 | 位置 |
|---|---|
| 新增 resolver 字段（照 `Models` 的样子） | `internal/agent/runner.go:76` 旁：`Prompt PromptResolver` |
| resolver 接口 | 新文件 `internal/agent/prompt.go`：`Resolve(ctx, scope, selection) (PromptResolve, error)`，`PromptResolve{Prompt string, Hash string, ToolNames []string}` |
| attempt 调用点 | `internal/agent/runner.go:327-338` 之后、`:514` 之前（**必须在任何 commit 之前**，与 model 解析同一条铁律，`runner.go:322-326` 注释） |
| 渲染时保留插件/MCP/skills 段 | `internal/host/host.go:153`（`SystemPrompt`）改造成 `SystemPrompt()` 调 `RenderSystemPrompt("")`，新增 `RenderSystemPrompt(preamble string) string`（`preamble==""` 回落 `cfg.Preamble`）；`internal/host/prompt.go:37` 不动 |
| 转发 | `internal/servicehost/services.go:182/261`（`promptService.SystemPrompt`、`toolsService.SystemPrompt`）各加 `RenderSystemPrompt(preamble string) string` |
| 数据来源 | `internal/servicehost` 新增 agent/team 读取（`store.GetAgent`/`GetTeam`，`internal/httpapi/agents.go:53/60` 同签名；注意 `internal/store` 不能 import `internal/agentteam`，`internal/architecture/agentteam_boundary_test.go:51-54`，所以要新建一个 `internal/agentbinding` 纯规则包或让 `servicehost` 直接 import `agentteam`——`agentteam` 只禁止被 `store`/`agent`/`agentext` import，`agentteam_boundary_test.go:60-65`，`servicehost` import 它是合法的） |

**优先级（高 → 低）**：

1. 会话/输入：`selection.agentId` 或 `selection.teamId`（→ captain）的 `systemPrompt.inline`
2. 同上，`systemPrompt.kind=file` →  resolver 读 `systemPrompt.ref`（工作区内相对路径，
   校验在 `internal/agentteam/validate.go:313`（`validateSystemPrompt`），
   读失败**不 fail run**，退化为无该段并记 warning（与 skills 发现失败的处理一致，
   `internal/host/host.go:47-53`）
3. 租户文档 `agent.systemPrompt`（今天的行为，`internal/servicehost/services.go:221`）
4. 进程 `--system-prompt`（`services.go:222-224`，`Preamble`）

**关键约束**：无论哪一级生效，`<mcp_servers>`、插件 sections、`<skills>` 索引
**必须照常渲染**——所以注入的是「preamble 覆盖」，不是「整个 prompt 替换」。
否则选一个 Agent 会静默关掉 MCP 与技能索引（今天的 `TestRefreshProductPromptPublishesPluginSections`
`internal/servicehost/prompt_test.go:40-110` 就是这条不变量的守卫，需扩展一条
「agent preamble 生效时插件 section 仍在」的用例）。

### c.2 agent 工具授权（`tools[].toolId + enabled`）如何过滤 registry

**registry 今天没有子集能力**（`internal/agenttool/registry.go:49/71/80/102`），需要新增：

```go
// internal/agenttool/registry.go
func (r *ToolRegistry) Subset(names []string) (*ToolRegistry, error) // 复制 + 重新 Register（schema 重新编译）
```

（复制而非共享指针，理由：`List()` 直接进 `AgentContext.Tools`
`internal/agent/runner.go:516`，共享会让一个 run 的工具面被另一个 run 改掉。）

**过滤规则（fail closed）**：

| 输入 | 结果 |
|---|---|
| agent `tools` 为空 / 未声明 | **全量**（spec：「工具与系统提示词为空时使用默认值」，`specs/agent-registry/spec.md:19-22`） |
| agent 声明了 grants | 仅 `enabled=true` 的 toolId 子集 |
| team `defaults.tools` 非空 | 成员/队长取 `team.defaults.tools ∪ agent.tools(enabled)`，`agent.tools` 里 `enabled=false` 的显式**减掉** |
| toolId 不在当前 registry（插件卸载） | **不 fail run**，记 warning 并从子集剔除（spec 要求「状态显示不可用」而非阻断，`specs/agent-registry/spec.md:41-48`） |
| 子集为空但 agent 声明了 grants | 空工具面（合法：模型无工具也能答） |

**注入位置**：`internal/agent/runner.go:339`（`tools := r.registry()`）→
`tools, err := r.toolsForRun(ctx, lease, selection)`；失败在此 return
（与 `runner.go:344-349` 的「无工具支持模型不得拿到工具」同一守卫位）。

**另需注意**：`internal/agent/runner.go:344-349` 会在模型声明
`SupportsToolCall=false` 且工具非空时**直接 fail run**。过滤后工具面为空时
这一守卫不再触发——这是期望行为，但要有测试固定（否则选一个 no-tool 模型的 Agent
会从「必然失败」变成「意外成功」，属于行为变更）。

### c.3 captain 作为主 agent 的注入点

一条链路，三处接线：

1. **解析**：`servicehost` 的 resolver 见 `selection.teamId` → `store.GetTeam` →
   `Team.ValidateForStart`（`internal/agentteam/team.go:300-316`，要求已 Seal，
   否则 `not_sealed`，`validate.go:92-94`）→ 取 `team.Captain.AgentID`
   → `store.GetAgent` → 得 captain 的 `defaultModel` / `systemPrompt` / `tools`。
2. **模型**：captain 的 `defaultModel` 只作为 **selection 的兜底**——
   `resolveActive`（`internal/servicehost/models.go:386-402`）在 fact 没有 `modelId`
   时用它填 `modelcfg.Selection`，从而实现 spec 的
   「会话 > Agent 默认模型 > 租户默认」（`specs/agent-registry/spec.md:35-40`）。
   今天前端在做这件事（`web/src/screens/model-picker.tsx:118-130`），
   服务端接管后前端只保留展示用途。
3. **成员**：`spawn_subagent` 已存在（`internal/agentext/builtin.go:251-265`，
   工具体 `internal/agentext/subagent.go:619-682`），且请求有 `Agent` 字段
   （`subagent.go:48`）。但子会话的 `user.message` 只写
   `subagent: {parentSessionId, agent, idempotencyKey, budget}`（`subagent.go:200-207`），
   **不带 selection**，所以子会话的 Agent 配置同样不生效。
   最小改法：`SpawnRequest` 增加 `Selection json.RawMessage`，写进
   `extras["selection"]`；`spawn_subagent` 工具在 captain 上下文里从
   `teamId` 解析成员 → 填成员的 `agentId` + `defaultModel`。

### c.4 成员结果进 captain 时间线（规则草案，不实现）

- **载体**：`spawn_subagent` 的返回值已经是结构化 JSON
  （`SubagentResult`，`subagent.go:63-76`：`status/summary/artifacts/usage/sessionId`），
  它作为 tool result 天然进 captain 的 transcript——**不需要新事件**。
- **规则 R1（最小）**：captain 的时间线只出现「一次工具调用 + 一条工具结果」，
  结果正文是 `SubagentResult` 的 `summary`（截断上限沿用
  `internal/agenttool/tool_executor.go` 的 `toolResultMaxBytes`）；`artifacts` 只留
  `type/name/ref`，不带子会话正文（`subagent.go:56-61` 已这样设计）。
- **规则 R2**：成员会话是**独立 session**（`subagent.go:191-211`），
  captain 不可见其 transcript；要看细节必须再发一次 `spawn_subagent` 或由成员回传摘要。
- **规则 R3**：团队模式下 captain 的工具面必须包含 `spawn_subagent`
  （即使 `agent.tools` 里没写），否则团队等于单 agent——这条要写进 c.2 的
  「始终保留」白名单（另一个候选是显式声明，更符合 fail-closed 气质）。
- **规则 R4**：成员失败不 fail captain run（`subagent.go:19-21` 已如此），
  失败以 `status=failed` + `error` 出现在那条 tool result 里。
- **规则 R5（非目标）**：不做「成员流式输出进 captain 时间线」的实时投影；
  那需要跨 session 的 SSE 合流，属于独立 change。

---

## d. 测试与证据计划

### d.1 主证据：真库 E2E + 标记串贯穿

目标：证明「会话选了 agent 后，agent 的 systemPrompt 真的到了 provider 的 wire 请求里」，
只认 wire/日志里的标记串，不认 UI 断言。

**fixture（独立命名，结束清理）**

```
agent:  kt-runtime-probe-<ts>    systemPrompt: {"kind":"inline","text":"KITE-RUNTIME-MARK-7 ..."}
team:   kt-runtime-probe-team-<ts>  captain = 上面的 agent
```

**步骤**

1. 起一个**独立库**的 serve 实例（照 `scripts/real-model-e2e.sh:767/801-804` 的
   `DROP/CREATE DATABASE ... WITH (FORCE)`；端口用 `pick_port`，
   **避开 13610-13799**，脚本 `:265-266` 已有这条断言）。
2. `keel agent create --name kt-runtime-probe-<ts> --provider <p> --model <m>
   --prompt "KITE-RUNTIME-MARK-7 …"` → 记下 agentId。
3. `keel team create --name kt-runtime-probe-team-<ts> --captain <agentId>` → teamId。
4. 建 session；`POST /v1/sessions/{id}/input` 带
   `{"selection":{"providerId":…,"modelId":…,"teamId":"<teamId>"}}`，断言 **201**
   （这是 A 方案的第一个门：今天必 400）。
5. **wire 断言**：复用 `scripts/real-model-e2e.sh:410-521` 的 recording proxy
   （`start_recording_proxy`），把 provider 的 `api.baseUrl` 指向它；proxy 已记录
   `body`，需**加一行记录 `messages[0].role=="system"` 的 `content` 摘要**
   （现在只记 `model/reasoningEffort/maxTokens/stream/hasTools/topLevelKeys`，
   见 `:456-469`）。断言 `KITE-RUNTIME-MARK-7` 出现在 system message 里。
6. **事件/SSR 侧断言**（wire 之外的旁证）：
   - `GET /v1/sessions/{id}/events` 的 `user.message.payload.selection.teamId` 存在；
   - `run.finished.payload` 带 `teamId/teamRevision/teamHash/promptHash`（c.1/c.3 新增字段）；
   - `GET /v1/sessions/{id}/events/stream`（`internal/httpapi/server.go:651`）透传的
     `run.finished` 同样带这些字段。
7. **反向断言（今天必然失败，改造后必须通过）**：
   - `selection.teamId` 不存在的团队 → 400 且 body 含 `teamId` 字段路径；
   - 归档 agent → 400；未 Seal 的团队 → 400 `not_sealed`（`internal/agentteam/validate.go:92-94`）。

### d.2 工具面过滤的验证方法

分三层，从便宜到贵：

1. **单测（唯一真值层）**：`internal/agenttool` 新增 `Subset` 的测试（含「未知名忽略」、
   「空名单=全量」、「schema 重新编译后仍能 Validate」）；
   `internal/agent` 新增 `toolsForRun` 测试：给定 agent grants（`a=true,b=false`）+
   registry（a,b,c）→ 得 {a}；team defaults.tools=['b'] + agent grants a=true →
   得 {a,b}（team 是并集，agent 的 enabled=false 只减自己声明的）。
2. **run 级集成（`internal/agent` 或 `internal/servicehost`）**：用
   `TestRunnerUsesHostAssembly`（`internal/agent/runner_test.go:461`）同型测试：
   断言 `provider` 收到的工具声明列表里**没有**被过滤的工具名。
   现成的 recording stream 双（`internal/agent/model_test.go:20` 的 `recordingStream`，
   能抓到每次请求的 `systemPrompt/model/tools`）足以完成断言。
3. **E2E（真库，弱断言即可）**：给 probe agent 只授权 `clock`，
   然后让模型尝试 `echo` → 期望 tool result 是
   `unknown tool`（`internal/agenttool/tool_executor.go:106` 的失败路径），
   且 `run.finished.toolTimings` 里出现该错误名。这条**不需要** provider 配合，
   用现有 fake/demo 或真模型都能跑。

### d.3 既有门禁不能被削弱（回归清单）

- `internal/modelcfg/catalog_test.go:478`（`TestValidateSelectionFact`）里的
  `unknown key "baseUrl"` 与 `options: unknown key "temperature"` 必须仍然失败——
  加 `agentId/teamId` 只放行这两个键，不能放宽成白名单外全放。
- `scripts/real-model-e2e.sh:1282-1286` 已经用
  `selection.apiKey` 作为「未知键必须 400」的活断言；**不要**把它换成
  `agentId`，否则这条回归门失效。
- `internal/servicehost/prompt_test.go:40-110` 的「插件 section 与配置 section 都不能被
  刷新丢掉」必须在 agent preamble 生效时依然成立（新增一条子用例）。
- `internal/architecture/agentteam_boundary_test.go:51-65`：`store` 不得 import
  `agentteam`，`agentteam` 不得 import `agent/store/agentext`——
  新增 resolver 时 `servicehost`/`agent` 的依赖方向要照此摆放
  （`internal/agent` **不能** import `internal/agentteam`，所以 resolver 必须定义在
  `internal/agent` 里的纯数据结构 + `servicehost` 侧实现，同 `Models ModelResolver`
  的既有分工，`internal/agent/model.go:50-59`）。

### d.4 证据产物

- 落 `.agents/reports/keel/agent-team-runtime-v1/`（项目规范：E2E HTML 报告 +
  截图，见 `AGENTS.md` 的「E2E 测试报告＝HTML 图文文档」）。
- 原始 JSON：`events-*.json`、`run-*.json`、`wire-requests.jsonl`（含 system prompt 摘要）、
  `serve.log` 的 grep 结果（`KITE-RUNTIME-MARK-7`）。
- 一份「原始需求 → 代码证据」对账表（GAP-AUDIT 的教训：不允许只报增量）。

---

## e. 分阶段实施拆解（每阶段 ≤ 半天）

> 顺序原则：先让**契约与词表**能承载（P1），再让**模型侧**正确解析（P2），
> 然后才是 prompt/tools 真正注入（P3/P4），最后团队编排与 UI（P5/P6）。
> 每阶段都必须可独立验证，不留「改一半」状态。

### P1（0.5d）selection 词表扩展 + 存在性校验

- 改动：`internal/modelcfg/catalog.go:283`（`selectionKeys`）、
  `decodeSelection`（`:355-380`）、`internal/modelcfg/types.go:244-250`；
  `internal/servicehost/models.go:260-270`（`ValidateSelection` 增加 agent/team 存在性
  与 Seal 校验，读 `store.GetAgent/GetTeam`）；
  `internal/servicehost/services.go` 新增 agent/team 读取 seam（`AgentTeams` 已可从
  `cmd/keel/serve.go:328` 的 `st` 提供）。
- 测试：`catalog_test.go` 新增 4 例；`servicehost` 新增 `ValidateSelection` 三例
  （不存在 agent / 归档 agent / 未 Seal team）。
- 验收：`go test ./internal/modelcfg/... ./internal/servicehost/...` 全绿；
  真库 `POST /input` 带 `selection.teamId` 返回 201（探针手工验证）。

### P2（0.5d）resolver：captain/agent → 模型 + prompt + toolNames

- 改动：`internal/agent/model.go:57`（`ModelResolver` 不改）新增
  `internal/agent/prompt.go`（`PromptResolver` + `PromptResolve{Prompt,Hash,ToolNames}`）；
  `internal/agent/runner.go:76`（新字段）、`:327-338` 后调用、`:339` 用 `ToolNames` 过滤、
  `:514` 用解析出的 prompt。
- 测试：`internal/agent` 新测试（resolver 返回的 prompt 进 wire；resolver 报错则
  attempt 在任何 commit 前失败，照 `internal/agent/model_test.go` 的失败例写法）。
- 验收：`go test ./internal/agent/...`；`go test ./internal/architecture/...`
  （边界不能破）。

### P3（0.5d）systemPrompt 注入 + 优先级链

- 改动：`internal/host/host.go:153` 拆出 `RenderSystemPrompt(preamble)`；
  `internal/servicehost/services.go:182/261` 转发；
  `internal/servicehost` 实现 `PromptResolver`（inline / file ref 读取 +
  优先级：selection agent > 租户文档 > 进程 preamble）；
  file ref 读取失败降级为无该段 + warning。
- 测试：`internal/servicehost/prompt_test.go` 新增两条
  （agent preamble 覆盖租户；插件 section 在两种情况下都不丢）；
  新增 file-ref 读取用例（含路径逃逸被拒，`validateSystemPrompt` 已有规则，`internal/agentteam/validate.go:313`）。
- 验收：`go test ./internal/host/... ./internal/servicehost/...` 全绿；
  **本阶段结束即可跑 d.1 的 wire 标记断言**（工具面仍是全量）。

### P4（0.5d）工具面过滤

- 改动：`internal/agenttool/registry.go` 新增 `Subset(names)`；
  `internal/agent/runner.go:339` → `toolsForRun`（规则见 c.2，含 team defaults 并集、
  `enabled=false` 减集、未知名剔除 + warning、**始终保留 `spawn_subagent`** 的决策点）；
  注意 `runner.go:344-349` 的 no-tool 模型守卫语义变化。
- 测试：`registry` Subset 3 例 + `agent` 过滤矩阵 4 例 + no-tool 守卫 1 例。
- 验收：`go test ./internal/agenttool/... ./internal/agent/...`；
  E2E 弱断言（d.2 第 3 层）通过。

### P5（0.5d）team 编排：captain + 成员 selection 传递

- 改动：`internal/servicehost` resolver 团队分支（`team.Captain.AgentID` →
  `store.GetAgent`，`ValidateForStart` 门）；
  `internal/agentext/subagent.go:41-59`（`SpawnRequest.Selection`）、
  `:200-207`（写进 extras）；`internal/agentext/builtin.go:251-265`
  （`spawn_subagent` schema 增 `agentId`，或由 captain 上下文注入成员解析）。
- 测试：`internal/servicehost` 团队解析 3 例（缺 captain / 未 Seal / 正常）；
  `internal/agentext` 子会话 extras 带 selection 的 1 例。
- 验收：真库「团队会话 → spawn_subagent → 成员自己的 prompt 生效」
  （成员 prompt 用第二个标记串 `KITE-RUNTIME-MARK-8` 区分）。

### P6（0.5d）run 快照落盘 + 前端 selection 携带 + 收口

- 改动：`internal/agent/runner.go:146-178`（`RunSummary` 加
  `AgentID/TeamID/TeamRevision/TeamHash/PromptHash`）；
  `internal/kernel/validate.go:438-446`（同形状校验）；
  `internal/httpapi/retry.go:102-109` 确认携带（已 verbatim，无需改）；
  `web/src/data/contract.ts:84-92`、`web/src/screens/model-picker.tsx:171-172`
  （选团队时写 `teamId`）、`web/src/data/store.ts:652-657`（team 标记与 selection 同源）。
- 测试：`internal/kernel` 的 run.finished 形状例；`web` harness 断言 payload；
  `nsh check` + `nsh test --target tui`（`web/README.md:26-30`）。
- 验收：`go test ./...`；`nsh check`；d.1 的 E2E 全套（含反向断言）通过；
  openspec `tasks.md` 的 P2/P3/P4 相关条目逐条勾除并附 file:line。

### 依赖关系

```
P1 ──▶ P2 ──▶ P3 ──▶ P6（快照/前端收口）
        └────▶ P4 ──▶ P5
```

P3 与 P4 可并行（两者都依赖 P2 的 resolver 骨架，但改的文件不重叠）；
P5 必须在 P4 之后，因为 `spawn_subagent` 的保留策略属于工具面规则。

### 明确不在本计划内

- 候选 B 的 `sessions.binding` 列与 `/v1/sessions/{id}/binding` 端点
  （→ 独立 change，服务「团队被会话引用时禁删」`spec.md:87-90`）。
- 成员输出实时投影进 captain 时间线（c.4 R5）。
- `keel agent start/stop`、`keel team save/start` 子命令（tasks.md 未完成项，
  与 run 注入无依赖，可另开）。

---

## 附录：勘察期的真库操作与残留物（已清零）

本阶段只在既有真库（`127.0.0.1:13760`，库 `runtime` @ 容器 `keel-pg`）上做过
**读 + 三次探针写**，用于验证 a.4 的 `selection` 词表边界。凭据一律从
`.env.local` 读入 shell 变量，**未打印** `KEEL_SERVE_PASSWORD` / `KEEL_API_KEY` /
`Authorization` / DSN 口令。

**写入了什么**（三个探针 session，均为随机 uuid，未创建任何 fixture agent/team）：

| session_id | 用途 | 事件数 |
|---|---|---|
| `429cf88f-56e0-485d-a36b-18d6c32c7208` | `/input` 被 400 拒绝（无事件落库） | 0 |
| `80d1c14e-90b3-4339-988e-6ebb97f6583a` | `/input` + 顶层 `teamId/agentId`（证明 typed body 丢弃未知键） | 7 |
| `1cbbd956-949d-46d2-9526-58d80a5aa984` | `/events` raw payload 直通（证明 kernel 放行未知顶层键） | 8 |

**清理证据**（单事务删除 + 复核计数）：

```
DELETE events 15 / runs 2 / submissions 2 / sessions 3
verify: sessions=0 events=0 runs=0
checkpoints / blob_refs / usage_ledger 本为 0
```

**旁注**：探针触发了一次真实模型调用并以
`upstream 401 {"code":"INVALID_API_KEY"}` 失败（`.env.local` 的 `KEEL_API_KEY`
对该网关无效）——这正是 `/input` 顶层未知键被静默丢弃的证据来源：
两条 session 都跑到 `run.finished`，但模型侧拿到的 selection 只有 model 两项。
该 run 行已随上面的清理删除。

**结论**：真库无探针残留；`agents` / `agent_teams` 两张表零新增行；
本阶段唯一新增文件是本文。

---

## 审查裁决（2026-11-14，MainAgent）

方案**通过**，按 P1→P6 执行。以下四处为审查裁决，实现时照此落地：

1. **R3 定案——团队模式自动保留 `spawn_subagent`**（不采用「仅显式声明」）：
   - 规则：`selection.teamId` 生效的 run，工具面**自动包含** `spawn_subagent`，
     即使 captain 的 `tools` grants 非空且未列出它；**唯一例外**：grants 显式列出
     `spawn_subagent` 且 `enabled=false`（用户的显式决定优先）。
   - 理由：团队的定义性能力就是委派成员；「队长存在但永远不能 spawn」= 静默坏掉的产品
     （违反「不得把坏状态留给用户」）。写进 `specs/agent-team/spec.md` 的 MODIFIED 条目，
     并加一条 P4 矩阵测试。
2. **租户文档拒绝执行身份键**：`tenant.go` 的文档 selection 校验路径继续用
   **model-only 键表**（不带 agentId/teamId）——把扩展键表只用于「输入 selection」路径。
   P1 实现时给 `ValidateSelectionFact` 拆出显式参数（如 `scope input|document`），
   测试各加一例：文档带 `teamId` 必须 400。
3. **前端去权威化（P6）**：`model-picker.tsx` 的 `captainSelectionOf`（客户端换算
   captain defaultModel）降级为**展示用途**；选择的权威换算在服务端 resolver。
   P6 需加一条 TUI 断言：选团队后发送的 payload `selection` 同时含 `modelId` 与 `teamId`
   （客户端不再替服务端「猜」模型优先级）。
4. **排期**：线 B（provider 删除保护等）正在编辑 `internal/agentapi`、`servicehost/host.go`
   与 `internal/store/agents.go`；本实现**待线 B 收口后开班**，避免同包并发编辑。
   P1 的 `modelcfg` 部分与 P2 的 `internal/agent` 部分与线 B 无文件重叠，
   开班后可按 P1→P3→(P4∥)→P5→P6 连续执行。

