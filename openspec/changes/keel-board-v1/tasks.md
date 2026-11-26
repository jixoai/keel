# Tasks: keel-board-v1

> 阶段门禁：每个 phase 结束 MUST 跑该阶段的单测/集成/E2E，并把证据写到
> `openspec/changes/keel-board-v1/evidence/<phase>-*.md`（脚本生成，禁手写）；独立子代理复核
> （禁 Codex）P0/P1 清零后才进入下一 phase。Phase 1/2 的证据归档是**阶段检查点**；整个
> change 的 `openspec archive` 只在 Phase 3 收口执行。

## 1. Phase 1：契约与存储

- [ ] 1.1 新建 `internal/board` 领域包：Channel/Message/Thread/Task/Claim/Needs/Label 类型、事件 kind 闭集、错误码常量；`go test ./internal/board` 覆盖类型与错误码映射。
- [ ] 1.2 实现七态状态机表与合法迁移校验（含终态、blocked、release/expire、reopen）；用表驱动测试覆盖每条合法边与非法边。
- [ ] 1.3 实现 token（32 字节 base64url / SHA-256 / 常量时间比较）、cursor、标签文法、needs 上限的纯函数校验；单测覆盖边界值与非法输入。
- [ ] 1.4 增加 `board_heads/board_events/board_requests/board_channels/board_messages/board_threads/board_tasks/board_task_needs/board_labels` schema、索引与 RLS 条目；`go test ./internal/store -run Board` 验证迁移幂等与 `rlsTenantTables` 覆盖。
- [ ] 1.5 实现 `Append` 引擎：租户头行锁、seq 分配、事件插入、纯 fold、投影更新同事务；属性测试验证增量 fold 与全量 replay 的投影逐字段一致。
- [ ] 1.6 实现幂等缓存：请求 digest、同 key 同结果返回、不同 digest 409 `duplicate_id`、result 脱敏；单测覆盖创建/claim/complete 的重试。
- [ ] 1.7 实现频道与消息的最小投影：首帖建频道、频道幂等创建、根/回复线程、消息版本/修订、频道计数；集成测试覆盖读回与排序。

## 2. Phase 1：任务、claim 与依赖

- [ ] 2.1 实现任务创建（channel 新线程 / re 既有线程）与 owner/by/for 规则；测试覆盖 owner Account、owner Bot、其他 Bot 的差异。
- [ ] 2.2 实现 `claim-ready` 选择顺序（assigned-to-caller → owned pool → `from` pool、oldest first）与 `waitSeconds` 上限；测试覆盖空队列、超时、过滤、子树频道。
- [ ] 2.3 实现 claim/renew/release 的 epoch/token 围栏与租约计算（默认 900s、30–3600s、tenant cap）；PG 集成测试覆盖并发 claim 只有一个成功。
- [ ] 2.4 实现 complete/fail/block/unblock/reopen/assign/needs/cancel 的守卫与事件；表驱动测试覆盖 no-op（unblock 非 blocked、reopen 非 failed）与终态拒绝。
- [ ] 2.5 实现租约 lazy expiry、可选 sweeper（15s）、读修复（ready 补齐）；用可注入时钟测试断言旧 claim 写失败、新 claim 成功。
- [ ] 2.6 实现 needs 校验（存在/可读/非 canceled/非自环/环检测/500 上限）与完成传播（同事务 ready 直接依赖者）；测试覆盖 20 上限、环、超深、失败/取消前置。
- [ ] 2.7 实现 `board.*` 保留标签拒绝、`board.task` 状态标签投影、按根标签过滤回复；测试覆盖客户端写保留标签被拒且无事件。
- [ ] 2.8 写真实 PG 并发测试：两个连接同时 claim、租约过期后交叉 complete、幂等重试、needs 完成传播；`go test -race` 通过。

## 3. Phase 1：API、权限与 SSE

- [ ] 3.1 增加 `board.read/write/manage` 权限键与默认 role/group 映射；测试覆盖无权限、只读、写、管理四种调用者。
- [ ] 3.2 实现频道关系解析：精确路径优先、最近祖先继承、`viewer/editor/owner` 蕴含；PG 测试覆盖 `work` 上的 editor 对 `work/production` 生效、`work/secret` 收窄。
- [ ] 3.3 实现 `GET /v1/board/me`、`GET/POST /channels`、`GET/POST /messages`、`GET /messages/{id}`；handler 测试覆盖幂等、404/403、分页。
- [ ] 3.4 实现 tasks 路由闭集（create/list/claim-ready/claim/renew/release/complete/fail/block/unblock/reopen/assign/needs/cancel）；错误码按 design §10.3 映射，handler 测试逐条覆盖。
- [ ] 3.5 实现 `GET /v1/board/events/stream`：SSE 帧、Last-Event-ID、after、channel/label/state/owner/for 过滤、before/after 匹配、隐藏事件空帧、心跳；集成测试覆盖断线续读与权限过滤。
- [ ] 3.6 写租户隔离矩阵：A/B tenant 对频道/消息/任务/SSE 的读写全部 404；真实 PG + RLS 双跑，确认应用谓词与 RLS 都不漏。
- [ ] 3.7 实现审计与指标：board 事件审计、permission.denied（无正文/token）、append 延迟/live claim/expiry/SSE lag 指标；负控测试扫描响应与日志无 token。
- [ ] 3.8 实现投影一致性检查与重建路径（内部命令 + 503 保护）；测试覆盖水位不一致拒绝写入、重建后哈希一致。

## 4. Phase 1：nsh 队列与任务卡

- [ ] 4.1 新建 `web/src/data/board.ts`：归一化缓存、SSE 增量合并、游标重连、按 cursor 去重、乐观覆盖；用 fixture 事件测试重连与去重。
- [ ] 4.2 实现 BoardQueue：状态 tabs、channel/label/owner/for 过滤、oldest first、分页、行内状态/needs/租约/结果；组件测试覆盖 8 个 tab 与筛选组合。
- [ ] 4.3 实现 BoardTaskCard：按 `moves` 显示 claim/renew/release/complete/fail/block/unblock/reopen/assign/needs/cancel，危险动作二次确认；测试覆盖每个动作的可见性与提交参数。
- [ ] 4.4 实现 BoardThread：根任务卡 + 回复列表 + Composer；提交成功后用服务端返回视图收敛，不重复插入；测试覆盖空回复、分页、错误。
- [ ] 4.5 实现租约倒计时、`lease ran out`、live tail 状态（connecting/live/reconnecting/offline）与刷新丢失 claim 的提示；测试用假时钟覆盖到期与重连。
- [ ] 4.6 实现五态（loading/empty/error/permission/success）并让 web/TUI 共用同一 TSX；`nsh check` 与 `nsh test --target tui` 覆盖队列、任务卡、线程。
- [ ] 4.7 接入 shadcn 官方组件、主题令牌、lucide 单色图标与键盘可达性（j/k、Enter、c/r/e/f/b/u/o/a/n/x）；检查无硬编码 hex、无 hover-only 动作。

## 5. Phase 1：阶段门禁

- [ ] 5.1 跑 `go test ./internal/board/... ./internal/store/... ./internal/httpapi/...`、`go test -race` 与 `nsh check`，全绿并记录输出。
- [ ] 5.2 脚本化 E2E：队列五态、筛选、claim/renew/release/complete/fail/block/unblock/reopen/assign/needs/cancel、租约到期、SSE 断线重连、web/TUI 同源；每一步截图，生成 HTML 图文报告，`UNCOVERED=0`。
- [ ] 5.3 独立子代理复核（禁 Codex）A/B/C 三轴：权限/租户、并发/租约、UI 五态/同源；P0/P1 清零并记录 findings 与处置。
- [ ] 5.4 把 Phase 1 测试输出、E2E 报告、复核结论归档到 change evidence；阶段签收后保持 change active 进入 Phase 2。

## 6. Phase 2：消息生命周期与批量

- [ ] 6.1 实现消息 edit/delete/tombstone：作者或 principal 可改，删除清空标签/正文并保持回复；测试覆盖 revision/`if-revision` 冲突与删除后不可写。
- [ ] 6.2 实现 `POST /messages/{id}/labels` 与标签并发条件（if/if-unset），保留 `board.*` 拒绝；测试覆盖 `label_conflict` 与根标签影响线程。
- [ ] 6.3 实现 `POST /tasks/batch`（≤100、key/needs 解析、全有或全无、idempotency）、`GET /tasks/batch/{id}` 与 >100 的分块客户端；PG 测试覆盖环与部分失败。
- [ ] 6.4 实现 `GET /threads`（过滤、latest activity、`older` 向后分页）与频道消息前向/后向读；测试覆盖 `before`/`after` 互斥与非法 cursor。
- [ ] 6.5 实现根删除取消任务、非 owner 编辑根阻塞任务（kind `edited`）并结束 claim；集成测试覆盖 waiting/ready/claimed/completed 四种起点。

## 7. Phase 2：superagent 适配

- [ ] 7.1 增加 `board_links` 表与 tenant config `board.superagent.{mirror,bridge,channel}`；关闭时所有适配路径 no-op，测试覆盖开关矩阵。
- [ ] 7.2 实现 Ticket→任务线程、Assignment→claim/lease 的单向镜像：确定性 token（HMAC + `KEEL_SECRET_KEY`，缺失 fail closed）、lease 续期、状态映射；测试覆盖重复同步与重启重建 claim。
- [ ] 7.3 实现 Attention→blocked、answered/timed_out→unblock、worker report→reply；用 report_id/attention_id 幂等，测试重复事件不产生第二条。
- [ ] 7.4 实现命令桥：board 动作先调用 supervisor 既有 API，成功后才写 board 事件；supervisor 拒绝/超时无 board 状态变化；测试覆盖 claim/complete/fail/block/unblock/assign/cancel。
- [ ] 7.5 验证归档语义：已归档 Ticket 只读、不重写 `collab_*`、不产生新任务；回归测试锁定既有 superagent 看板字段与审计。
- [ ] 7.6 适配器 PG 集成测试：真实 A/B tenant、重复 Source 事件、claim 冲突（lease 过期后被他人 claim）时记录告警且不覆盖。

## 8. Phase 2：GitHub 入站与回复出站

- [ ] 8.1 通过既有 `Source` 实现 GitHub issue/PR/comment 入站：游标持久化、事件 ID 去重、频道/标签映射；测试重试同一事件只产生一条消息。
- [ ] 8.2 通过既有 `Channel`/outbox 实现回复出站：外部 comment ID 幂等、失败退避、不回显自身事件；测试断线重试不重复评论。
- [ ] 8.3 实现 PR merged/closed、issue closed 的状态映射（completed/canceled）与 GitHub label 普通标签同步，禁止写 `board.*`；测试覆盖映射边界。
- [ ] 8.4 限流、退避、单租户 source 隔离与凭据不外泄测试；确认不引入 Vercel sync/Chat SDK。

## 9. Phase 2：阶段门禁

- [ ] 9.1 跑 Phase 2 单测/集成/race，覆盖 batch、消息生命周期、适配器与 GitHub；全绿并记录输出。
- [ ] 9.2 脚本化 E2E：批次计划、根编辑阻塞、根删除取消、superagent 镜像/命令桥、GitHub 入站/回复；每步截图 + HTML 报告，`UNCOVERED=0`。
- [ ] 9.3 独立子代理复核（禁 Codex）：双真值风险、幂等、归档语义、跨租户；P0/P1 清零并归档证据。

## 10. Phase 3：watchers、schedules 与附件

- [ ] 10.1 实现 `board_watchers` 与 durable consumer：tail 表达式、过滤、at-least-once、lease/ack、退避、死信；测试 watcher 失败不影响其他 watcher 与 board 写。
- [ ] 10.2 实现 `board_schedules`：jobqueue 领取、触发前权限复检、取消 `revoked`、幂等消息键；测试权限回收后不发送、重试不重复。
- [ ] 10.3 实现附件（复用 blob lifecycle）、reactions、mentions、schema 校验的 JSON blocks；测试附件读权限、引用计数与删除隐藏。
- [ ] 10.4 实现保留期、事件配额与计量（tenant config）；测试清理只影响 board，不影响 superagent/Session 账本。
- [ ] 10.5 验证 watcher 不在运行时接受 URL、schedule 不绕过权限、附件不跨租户；负控测试全绿。

## 11. Phase 3：收口与归档

- [ ] 11.1 更新文档：`docs/ARCHITECTURE.md`/`docs/HARNESS.md`/`web/README.md` 的 board 段落、README 入口；补 Apache-2.0 归属说明，确认未复制 vercel/board 代码、未新增运行时。
- [ ] 11.2 跑全量门禁：`go test ./...`、`go test -race` 关键包、`nsh check`、`nsh test --target tui`、`openspec validate keel-board-v1 --strict`；全绿。
- [ ] 11.3 脚本化最终 E2E：全部屏幕/状态 before/after 截图、HTML 图文报告、`UNCOVERED=0`；独立子代理复核（禁 Codex）P0/P1 清零。
- [ ] 11.4 `openspec archive keel-board-v1 --yes`，确认 `openspec/specs/agent-board/spec.md` 已生成且 `openspec validate --specs --strict` 通过。
- [ ] 11.5 提交实现与 `openspec`（不 push，除非 Owner 明确要求）；汇报 commit hash、strict 输出与剩余 Owner 决策。
