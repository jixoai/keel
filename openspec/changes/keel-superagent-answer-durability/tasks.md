# Tasks: keel-superagent-answer-durability

## 1. 持久化答案协议

- [x] 1.1 Ticket 文档新增 `answers []AnswerReceipt` 与 pending 的
      `trusted/allowDispatch/optionId/answerText/answerActor`；
      `LastKnownThread` 以最后一个未失败 snapshot 为比较点。
      （`internal/superagent/domain.go`）
- [x] 1.2 答案写序改为 intent → Host.AnswerAttention → deliver → receipt；
      去掉了投递前写 `accepted` 审计。（`internal/superagent/orchestrator.go`
      `answerAttentionLocked`/`deliverPendingLocked`）
- [x] 1.3 重试/重启自愈：`retryAnsweredAttentionLocked` +
      `reconcileAnswerDeliveriesLocked`（Recover 每次启动扫描，`ListAttentionsByStatus`
      最新 200 条）。（`internal/superagent/orchestrator.go`、`internal/store/superagent.go`）
- [x] 1.4 故障注入用例：intent 写失败 Attention 保持 open；answered 无证据重试必须
      投递；关闭工单显式 `ErrAnswerNotDelivered`；重启后 pending 继续投递；
      housekeeping 先补写「intent 存在但 Attention 仍 open」的缺失决策再投递；
      被他人改答的 stale intent 不投递且标 Failed。
      （`internal/superagent/answer_state_test.go`、`answer_state_pg_test.go`）

## 2. 重试策略与人工出口

- [x] 2.1 只对永久错误标 Failed；临时错误无限重试、退避 30s 封顶。
      （`pendingPermanentError`/`pendingBackoff`）
- [x] 2.2 Board 增加 `pendingCount/failedPendingCount`，保留完整 pending 行；
      `RetryPending`/`DismissPending`（终态 retry 409、dismiss 可结算）；审计在动作后写
      `accepted`/`failed`。（`internal/superagent/orchestrator.go`、
      `internal/harnessapi/superagent.go`）
- [x] 2.3 第 11 次 housekeeping 仍投递的回归用例。
      （`TestTransientDispatchFailureStillDeliversAfterTenRetries`）

## 3. 信任与状态

- [x] 3.1 capture 时持久化 Trusted/AllowDispatch；无 live assignment 只有
      answer/trusted follow-up 可派工。
- [x] 3.2 blocked 工单只允许 answer/trusted follow-up 解锁；不可信 context 保持
      blocked 且 Attention 仍 open；不可信不阻塞其后的 answer。
- [x] 3.3 用例：live worker + 不可信评论 + 投递失败 + worker 结束；blocked +
      不可信评论。（`TestUntrustedContextNeverStartsAWorker`、
      `TestUntrustedCommentDoesNotUnblockABlockedTicket`）

## 4. API / SSE / Web

- [x] 4.1 answer 202/409/503 语义与 `attention/deliveryPending/deliveryFailed` 载荷；
      retry/dismiss 路由（`internal/harnessapi/superagent.go`、
      `internal/harnessapi/superagent_test.go`）。
- [x] 4.2 记录答复（含 pending/failed）后 `o.changed`。（`AnswerAttention`）
- [x] 4.3 Web：解析 202/409、提示「已记录、等待投递/投递失败」、工单详情
      待投递输入区 + 重试/清除；fixture/harness 数据、data 测试与 TUI 用例。
      （`web/src/data/{contract,http,superagent,fixture}.ts`、
      `web/src/screens/books/superagent.tsx`、`superagent.tui.test.tsx`）

## 5. P2 收口

- [x] 5.1 同内容 pending snapshot 去重（comment bump 不重复捕获/投递）。
- [x] 5.2 PG advisory lock 单写者 lease；`cmd/keel` 在 `InterruptActiveCollabSessions`
      之前获取，第二副本 fail closed；Store 无 lease 能力时 `Run` 也 fail closed。
      （`internal/store/superagent.go`、`cmd/keel/superagent.go`）
- [x] 5.3 answer 评论 outbox 前置入队（幂等键）；启动扫描补齐 `commentQueued=false`
      的 receipt。（`ensureAnswerComment`、`enqueueAnswerComment`）
- [x] 5.4 审计只在投递成功后写 accepted；冲突/失败不留误导行。

## 6. 门禁与证据

- [x] 6.1 `gofmt -l` 空、`go vet`；`go test ./internal/superagent ./cmd/keel
      ./internal/collab ./internal/harnessapi`；`go test -race ./internal/superagent`。
- [x] 6.2 真 PG（KEEL_TEST_DSN 非 skip）：`TestAnswerProtocolIsDurableInPostgres`、
      `TestWriterLeaseRejectsASecondProcess` 以及既有 PG 用例。
- [x] 6.3 `nsh check`；`nsh test --target tui` 全量实际结果逐次记录（含宿主 flake，
      不以一次“全绿”作为结论），`--filter superagent` 20/20。
- [x] 6.4 `openspec validate --all`。
- [x] 6.5 live GitHub E2E（`scripts/superagent-live-github/run.sh`，只写 `keel-test-*`），
      报告绑定最终 commit/bin sha 并发布到
      `https://local-kzf-keel-live-gh.fde2e.com/report.html`。第五轮重修后重跑并重发；
      其回归清单含停止/终态边界、扫描 re-arm 与租约健康用例。

## 7. 第五轮复审（停止/终态硬门与租约健康）

- [x] 7.1 `ticketDeliveryClosed`（revoked / cancelling / cancelled / done / failed）硬门：
      `deliverPendingLocked`、`attemptPendingDeliveryLocked`、`answerAttentionLocked`、
      `retryAnsweredAttentionLocked`、`RetryPending` 在投递/重试前拒绝（HTTP 409），
      不投递、不清 pending、不写 attempt/Failed；`DismissPending` 保留为终态结算入口。
      UI 在停止/终态禁用 retry、保留 dismiss。
      （`internal/superagent/orchestrator.go`、`web/src/screens/books/superagent.tsx`）
- [x] 7.2 回归：cancelling+live、cancelled、done、failed 四组，answer/follow-up 各一条，
      断言零投递/零派工、pending 未变、retry 409、dismiss 成功。
      （`TestClosedTicketNeverDeliversPendingInput`、`TestSuperagentClosedTicketRetryIs409`、
      `TestSuperagentClosedTicketAnswerIs409`）
- [x] 7.3 启动补偿失败自愈：`noteAnswerRepairFailure` 保持 `answerRepair` 并按 1s→30s
      退避；`Recover` 的扫描错误不再中止恢复；housekeeping 到期重跑。
      （`TestAnswerRepairScanFailureIsRetriedByHousekeeping`）
- [x] 7.4 临时 Host 写失败：intent 已持久化 → 返回 `ErrAnswerDeliveryPending`（202
      `deliveryPending`）、触发 `o.changed`、pending pump 补写 decision 后投递；永久拒绝
      才移除 intent 返回 4xx。（`TestTransientAnswerWriteIsRecordedPending`）
- [x] 7.5 WriterLease `Healthy`（PG `SELECT 1` 复核会话）：Run 在 Recover 前与每轮 poll
      前校验，失联即停写退出；真 PG 用 `pg_terminate_backend` 杀租约后端，断言 Healthy
      变红、第二进程可在锁释放后接管。（`TestRunStopsWhenTheWriterLeaseIsUnhealthy`、
      `TestWriterLeaseHealthDetectsATerminatedBackend`）
- [x] 7.6 `MandateObserve` 禁止 dispatch（含 human resume）；Bot principal 不可达
      `/v1/superagent/*`；不可信 context 无 live 时保留等待；`ListAttentionsByStatus`
      增加 mode 过滤与 `collab_attentions_status_recovery` 索引。
      （`TestObserveMandateCannotBeResumedForDispatch`、
      `TestSuperagentBotPrincipalCannotUseTheOperatorAPI`、
      `TestUntrustedContextWithoutALiveWorkerIsKept`、`internal/store/schema.sql`）
- [x] 7.7 终态 pending 结算：retry 明确 409，dismiss 200；`RetryPending`/`DismissPending`
      审计结果与动作一致（accepted/failed）。（`TestPendingAuditRecordsTheOutcome`）
