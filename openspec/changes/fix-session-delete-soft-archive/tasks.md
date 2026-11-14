# Tasks: fix-session-delete-soft-archive

## 实现（2026-11-14 完成）

- [x] schema：`internal/store/schema.sql` 加 `sessions.archived_at`（幂等 `ALTER ... IF NOT EXISTS`，
      紧邻 sessions 既有 ALTER 段）
- [x] store：`internal/store/store.go` 加 `ArchiveSession`（UPDATE 置 `archived_at`；
      已归档/存在性复核，重复删除报 true）
- [x] agentapi：`Config.ArchiveSession` seam + `DELETE /v1/sessions/{id}` 路由（`api.go`）
      + handler（`sessions.go`：503/404/200，幂等）
- [x] 列表：`internal/agentapi/sessions_pg.go` 的 `sessionSelect` 加 `AND s.archived_at IS NULL`
- [x] 装配：`cmd/keel/serve.go` 产品 Server 装配处接 `ArchiveSession: st.ArchiveSession`

## 测试

- [x] `internal/agentapi/sessions_test.go`：DELETE 200 + archived:true；重复 200；
      未知 id → 404；seam nil → 503（fake seam）
- [x] `internal/store/session_archive_test.go`：真 PG round trip（标记置位、账本行保留、
      跨租户/不存在 found=false、幂等）

## 验收（真库 E2E，2026-11-14，13760）

- [x] 真库：4 个探针会话 DELETE → 200 archived:true；`GET /v1/sessions` 41→37、探针 0 残留；
      归档会话 `GET …/events` 仍可读；重复 DELETE → 200；未知 id → 404
- [x] **UI 全路径**（浏览器 1440×900，真库）：行悬停 → 会话操作菜单 → 删除会话 → 确认框
      「删除会话？」→ 删除 → 行消失、无失败提示（列表 37→36）——原「UI 删除恒 404」在用户视角闭环
- [x] `go build ./...` OK；`gofmt -l` 空；`go test ./internal/store/... ./internal/agentapi/... ./cmd/keel/...` 全绿
- [x] web 侧无需改动（客户端调用形态本已正确）；TUI 套件随本批提交前统一复跑 106/106
