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
- [x] 2.2 重启/Worker 恢复统一走 `Runner.modelView`；PG 重启恢复测试已覆盖。
      handoff 复用同一路径，尚无专项 E2E。
- [x] 2.3 MaxCheckpoints/TTL 清理只删投影；最近有效 checkpoint 保护；事件不删
      （kernel/store/MemStore/PGStore `PruneCheckpoints`）。
- [x] 2.4 日志：checkpoint 创建/恢复失败/清理/手动压缩均为结构化日志，只含 ids/tokens/
      cutSeq/policyVersion，不含正文。未新增 checkpoint 专属指标（复用
      `keel_compactions_total`）。

## 3. 手动压缩与 API/UI

- [x] 3.1 `POST /v1/sessions/{id}/compact`：路由 + `session.write` 权限 + requestId 幂等 +
      返回值 + 脱敏审计日志 + handler 测试；`Runner.CompactNow` 内部 seam + PG 测试。
      **生产 `cmd/keel/serve.go` 尚未注入 `CompactSession`，当前返回 501 not_wired。**
- [ ] 3.2 设置页展示策略与最近 checkpoint（只读）；README/RUNBOOK 更新（RUNBOOK 已完成，
      设置页未接）。
- [x] 3.3 自动/手动压缩共用切点/失败语义；重复 requestId 不重复 checkpoint（投影
      request_id 唯一 + 幂等回放测试）。

## 4. 验收

- [x] 4.1 测试：阈值/保留/失败/取消/重启/损坏/权限/租户/TTL/最大数。
- [x] 4.2 `go test -race` 相关包（compaction/kernel/agent/harnessapi/store/loop/host/
      servicehost/accessgate）+ `openspec validate --strict`。
- [ ] 4.3 独立 reviewer 复核；E2E 报告（压缩前后、恢复、失败态）；归档。
