# Proposal: keel-collab-p2

## Why

协作五线已归档，但 core 留了 3 条 P2、checkpoint 留了 1 条 P2：Assignment 跨 WorkItem 重绑/错误分类、Link 端点类型与跨 WorkItem 关系、per-request tenant registry、checkpoint summary hash/requestId prune/TTL。它们不影响主流程，但属于已登记的边界缺口。

## What Changes

- `collaboration-core`：Assignment 重绑路径与 typed error；Link 端点类型/去重/跨 WorkItem 关系；per-request tenant registry。
- `checkpoint-policy`：summary hash；requestId 去重表独立于 prune；TTL 惰性清理。
- 只修这 4 条；不重开已归档 change 的其他任务。
