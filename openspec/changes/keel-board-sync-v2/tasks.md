# Tasks: keel-board-sync-v2

## 0. Storage dialect boundary

- [ ] 定义 keel-board append log/store/sync/audit 接口与 dialect-neutral 类型
- [ ] 将 pgx/SQL/事务/迁移/公告锁/LISTEN-NOTIFY 收进 PG adapter
- [ ] 加 dialect capability 声明；不支持能力显式返回 typed error
- [ ] 加 storage conformance suite；PG adapter 全绿
- [ ] `go list -deps` 证明 domain/server/sync 不直接依赖 pgx

## 1. Board sync API v0.1.2

- [ ] keel-board 增加 targets / oldestPendingAgeSeconds / failures24h 只读字段
- [ ] 协议/服务端/store 测试；缺字段保持 null，不伪造 0
- [ ] 打 keel-board v0.1.2 tag 并推 origin
- [ ] keel `go.mod` 升级到 v0.1.2；mount DTO/权限映射
- [ ] keel mount contract tests + PG integration

## 2. UI

- [ ] sync 页显示真实 targets/lag/24h；缺失只留一句弱提示
- [ ] 390 选中 tab 自动滚入；筛选 sheet body 可滚、footer 可换行
- [ ] task 页 provider 错误可展开；移动端复制链接可达
- [ ] board P2：watcher dead/lastError/depth.dead、automation 下次日期/危险权重、去 meta 文案
- [ ] board TUI/UI 证据更新；390/1440 截图

## 3. Gates and archive

- [ ] `nsh check`、board TUI、UI 证据 53/26、file coverage
- [ ] Go `-p 1 ./...` + race 定向
- [ ] 独立子代理复审 P0/P1=0
- [ ] 更新 DISPOSITION/README/RUNBOOK；`openspec archive keel-board-sync-v2 --yes`
- [ ] 发 v0.6.0（与 release closure 协调）
