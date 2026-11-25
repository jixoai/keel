# Tasks: keel-checkpoint

## 1. 领域与策略

- [ ] 1.1 定义 CheckpointPolicy（enabled/threshold/retain/max/TTL/manual）与 tenant→session
      收窄规则。
- [ ] 1.2 检查现有 compaction/checkpoint 实现，补齐 cutSeq、tokensBefore/After、保留段、
      policyVersion、幂等键；不重复造第二套 checkpoint。
- [ ] 1.3 切点安全：不在 tool 中间、不压缩最后一条 user；失败/取消保留原上下文。

## 2. 恢复与保留

- [ ] 2.1 checkpoint + tail 恢复路径；损坏/缺失 fail closed，回退全量事件重建。
- [ ] 2.2 重启/handoff/Worker 恢复统一走同一路径；真 PG 测试。
- [ ] 2.3 MaxCheckpoints/TTL 清理只删投影；最近有效 checkpoint 保护；事件不删。
- [ ] 2.4 指标/日志：创建/恢复/失败/清理、tokens、耗时、cutSeq；脱敏。

## 3. 手动压缩与 API/UI

- [ ] 3.1 `POST /v1/sessions/{id}/compact`：权限、幂等 requestId、返回值、审计。
- [ ] 3.2 设置页展示策略与最近 checkpoint（只读）；README/RUNBOOK 更新。
- [ ] 3.3 自动/手动压缩共用切点/失败语义；重复请求不重复 checkpoint。

## 4. 验收

- [ ] 4.1 测试：阈值/保留/失败/取消/重启/损坏/权限/租户/TTL/最大数。
- [ ] 4.2 `go test -race` 相关包 + web/TUI 回归；`openspec validate --strict`。
- [ ] 4.3 独立 reviewer 复核；E2E 报告（压缩前后、恢复、失败态）；归档。
