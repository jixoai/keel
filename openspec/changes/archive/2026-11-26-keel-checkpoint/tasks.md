# Tasks: keel-checkpoint

## 1. 领域与策略

- [x] 1.1 定义 CheckpointPolicy（enabled/threshold/retain/max/TTL/manual）与 tenant→session
      收窄规则（`internal/compaction/policy.go`，含单测）。
- [x] 1.2 检查现有 compaction/checkpoint 实现，补齐 cutSeq、tokensBefore/After、保留段、
      policyVersion、幂等键；不重复造第二套 checkpoint（复用 compaction 事件 + checkpoints
      投影，新增列与 recordId 幂等索引）。
- [x] 1.3 切点安全：不在 tool 中间、不压缩最后一条 model 尚未看到的 user；失败/取消保留原上下文。

## 2. 恢复与保留

- [x] 2.1 checkpoint + tail 恢复路径；损坏/缺失 fail closed，回退全量事件重建
      （`modelView` 结构校验 + `corruptCheckpointReason`）。
- [x] 2.2 重启/handoff/Worker 恢复统一走 `Runner.modelView`；PG 重启与 handoff
      （新 owner/新 epoch）恢复测试已覆盖。
- [x] 2.3 MaxCheckpoints/TTL 清理只删投影；最近有效 checkpoint 保护；事件不删
      （kernel/store/MemStore/PGStore `PruneCheckpoints`）。
- [x] 2.4 日志与指标：checkpoint 创建/恢复/失败/清理/手动压缩均为结构化日志（只含
      ids/tokens/cutSeq/policyVersion，不含正文）；固定基数指标
      `keel_checkpoint_created_total`/`_recovered_total`/`_failed_total{reason}`/
      `_pruned_total`。

## 3. 手动压缩与 API/UI

- [x] 3.1 `POST /v1/sessions/{id}/compact`：路由 + `session.write` 权限 + requestId 幂等 +
      返回值 + 脱敏审计日志 + handler/适配器测试；生产 `cmd/keel/serve.go` 已接线
      （run lease 认领/续租/settle，busy 409、quota 429、disabled 409）。
- [x] 3.2 session 级策略存储与 API：`session_checkpoint_policies` 表 +
      `GET/PUT /v1/sessions/{id}/checkpoint-policy`（tenant/session/effective/narrowed
      只读投影）；runner 每 attempt 通过 resolver 读取生效策略。设置页 UI 未接。
- [x] 3.3 自动/手动压缩共用切点/失败语义；重复 requestId 不重复 checkpoint（投影
      request_id 唯一 + 幂等回放测试）。

## 4. 验收

- [x] 4.1 测试：阈值/保留/失败/取消/重启/handoff/损坏/权限/租户/TTL/最大数/指标基数。
- [x] 4.2 `go test -race` 相关包（compaction/kernel/agent/harnessapi/store/
      sessionpolicy/metrics/loop/host/servicehost/accessgate/cmd-keel）+
      `openspec validate --strict`。
- [x] 4.3 独立 reviewer 复核；E2E 报告（压缩前后、恢复、失败态）；设置页 UI；归档。（12 步 / 66 断言；kill -9 恢复与 exact-prefix；复核 PASS）

## 5. 独立复核修复（No-Go 回合）

- [x] P0 未读输入：手动压缩复用 `unreadInputs` + `presentUnreadInputLast`；切点进入未读块
      或覆盖未读 seq 时拒绝（409 conflict）；锚点改由 `checkpointAnchor` 按日志顺序解析；
      恢复时 `unreadBelowCheckpoint` fail closed。测试覆盖 retain 0、默认 retain、损坏锚点。
- [x] P0 模型解析：手动压缩与自动 run 共用 `resolveSurface`/`activeModelFor`，使用解析后的
      stream/provider/key/thinking/contextWindow/request patch；resolver 失败 fail closed。
- [x] P1 悬空 tool call：手动压缩先查 `UnresolvedToolCalls`，有未闭合调用返回 conflict。
- [x] P1 `enabled=false` 不再关闭手动压缩；补完整策略矩阵（enabled × manual）。
- [x] P1 tenant `compaction.enabled` 接入 `settings.Enabled` 与策略投影。
- [x] P1 fork 前缀：`checkpointAnchor` 减去 prefix 长度，切点落在前缀内返回 0；
      手动压缩在前缀内/无法锚定时拒绝。补 fork compaction + restart 测试。
- [x] P2（已做）：retainTokens 无条件应用；`LoadCheckpoint` 跳过未锚定行；prune 只保护
      最新已锚定行；readiness 校验 checkpoint 新列；TTL 上界；手动读取不计 recovered；
      Access 未接线 fail closed；策略读取失败计 `policy` 指标；每 attempt 策略只解析一次。
- [ ] P2（未做）：checkpoint summary hash；requestId 去重表独立于 prune；TTL 惰性清理；
      手动压缩走 hygiene/hooks；手动 claim 不消耗 attempts。
