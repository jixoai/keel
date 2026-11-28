# Proposal: keel-pi-parity-closure-2

## Why

`keel-ecosystem` 已冻结；parity 审计后仍有约 6 个真实能力缺口。它们不应通过解冻旧板或勾旧任务来“完成”，而应以新 change 交付：CompactionTask/checkpoint 语义、skills allowed-tools 执行期交集、MCP 连接池/健康/破坏性审批、usage ledger/observed stream、nightly chaos/conformance golden、最终容量/文档。

## What Changes

- 新建 PI parity closure 能力，不恢复旧 `keel-ecosystem` 任务板。
- 按六项能力逐项实现并验收；每项有独立测试/证据。
- 不重做已由 hooks/span/OTLP/真实 MCP E2E 交付的部分。
- 保持 keel 内核/宿主边界；不引入新的运行时依赖，除非 spec 明确。
