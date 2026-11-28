# Tasks: feat-keel-collab-p2

## 1. P2-4 Assignment 跨 WorkItem 重绑与错误分类

- [ ] 1.1 `WorkStore` 增加 `ListAssignmentsBySession`；MemoryWorkStore 与 PG
      (`internal/store/superagent.go`) 实现同租户查询。
- [ ] 1.2 `Assign` 增加 index + 全局 assignment 扫描：已绑定其他 WorkItem 的 session
      返回 `session_already_assigned`；同 WorkItem 重复绑定返回
      `assignment_already_bound`；终态 assignment 复用返回 `assignment_terminal`。
- [ ] 1.3 幂等键复用参数（Bot/session）不一致返回 `assignment_conflict`；
      `CreateSession`/`finishCreation` 的并发绑定冲突改用新错误码。
- [ ] 1.4 补测试：跨 WorkItem 重绑、index 未带 WorkItem 的已绑定 session、终态复用、
      幂等参数冲突、并发绑定、租户隔离；`-race`。

## 2. P2-5 Link 端点完整性、ID 去重与跨 WorkItem 关系

- [ ] 2.1 `AddLink` 校验 session/work_item/assignment 端点在同租户存在；拒绝自环和
      非闭集端点。
- [ ] 2.2 无显式 ID 时生成确定性 link ID；Memory/PG store 以自然键去重，重复关系
      返回原记录；显式 ID 内容冲突返回 `link_conflict`。
- [ ] 2.3 `ListLinks` 同时返回 scope 等于该 WorkItem 与端点为该 WorkItem 的关系，
      使跨 WorkItem 链接从两端可读；PG schema 增加自然键唯一索引。
- [ ] 2.4 补测试：不存在/跨租户端点、重复 AddLink 幂等、显式 ID 冲突、A↔B 双端读取、
      memory/PG parity。

## 3. P2-6 per-request tenant registry

- [ ] 3.1 新增 `ModeRegistrySource` / `TenantRegistries`：按请求租户懒构建并缓存，
      校验 factory 返回的 tenant 一致，未知租户返回 `tenant_unknown`。
- [ ] 3.2 `HostOptions` 支持 `Registries`（与静态 `Modes` 二选一）；Host 所有入口
      按请求 tenant 解析 registry；审计回退 tenant 使用请求 tenant。
- [ ] 3.3 更新 production 装配：`newCollabModes` 作为 factory，`newCollabHost` 注入
      `TenantRegistries`；superagent/capabilities 解析部署租户 registry。
- [ ] 3.4 补测试：两个租户各自 registry、未知租户拒绝、缓存同一对象、factory 错误、
      静态模式回归、并发解析 `-race`。

## 4. Checkpoint summary hash

- [ ] 4.1 `Checkpoint`/`CheckpointRecord`（kernel 与 store）增加 `SummaryHash`；
      schema 增加 `summary_hash`；Save 时计算/校验，Load 返回。
- [ ] 4.2 automatic/manual compaction 事件 payload 写 `summaryHash`；恢复校验行内
      哈希与事件哈希，不一致回退全量事件并计 `corrupt`。
- [ ] 4.3 补测试：正常哈希、行内篡改、事件/行不一致、旧空哈希跳过、mem/PG parity。

## 5. requestId 去重独立于 prune

- [ ] 5.1 schema 新增 `checkpoint_requests` 表；kernel PG/Mem 与 store PG 在保存
      manual checkpoint 时写幂等表，`CheckpointByRequest` 从幂等表读取。
- [ ] 5.2 prune 只删 `checkpoints`，不删 `checkpoint_requests`；同 requestId 不同
      upto_seq 仍拒绝。
- [ ] 5.3 补测试：投影被 max/TTL prune 后 requestId 仍命中、重复 manual compact 不再
      调模型、mem/PG parity。

## 6. TTL 惰性清理

- [ ] 6.1 Run 的恢复读取路径在构造 model view 前按生效策略惰性执行
      `PruneCheckpoints`（TTL/Max），失败 best-effort。
- [ ] 6.2 补测试：只有 TTL 配置、无新 checkpoint 写入时，过期旧投影在下次 Run 被清理；
      最新 checkpoint 保留；事件与幂等表保留。

## 7. 验收

- [ ] 7.1 `go test -race ./internal/collab/...`、`go test -race ./internal/kernel/...`、
      `go test -race ./internal/agent/...`、`go test ./cmd/keel/...`（PG 用例按
      `KEEL_TEST_DSN` 门控；未设置时记录 BLOCKED）。
- [ ] 7.2 `openspec validate --strict feat-keel-collab-p2`。
- [ ] 7.3 提交 change + 代码，push 分支；报告 commit、测试与剩余 BLOCKED。
