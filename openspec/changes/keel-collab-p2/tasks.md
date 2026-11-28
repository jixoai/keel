# Tasks: keel-collab-p2

## 1. Core P2

- [x] Assignment 跨 WorkItem 重绑路径、错误分类、原 assignment 不变测试
- [x] Link 端点类型/去重/跨 WorkItem 关系测试
- [x] per-request tenant registry；并发双租户测试

## 2. Checkpoint P2

- [x] checkpoint summary hash 规范化
- [x] requestId 去重表独立于 prune
- [x] TTL 惰性清理与边界测试

## 3. Gates

- [x] 定向 Go/PG 测试 + `-race`
- [x] 独立子代理复审 P0/P1=0（PASS，final HEAD）
- [ ] 更新归档 change 的 disposition；`openspec archive keel-collab-p2 --yes`

> Evidence: `evidence/execution-2026-11-28.md` (commands, PG rows, mem/PG
> parity, remaining review/archive intentionally deferred).
