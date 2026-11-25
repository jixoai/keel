# Tasks: keel-collab-core

## 1. 契约与包边界

- [ ] 1.1 新建 `internal/collab`：ModeDescriptor、ModeRegistry、Host、WorkItem/Assignment/
      Attention/Source/Channel/Link 端口与类型化错误。
- [ ] 1.2 冻结依赖规则：mode 插件不得 import kernel/store 直接建 Session；Host 是唯一门。
- [ ] 1.3 定义 tenant 上限与 Bot/WorkItem 只能收窄的 limit 合并规则（depth/children/
      concurrency/budget）。

## 2. 模式注册与装配

- [ ] 2.1 `cmd/keel/serve.go` 构建 ModeRegistry；内置注册 `subagent`，为 `superagent`
      预留注册点；重复/未知 id 启动失败。
- [ ] 2.2 只读模式列表 API/装配投影：id、roles、topology、workUnit、limits、enabled。
- [ ] 2.3 模式停用路径：拒绝新 WorkItem，保留既有 WorkItem/Assignment 可读/可接管。

## 3. 受控 Host

- [ ] 3.1 Host.CreateSession：tenant、`bot.invoke`、permission、budget、depth/width、
      concurrency、取消传播、审计。
- [ ] 3.2 Host.Assign：WorkItem → Bot/Session 绑定；Assignment 生命周期独立于 Session。
- [ ] 3.3 Host.Ask：结构化 Attention；复用 decision 语义；路由与超时。
- [ ] 3.4 负控：插件绕过 Host、跨租户、缺 `bot.invoke`、超预算/深度均拒绝并审计。

## 4. 现有 subagent 适配

- [ ] 4.1 `harnessext.SubagentRunner` 适配为 `subagent` mode 的 Host 使用者，保持现有
      `agent` 工具和 HTTP 响应兼容。
- [ ] 4.2 toolsurface 只解析参数/投影结果，不再自行实现权限/深度判断。
- [ ] 4.3 旧入口与新 Host 入口的一致性测试。

## 5. 验收

- [ ] 5.1 `go test -race` 相关包；`openspec validate --strict keel-collab-core`。
- [ ] 5.2 独立子代理复核契约与依赖方向；P0/P1 清零。
- [ ] 5.3 更新 `docs/HARNESS.md`/`docs/ARCHITECTURE.md` 的协作模式边界，归档本 change。
