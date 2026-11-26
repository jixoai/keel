# Design: keel-collab-core

## 1. 分层

```text
Kernel
  Session / Run / event ledger / lease
  —— 不知道 Bot、mode、main/sub、ticket

Harness core
  Bot role / permissions / Session execution
  Collab Host：受控创建、预算、深度、取消、审计
  Mode registry：subagent / superagent / future

Mode plugin
  subagent：父子树、委派、结果聚合
  superagent：Source → Ticket → Assignment → Worker
  future：pipeline / peer review / swarm ...

Source / Channel plugin
  GitHub / Feishu / 其他外部边界
```

Kernel 保持通用；Bot 和协作模式都在 Harness 层。Mode 插件不得 import kernel/store
直接创建 Session，只能调用 Host。

## 2. 核心类型（端口，不是最终表结构）

```go
type ModeDescriptor struct {
    ID          string
    Roles       []RoleSpec       // 需要哪些 Bot 角色，不是 BotKind
    Topology    string           // tree | work-queue | pipeline | peer | ...
    WorkUnit    string           // delegation | ticket | job | ...
    Limits      ModeLimits       // depth/children/concurrency/budget
    Capabilities []string        // attention, source, channel, ...
}

type Host interface {
    CreateSession(ctx, tenantID, modeID string, req SessionRequest) (SessionRef, error)
    Assign(ctx, tenantID, workItemID, botID string, req AssignmentRequest) (AssignmentRef, error)
    Ask(ctx, tenantID, workItemID string, req AttentionRequest) (AttentionRef, error)
    Cancel(ctx, tenantID, workItemID string, reason string) error
}
```

`WorkItem`、`Assignment`、`Attention`、`Source`、`Channel` 先以接口/记录类型出现；
持久化实现由具体 mode change 决定。core 不强行把所有模式塞进一张表。

## 3. 不变量

- **角色/模式分离**：同一 Bot 可在不同 WorkItem 中作为 main、sub、worker 或 orchestrator；
  这些是 mode 的位置，不是 Bot 的属性。
- **单一创建门**：任何 mode 创建的 Session 都经过 Host；Host 校验 tenant、`bot.invoke`、
  budget、depth/width、并发和取消传播，并写审计。
- **模式不可提权**：mode 不能读取其他租户/其他 WorkItem 的 Session；不能绕过 `bot.invoke`。
- **模式可停用**：停用 mode 只停止新工作；既有 WorkItem/Assignment 可读、可人工接管、
  可审计，不能静默丢失。
- **单租户只是租户数为 1**：所有 WorkItem/Assignment/mode 配置带 tenant_id，不使用第二套
  单租户代码路径。

## 4. 与现有代码的接入

- `internal/harnessext.SubagentRunner` 先适配为 `subagent` mode 的 Host 使用者：
  对外 API 不变，内部把 spawn/HTTP/tool 入口统一到 Host。
- `internal/toolsurface/agent` 只做参数解析和结果投影；不自己数深度。
- `cmd/keel/serve.go` 构建 ModeRegistry，把 Host 注入产品工具面。
- 现有 Bot/TeamBot 表不增加 `kind=super` 或 `kind=sub`。

## 5. 兼容与迁移

- Alpha 零兼容：新模式元数据直接新增；旧 `agent` 工具语义保留为 `subagent` mode 的接口。
- 不新增 BotKind；不迁移 `bots` 表。
- 任何新模式若需要新持久化，由对应 change 增加，不在 core 里预建空表。

## 6. 开放问题（实现前冻结）

1. `WorkItem`/`Assignment` 是复用 `tasks`/`runs` 还是新建协作表？
   - 倾向：core 只定义接口；superagent 用独立表，subagent 用现有 Session/task card。
2. mode 插件是进程内 Go 注册，还是 `plugin.yaml` 可安装插件？
   - 倾向：先进程内内置注册，插件安装留给后续；两者共用 ModeDescriptor。
3. Host 的深度/并发限制放在 tenant 配置还是 Bot Harness？
   - 倾向：tenant 上限 + Bot/WorkItem 只能收窄，不能突破 tenant 上限。
