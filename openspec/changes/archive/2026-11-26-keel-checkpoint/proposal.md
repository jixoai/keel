# Proposal: keel-checkpoint

> 立项：2026-11-25。来源：Bot/协作模式讨论中确认 checkpoint 是必须能力。
> 定位：checkpoint 是长对话/压缩的“可恢复存档点策略”，不是文件备份，也不是新的会话真值。

## Why

现有压缩路径已有 checkpoint 语义，但缺少独立、可配置的策略：何时落 checkpoint、保留
多少尾部、最多保留几个、何时过期、如何从 checkpoint + tail 恢复、如何手动压缩。长会话和
super-agent worker 都会反复压缩；如果策略散落在压缩实现里，容易出现切点不安全、恢复不一致、
存储无界或历史不可读。

本 change 把 checkpoint 做成明确的产品能力：事件日志仍是唯一真值，checkpoint 只是投影；
压缩历史仍可读；恢复必须可从 durable checkpoint + tail 重建。

## What Changes

1. **Checkpoint 生成**：在压缩切点生成 durable checkpoint，记录 cut event seq、
   tokensBefore/After、摘要、保留段引用、配置版本；摘要失败保留原上下文，不落半成品。
2. **切点安全**：不在工具调用中间切、不压缩最后一条 user；压缩失败/取消不改变当前上下文。
3. **恢复**：从 checkpoint + tail 重建模型上下文；历史事件只追加、不删除、仍可读；
   重启后恢复同一结果。
4. **策略配置**：按 tenant/session 配置阈值、保留 token、最大 checkpoint 数、TTL/
   retention；只能收窄 tenant 上限，不能突破。
5. **手动压缩**：`POST /v1/sessions/{id}/compact`（幂等/可审计），与自动压缩共用同一策略。
6. **可观测**：checkpoint 创建/恢复/失败/清理的结构化日志与指标；不含正文/密钥。
7. **UI**：设置页展示当前策略与最近 checkpoint（只读）；不把 checkpoint 当成可编辑消息。

## Capabilities

### New Capabilities

- `checkpoint-policy`: checkpoint 的生成、切点安全、恢复、保留/过期和手动压缩。

### Modified Capabilities

<!-- 若实现需要修改 agent-execution/agent-loop-contract 的既有 requirement，
     实现阶段补精确 delta；本提案不虚构修改。 -->

## Impact

- `internal/compaction`、`internal/agent`/`internal/loop`、store/checkpoint 表/读取路径；
- `cmd/keel`/harness API 手动压缩路由；设置页/README/RUNBOOK；
- 不改变事件日志的单一真值；不引入第二套消息存储。
- 测试：压缩阈值、切点安全、摘要失败、重启恢复、retention、手动压缩、租户隔离。

## Out of scope

- 完整 spill/compaction 算法重写；prompt cache 策略。
- 跨会话共享 checkpoint、云端备份。
- 把 checkpoint 暴露成可编辑的聊天记录。
