# Design: keel-checkpoint

## 1. 真值与投影

```text
events (append-only, truth)
  └─ compaction event + checkpoint projection
       ├─ cutSeq / cutEventId
       ├─ tokensBefore / tokensAfter / retainedTail
       ├─ summary / summaryHash
       └─ policyVersion / createdAt
```

checkpoint 不是删除历史；它只是让模型上下文从“checkpoint + tail”重建。任何恢复都必须
能回放到同一事件 seq。

## 2. 生成流程

```text
上下文超过 threshold
  → 计算安全切点（不在 tool 中间，不切最后一条 user）
  → 请求摘要
  → 成功：追加 compaction 事件 + checkpoint（同一事务/幂等键）
  → 失败/取消：保留原上下文，记录失败，不落半成品
```

## 3. 策略

```go
type CheckpointPolicy struct {
    Enabled          bool
    ThresholdTokens  int
    RetainTokens     int
    MaxCheckpoints   int
    TTL              time.Duration
    ManualCompact    bool
}
```

- tenant 上限 + session 只能收窄；
- `MaxCheckpoints` 到期清理只删 checkpoint 投影，不删事件；
- TTL 清理不影响当前会话恢复（最近一个 checkpoint 必须保留，直到被新 checkpoint 替代）。

## 4. 恢复

- 读取最近有效 checkpoint，校验 cutSeq/摘要/保留段引用；
- 从 checkpoint + tail 重建上下文；
- checkpoint 缺失/损坏：fail closed，回退到从事件全量重建（记录告警）；
- 重启、handoff、worker 恢复使用同一路径。

## 5. 手动压缩

`POST /v1/sessions/{id}/compact`：

- 需要 session 写权限；
- 幂等 requestId；
- 与自动压缩共用切点/失败语义；
- 返回 checkpoint id、tokensBefore/After、是否实际执行；
- 审计不含正文。

## 6. 可观测

- 指标：checkpoint 创建/恢复/失败/清理次数、压缩后 token、耗时；
- 日志：session/tenant/run id、cutSeq、policyVersion；不含消息正文/摘要正文。
