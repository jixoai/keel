# Tasks: keel-collab-core

## 1. 契约与包边界

- [x] 1.1 新建 `internal/collab`：ModeDescriptor、ModeRegistry、Host、WorkItem/Assignment/
      Attention/Source/Channel/Link 端口与类型化错误。
- [x] 1.2 冻结依赖规则：mode 插件不得 import kernel/store 直接建 Session；Host 是唯一门。
      （`internal/architecture` 的 `TestCollabCoreStaysIndependent` 钉住依赖方向。）
- [x] 1.3 定义 tenant 上限与 Bot/WorkItem 只能收窄的 limit 合并规则（depth/children/
      concurrency/budget）。

## 2. 模式注册与装配

- [x] 2.1 `cmd/keel/serve.go` 构建 ModeRegistry；内置注册 `subagent`，为 `superagent`
      预留注册点；重复/未知 id 启动失败。（superagent 先以 disabled descriptor 注册；
      subagent 默认深度 3 由 `DefaultSubagentMaxDepth` + `LimitedDepth(3)` 表达并用测试钉住。）
- [x] 2.2 只读模式列表 API/装配投影：id、roles、topology、workUnit、limits、enabled。
      （`GET /v1/capabilities` 的 `collab.modes` 块 + `collab` 目录。）
- [x] 2.3 模式停用路径：拒绝新 WorkItem，保留既有 WorkItem/Assignment 可读/可接管。

## 3. 受控 Host

- [x] 3.1 Host.CreateSession：tenant、`bot.invoke`、permission、budget、depth/width、
      concurrency、取消传播、审计。
- [x] 3.2 Host.Assign：WorkItem → Bot/Session 绑定；Assignment 生命周期独立于 Session。
- [x] 3.3 Host.Ask：结构化 Attention；复用 decision 语义；路由与超时。
      （已含路由/超时端口 `AttentionPublisher`；DecisionBroker 具体适配留给后续 mode change。）
- [x] 3.4 负控：插件绕过 Host、跨租户、缺 `bot.invoke`、超预算/深度均拒绝并审计。

## 4. 现有 subagent 适配

- [x] 4.1 `harnessext.SubagentRunner` 适配为 `subagent` mode 的 Host 使用者，保持现有
      `agent` 工具和 HTTP 响应兼容。
      （`harnessext.CollabSubagentLauncher` 适配器已提供；生产 Host 装配留给
      `keel-collab-subagent`，现有入口未改动。）
- [x] 4.2 toolsurface 只解析参数/投影结果，不再自行实现权限/深度判断。
      （现状已满足：权限在 `toolauth`，toolsurface 没有深度计数；本阶段未改其行为。）
- [ ] 4.3 旧入口与新 Host 入口的一致性测试。

## 5. 验收

- [x] 5.1 `go test -race` 相关包；`openspec validate --strict keel-collab-core`。
- [x] 5.2 独立子代理复核契约与依赖方向；P0/P1 清零。
      （复核发现 8 个 P1，已在 §6 逐项修复并补测试；P2 登记在 §6。）
- [ ] 5.3 更新 `docs/HARNESS.md`/`docs/ARCHITECTURE.md` 的协作模式边界，归档本 change。

## 6. 独立复核修复（第二轮）

- [x] P1-2 CreateSession idempotency：同 key 返回同一 Session；成功重试、部分失败重试、
      并发/宽度已满重试测试。
- [x] P1-3 深度策略：新增显式 `DepthPolicy`（forbidden/unlimited/limited），不再用 0 表示
      多种语义；ModeDescriptor 必须声明；subagent 默认 `LimitedDepth(3)` 由测试钉住。
- [x] P1-4 WorkItem membership + `Scope.BotIDs`：Assign/Ask/CreateSession 统一校验；
      跨 WorkItem/bare-child/scope 负控测试。
- [x] P1-5 人工接管：移除自报 `HumanTakeover`，改为独立 `OperatorChecker` seam；
      停用模式无 operator 权限时拒绝。
- [x] P1-6 取消覆盖：WorkItem scoped Session 必须绑定 Assignment；Cancel 递归取消
      所有 bare 子 Session/子树；补 bare 子树测试。
- [x] P1-7 容量原子预留：宽度/并发 check+reserve 在 Host 互斥区内完成；补并发 goroutine
      + `-race` 测试。
- [x] P1-8 审计 fail-closed：OpenWorkItem/Assign/Ask/CreateSession 的 accepted 审计在写入前
      完成；审计失败返回 `audit_unavailable`；补测试。
- [x] P2-1 Attention answer/expire/幂等回答 + 超时标记。
- [x] P2-2 `CompleteSession` 投影 done/failed/cancelled 并释放配额。
- [x] P2-3 `NewHost` 校验 registry tenant 与 Host tenant 一致。
- [ ] P2-4 Assignment 跨 WorkItem 重绑的剩余路径与错误分类细化（当前 CreateSession 已拒绝
      已绑定 assignment；Assign 对已有 assignment 的 session 仍未做全局重绑扫描）。
- [ ] P2-5 Link 端点类型已校验；ID 生成/去重与跨 WorkItem 关系仍可细化。
- [ ] P2-6 per-request tenant registry（当前 registry 仍为装配期单租户对象）。
