# Tasks: keel-collab-p2

## 1. Core P2

- [ ] Assignment 跨 WorkItem 重绑路径、错误分类、原 assignment 不变测试
- [ ] Link 端点类型/去重/跨 WorkItem 关系测试
- [ ] per-request tenant registry；并发双租户测试

## 2. Checkpoint P2

- [ ] checkpoint summary hash 规范化
- [ ] requestId 去重表独立于 prune
- [ ] TTL 惰性清理与边界测试

## 3. Gates

- [ ] 定向 Go/PG 测试 + `-race`
- [ ] 独立子代理复审 P0/P1=0
- [ ] 更新归档 change 的 disposition；`openspec archive keel-collab-p2 --yes`
