# Tasks: keel-collab-superagent

## 1. 领域与存储

- [x] 1.1 定义 Ticket/Assignment/Subscription/Attention/Channel/Source cursor 类型与状态机；
      全部带 tenant_id。（`internal/superagent/domain.go`）
- [x] 1.2 新表与迁移：`collab_work_items`、`collab_assignments`、`collab_subscriptions`、
      `collab_attention`、`collab_source_cursors`（或等价）；幂等键/索引。
      （`internal/store/schema.sql` + `internal/store/superagent.go`；Ticket mandate 存 input JSON，
      不向 sessions/tasks 加 superagent 专属列；`owner_session_id` 为独立列并有 PG 重启用例）
- [x] 1.3 与现有 Bot/Session/decision/notify/task log 的吸附边界；不新增 BotKind。
      （Worker 走 `collab.Host` + `CollabSubagentLauncher`；Attention 复用 `collab.Attention`/
      decision 词汇；审计写 auth_events；无 BotKind=super）
- [x] 1.4 core 边界：superagent 不私补权限。Assign/Ask/CreateSession 只经 Host；不使用
      `HumanTakeover`；operator/takeover 走独立 `OperatorAuthorizer` seam（未装配即 fail closed）；
      accepted 路径审计 fail-closed；Ticket 取消的 session/子树 fan-out 交给 core Cancel。
- [x] 1.5 core P1 静态复核：main 的 core 已有 WorkItem membership、OperatorChecker、
      Cancel session tree 与 CompleteSession；superagent 只调用这些端口，未私补权限。
- [x] 1.6 重启恢复：启动时 `InterruptActiveCollabSessions` 标记上一进程的 active worker；
      `Recover` 以 collab index + WorkerInspector（runner/run 行）判定，旧 Assignment failed
      带 recovery note 并重派；PG 用例断言槽位释放。

## 2. Orchestrator 与 Worker

- [x] 2.1 `superagent` mode 注册：roles=orchestrator/worker；工单看板/装配投影。
      （`cmd/keel/superagent.go` 环境变量启用路径；默认仍 disabled）
- [x] 2.2 Orchestrator：Source 事件 → Trigger/Thread → 评估 accept/attach/stop/ignore；
      accept 创建 Ticket + Mandate；重复 Trigger 幂等。（`seen` 收件箱 + Ticket idempotency key）
- [x] 2.3 WorkerHost：Ticket → Assignment → Worker Session；重启恢复/重派/幂等；
      Worker 可在 Ticket 内使用 subagent mode。（Assignment 独立于 Session；Host 对重复 child
      Put 做同 parent/workitem/bot 的幂等接受；boot 中断旧 worker，open assignment 失败收尾可重试）
- [x] 2.4 并发槽位：tenant 上限 + 人工 follow-up 额外槽位；排队/取消/接管。
      （`Slots` 计数 durable assignments，不是进程内计数；`Reconcile` 把 SessionIndex 的
      done/cancelled 投影回 Assignment 释放槽位；Host `MaxConcurrent` 是硬上限）

## 3. GitHub Source/Channel（MVP）

- [x] 3.1 GitHub Source：Issue/PR/comment 读取、RFC3339 游标、幂等 id、允许列表。
      （CI 状态未实现，保留 `EventCIStatus` 词汇；真实 GitHub E2E 未跑，httptest 覆盖读取/游标）
- [x] 3.2 GitHub Channel：评论回写、消息 id 幂等标记、Outbox 重试、同 thread 保序、
      发送前 marker 去重；GitHub 只能写配置 repo。
- [x] 3.3 订阅/fan-out：Ticket ↔ Thread；人类 follow-up 归属；不重复创建 Ticket。
- [x] 3.4 不可信输入：Source 文本不能改 Policy/授权；审计不含正文/密钥；
      不可信评论不自动 resume blocked Ticket，无 live worker 的恢复只接受允许 actor/follow-up/answer。

## 4. Attention 与 UI

- [x] 4.1 结构化 Attention：问题/来源/选项/路由/过期；答复唤醒同一 Ticket。
      （复用 core `AnswerAttention`；answer 记录 actor、幂等、已答不重开/不重派；停掉的
      Ticket 不重开；重试走 Outbox；Source 评论可匹配唯一 open Attention 作答）
- [x] 4.2 工单看板 API 与最小写入口：待人工/执行中/完成/失败计数、Ticket 详情、
      Assignment、worker report/ask、operator answer/resume/stop。写路由需 `task.write`；
      operator 再走独立 `OperatorAuthorizer`。产品看板已做（web/TUI 同屏同语义：`/superagent`
      路由 + 主导航「任务」、四类计数、Ticket 详情、收件箱、answer/resume/stop；
      `web/src/screens/books/superagent.tsx`、`web/src/data/superagent.ts`）。
- [x] 4.3 Attention 收件箱 API 与来源跳转字段；web/TUI 五态已做（loading/empty/error/denied/
      success，另含只读、写失败与 503 中文化；identity stream 的 `superagent`
      失效事件重读已尝试加载的 board/inbox，含错误态与首屏加载中的补读）。
      `nsh test --target tui --filter 'superagent'` → `17 tests: 17 passed, 0 failed`
      （10 个 `web/src/screens/books/superagent.tui.test.tsx` TUI 用例 + 4 个
      `web/src/data/superagent.test.ts` 用例 + 3 个
      `web/src/data/identity-stream.test.ts` 的 `superagent` 失效事件用例）；全量
      `nsh test --target tui` → `434 tests: 434 passed, 0 failed`。
      （`GET /v1/superagent/attention`）
- [x] 4.4 不提供“新建 Super Agent Bot”入口；只提供 mode/角色配置。
- [x] 4.5 Worker 模型工具适配（`report`/`ask` product extra tool）：
      RunScope → `collab_session_index` → assignment 解析 Ticket，schema 无 ticketId/assignmentId；
      非 worker fail closed，`task.write` + worker-actor 审计，report/ask 均幂等且 requestId
      按 assignment+session 定作用域，waiting 校验 open/归属，输入有硬上限，blocked/closed 拒绝。
      （`internal/toolsurface/superagent.go` + `internal/superagent/worker_tools.go` +
      `cmd/keel/superagent_tools.go`；默认部署仍只有 11 个 builtin tool）

## 5. 验收

- [x] 5.1 真 PG 集成：owner_session 回读、事件幂等、Ticket/Assignment 重启恢复、
      并发槽位、跨租户负控。（`internal/store/superagent_test.go` 使用本地临时 PG 实跑；
      DSN 未提交）
- [x] 5.2 GitHub 真链路 E2E：Issue → Ticket → Worker → PR/评论 → Attention → 完成；
      每步截图 + HTML 报告（公网）。（2026-11-27 实跑：真实 `keel serve` + 临时 PostgreSQL +
      `KEEL_SUPERAGENT_SOURCE=github`，测试仓库
      `Gaubee/keel-test-superagent-e2e-20261126-161045`；worker 模型经
      4.5 的 `report`/`ask` 工具（脚本化 OpenAI provider），由产品 **HTTP API**
      回答 Attention；断言全绿，报告
      https://local-kzf-keel-live-gh.fde2e.com/report.html
      （`scripts/superagent-live-github/`）。实跑发现并修复：operator seam 未装配导致
      产品答复 403、Channel 自身评论被 Source 读回形成回声（owner + 行尾 marker 精确过滤）、
      issue_updated 按持久化「已投递标题/正文」快照去重（重启后仍成立）；三轮复审又收紧：
      标题-only 编辑把新标题写入 follow-up 并更新工单标题、派工失败/未派工不得推进快照
      （回滚 resumed 状态并让游标重试）、NoopInputSink 降级不推进、issue 正文编辑不再被
      误当自由文本答复、issue/comments 列表跟随 Link 分页（单页 >per_page 不再截断）。
      报告绑定 `git rev-parse HEAD` + 二进制 sha256，且只在 exit=0 时发布。
      未覆盖：PR 线程、真实 LLM 判断质量、第二个人类账号、App token——见报告「缺口与风险」。）
- [x] 5.3 独立 reviewer 复核；P0/P1 清零；`openspec validate --strict`；归档。
      （产品 UI 与 Worker 工具分别经独立 reviewer 复核 PASS，P0/P1=0；strict 通过。
      5.2 的外部凭据阻塞已于 2026-11-27 实跑解除；多轮复审的 P1/P2（快照基准、
      派工失败回滚、marker 过滤范围、标题投递、分页、报告版本绑定）已修复并补测。）
