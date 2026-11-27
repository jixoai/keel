# Tasks: keel-board-v1

> 阶段门禁：每个 phase 结束 MUST 跑该阶段的单测/集成/E2E，并把证据写到
> `openspec/changes/keel-board-v1/evidence/<phase>-*.md`（脚本生成，禁手写）；独立子代理复核
> （禁 Codex）P0/P1 清零后才进入下一 phase。Phase 0 在独立仓库产出不可变 commit + `v0.1.0`
> tag；Phase 1 只做 keel 导入/适配；Phase 2 只做 UI；最终 `openspec archive` 在 Phase 2 收口。

## 1. Phase 0：独立仓库与契约

- [x] 1.1 创建 `github.com/jixoai/keel-board`（Apache-2.0）：`LICENSE`、`NOTICE`（上游 `vercel/board@26428ed4…` 归属 + 独立实现声明）、README、CI、Go module、`cmd/board`；`go test ./...` 通过。
- [x] 1.2 实现 `board` 领域包：Channel/Message/Thread/Task/Claim/Needs/Label/Attachment/Block 类型、事件 kind 闭集、错误码、七态状态机；表驱动测试覆盖每条边与非法边。
- [x] 1.3 实现 token（32 字节 base64url/SHA-256/常量时间）、cursor、标签文法、needs 上限、文本/graph 校验；单测覆盖边界与非法输入。
- [x] 1.4 写 conformance 契约测试骨架：同一个测试套件可跑 in-memory store 与 PG store；fake auth/authorizer 可切换；`go test ./conformance` 通过。

## 2. Phase 0：核心日志与存储

- [x] 2.1 增加 core schema（`board_heads/board_events/board_requests`）、索引、FORCE RLS 与幂等迁移；投影为进程内确定性 fold（不建 PG 投影表，见 design §4.2）；`go test ./store -run 'Board|Postgres'` 验证重复迁移与 A/B 租户隔离。
- [x] 2.2 实现 `Append` 引擎：tenant head 锁、seq、事件与幂等结果同事务、提交后进程内 fold；属性测试验证增量 fold 与全量 replay 一致。
- [x] 2.3 实现幂等缓存（digest 定义幂等范围：post/task 的落点、batch 的 request key；同 key 同结果、不同 digest 409 `duplicate_id`、result 脱敏）与 cursor 编解码；测试创建/claim/complete 重试。
- [x] 2.4 实现频道/消息/线程：首帖建频道、幂等创建、根/回复、版本/修订、频道计数、线程汇总；集成测试读回与排序。
- [x] 2.5 实现任务创建/list/owner/by/for、channel 与 re 两种入口、重复 re 409 `already_a_task`；测试 owner Account/Bot 差异。
- [x] 2.6 实现 claim-ready 顺序、claim/renew/release、epoch/token 围栏、租约计算；PG 并发测试断言只有一个 claim 成功。
- [x] 2.7 实现 complete/fail/block/unblock/reopen/assign/needs/cancel 的守卫与事件；表驱动测试覆盖 no-op 与终态拒绝。
- [x] 2.8 实现 lazy expiry、可选 sweeper、读修复（ready 补齐）；可注入时钟测试旧 claim 失败、新 claim 成功。
- [x] 2.9 实现 needs 校验（存在/可读/非 canceled/自环/环/500 上限）与完成传播；测试 20 上限、环、超深、失败/取消前置。
- [x] 2.10 实现 `board.*` 保留标签、`board.task` 投影、按根标签过滤回复；测试客户端写保留标签被拒。

## 3. Phase 0：完整 board 表面

- [x] 3.1 实现消息 edit/delete/tombstone、`if-revision`、标签 API、批量任务/plan、threads 列表与 older 分页；测试 revision 冲突、根删除取消、非 owner 编辑阻塞。
- [x] 3.2 实现 attachments（blob lifecycle、4MB/10 per message、签名直传可选）、reactions、mentions、schema 校验的 JSON blocks；测试读权限、引用计数、删除隐藏、非法 block。
- [x] 3.3 实现 watchers（`board_watchers`/`board_watch_deliveries`，durable consumer、lease/ack、退避、死信）与 schedules（`board_schedules`、权限复检、幂等消息键）；测试失败隔离。
- [x] 3.4 实现 `/v1/board` core API、SSE（Last-Event-ID、过滤、隐藏事件、心跳）、权限矩阵、404/403、审计/指标；`/metrics` 暴露 design §18.2 的核心指标，sync 审计写 `board_audit`（无正文/secret）；HTTP 与 SSE 集成测试通过。
- [x] 3.5 实现投影一致性检查与重建命令/测试；水位不一致拒绝写入，重建后哈希一致。

## 4. Phase 0：Sync 引擎

- [x] 4.1 增加 sync schema：`board_sync_entries/inbox/threads/copies/outbox/source_cursors/bridge_nonces` + 索引 + RLS；迁移幂等。
- [x] 4.2 实现 inbound：provider 验签结果落 inbox、dedupe 唯一、lease/ack、失败退避/dead letter、重启恢复；测试重复 webhook 只导入一次。
- [x] 4.3 实现 outbound：board 事件→outbox、每 thread lane 保序、确定性 delivery_id、receipt/copy、失败重试/`message.sync_failed`/后续继续；测试失败与清除。
- [x] 4.4 实现 thread mirror：root 起 thread、attach channel 的 root+backfill、thread target 只未来、subthread 归组与回复路由、`noSync`、来源 target/key 跳过；测试各路径。
- [x] 4.5 实现 echo 抑制：bot 身份、隐藏 marker、bridge/平台无身份时 fail closed；测试不自激、不重复。
- [x] 4.6 实现 sync 管理/源读取 API（entries/status/inbox/retry/sources read/join/webhooks/bridge events）与权限；entries 的创建/轮换在 Phase 1 接 tenant secret store 后提供（10.6），Phase 0 由部署配置构造 registry；测试 board.manage 与未知 key。
- [x] 4.7 实现文件/链接/mentions/typing：入站文件抓取与降级、出站上传/说明行、link snapshot、native mention、typing ephemeral；测试大小/失败降级。

## 5. Phase 0：Slack 原生适配器

- [x] 5.1 实现 Slack webhook 验签、事件归一化（message/edit/delete/reaction/files/link）、dedupe、3 秒内存先落盘；测试伪造签名与重复投递。
- [x] 5.2 实现 Slack 出站：post/edit/delete/react/files/typing、thread/reply、bot 身份与 `chat:write.customize` 降级；测试失败/限流/文件拒绝。
- [x] 5.3 实现频道名称/ID 解析、permalink、source read、join opt-in、echo 识别；测试私有/未知频道与权限。
- [x] 5.4 Slack 契约测试 + fake Slack 服务器端到端：双向 message/edit/delete/reaction/file/typing，全部通过。

## 6. Phase 0：GitHub 原生适配器

- [x] 6.1 实现 GitHub App webhook 验签与事件归一化：PR/issue opened/closed/reopened、comments、review submitted/dismissed、workflow run；测试重复 delivery。
- [x] 6.2 实现生命周期/状态标签：`state/review/head/tag.*/assignee.*` reconcile、review thread 归组、CI pass/fail；测试 PR/issue 全生命周期。
- [x] 6.3 实现出站 comment/edit/delete/reaction、`botUserId`/`viewer` 识别、rate limit；测试评论双向与不回显。
- [x] 6.4 GitHub 契约测试 + fake GitHub 服务器端到端；验证 board root 默认不创建 issue、已有线程双向。

## 7. Phase 0：Linear 原生适配器

- [x] 7.1 实现 Linear webhook 验签与事件归一化：issue created/updated、comments、properties/labels/assignee；测试重复与未知 team。
- [x] 7.2 实现入站映射：root/comment/state/property、comment thread 归组；测试状态类型与 issue 更新。
- [x] 7.3 实现出站：board root 创建 issue、comment/edit/delete、状态/属性、reaction 尽力；测试创建与失败。
- [x] 7.4 实现 Linear source read（team/project/issue）与权限；契约 + fake Linear 服务器端到端。

## 8. Phase 0：Chat SDK 通用 bridge

- [x] 8.1 定义 bridge 契约：归一化 `InboundEvent[]`、`deliver/resolve`、错误码、大小上限、HMAC v1、timestamp/nonce、重放窗口；写 JSON schema/文档。
- [x] 8.2 实现 board 侧 bridge：`POST /sync/bridge/{key}/events` 验 HMAC/nonce/落 inbox；board→sidecar 的 `resolve/deliver` 客户端与重试；测试坏签名/重放。
- [x] 8.3 实现参考 sidecar fixture（测试用 fake Chat SDK adapter，不嵌入生产运行时）：inbound/outbound/resolve；测试任意 adapter 形状。
- [x] 8.4 bridge conformance：与原生 provider 共用测试套件，验证 Chat SDK 通用路径可替代平台适配器。

## 9. Phase 0：门禁与发布

- [x] 9.1 跑 `go test ./...`、`go test -race`、PG 集成、conformance、`go test -count=10`（store/log/syncstore/automation/conformance）与 E2E；全绿并记录输出。
- [x] 9.2 E2E（独立仓库）：fake Slack/GitHub/Linear + fake Chat SDK bridge，覆盖双向镜像、edit/delete/reaction/file/typing、attach/backfill、失败/重试/dead letter、重启恢复；脚本生成 HTML 报告。
- [x] 9.3 独立子代理复核（禁 Codex）：并发/租约、sync 去重/回声、凭据/不可信输入、跨租户；P0/P1 清零并记录 findings。
- [x] 9.4 产出不可变 commit 与 `v0.1.0` tag；写 phase 0 evidence 与 pin 记录（module + commit + Apache-2.0）；完成阶段归档检查点并签收。
  > tag `v0.1.0` 指向 `d5a9e0a`（annotated `d1cb67d`，已推 origin）；脚本证据
  > `evidence/phase-0-gates-20261127T073344Z/phase-0-gates.md`（gofmt/vet/test/race/E2E/stress 全 exit 0，
  > stress 816s；最终独立复审 PASS，P0/P1=0）；pin `evidence/phase-0-pin.txt`。
  > Phase 0 门禁：CI run 45956949204 同一 commit test+stress 全绿。

## 10. Phase 1：keel 导入与基础设施适配

> Phase 1 基础设施（10.1–10.5、10.7）证据：`evidence/phase-1-infrastructure.md`
> （`scripts/keel-board-phase1-gate.sh` 生成）。10.6 与 12.x/13.1-13.2 由
> secret-store/sync lane 完成；10.7 的 credential 状态由 10.6 的 entry/secret 派生。
> **owner 写入正式偏离**：board 模块与 keel 关系表不能共享事务；201 后立即写 owner，
> 启动/周期 reconciler 按 `channel.created` 与最早 `message.posted` root 补偿（非原子）。
> Phase 2 做频道 ACL 撤销时必须加 revocation marker，否则 reconciler 会重新补授。
> **依赖 pin**：10.1 当前 `v0.1.1`（annotated tag，commit `70b503f0cdbd8f860ac1a373a6d1576175018ff0`）。

- [x] 10.1 keel `go.mod` 固定 `github.com/jixoai/keel-board` v0.1.1（tag，commit `70b503f…`）；`go test ./...` 通过；记录 NOTICE/许可证。
- [x] 10.2 实现 Account/Bot/Profile 适配：`Principal/Actor` 解析、Bot 身份、owner/by/for、平台 actor 命名；测试人与 Bot 差异。
- [x] 10.3 实现 RBAC/ReBAC：`board.read/write/manage` 权限键、频道 `resource_relations` viewer/editor/owner、最近祖先继承；真实 PG 测试。
- [x] 10.4 接入 keel PG 与 RLS：keel-board migrations 在 keel 启动幂等执行，`tenant_id` 谓词 + RLS 双保险；A/B 租户测试。
- [x] 10.5 接入 SSE hub 与 Last-Event-ID；keel 的 tenant 鉴权、心跳、多副本 NOTIFY/poll；集成测试。
- [x] 10.6 接入 tenant secret store：`SecretResolver`、sync entries 的 POST/PATCH（创建/启停/轮换/路由，只传 secret_ref）、`board.sync.enabled`/`board.enabled` feature flag；测试关闭即 404/无 webhook，日志/事件/响应无 secret。（evidence: `evidence/phase-1-sync.md`；启停即时，创建/轮换/路由变更持久化后需重启装载，期间 entry 在 board sync store 停用；创建即 pending disabled。路由目标必须是已存在且有 `board_channel` relation 的频道，否则 400；历史坏 entry boot 时 disabled 且不启动 worker；零 relation 频道可由 tenant board.manage 显式认领）
- [x] 10.7 接入 telemetry/audit：board/sync 指标、permission.denied、credential 状态；测试无 secret/正文。

## 11. Phase 1：superagent 适配

> 证据：`evidence/phase-1-superagent.md`（`scripts/keel-board-phase1-superagent-gate.sh`
> 生成，含真实 PG A/B、restart、race 与二进制增量）。

- [x] 11.1 增加 `board_links` 与 `board.superagent.{mirror,bridge,channel}`；关闭时 no-op；测试开关矩阵。
- [x] 11.2 实现 Ticket→task 线程、Assignment→claim/lease 单向镜像：确定性 token（HMAC + `KEEL_SECRET_KEY`，缺失 fail closed）、状态映射、lease 续期；测试重复同步/重启。
- [x] 11.3 实现 Attention→blocked、answered/timed_out→unblock、report→reply；report_id/attention_id 幂等；测试重复事件。
- [x] 11.4 实现命令桥：board 动作先调 supervisor 既有 API，成功后才写 board 事件；拒绝/超时无 board 状态变化；测试全部动作。
- [x] 11.5 验证归档只读、不重写 `collab_*`；确认 GitHub 只有统一 sync 一个入口，superagent Source 不双同步；回归测试。
- [x] 11.6 冻结“单一任务面”：旧 superagent 看板（web/TUI `/superagent` 路由、页面、只读数据源）标记为待删除，不新增其功能/兼容层；board 队列/线程必须覆盖 Ticket/答复/完成旅程；测试旧看板不再是唯一入口。

## 12. Phase 1：keel 上的 sync 启用

- [x] 12.1 从 tenant config/secret store 装配 Slack/GitHub/Linear/Chat SDK entries；webhook 路径注册；未配置时 `sync_disabled`；测试配置矩阵。entry 的静态 route 在写入与 boot 时都要求频道已存在且至少有一条 `board_channel` relation（fail closed），零 relation 频道走显式认领。（evidence: `evidence/phase-1-sync.md`）
- [x] 12.2 验证 Slack/GitHub/Linear 在 keel 的 webhook→inbox→board→outbound→receipt 全链路；fake 平台 E2E。（evidence: `evidence/phase-1-sync-e2e.html`）
- [x] 12.3 验证 bridge sidecar 在 keel 部署中的 HMAC/nonce/超时/5xx/malformed 重试；sidecar 不属于 keel 运行时。E2E 覆盖：真实 client timeout（sidecar 延迟 16s > 15s client timeout，第一次超时、第二次成功，attempts=2）、5xx 两次后成功、malformed 响应后重试成功；supervisor 侧由真实 PG + HTTP 的 bridge E2E 覆盖 timeout/5xx/retry 与 `board_disabled`。（evidence: `evidence/phase-1-sync-e2e.html`）
- [x] 12.4 验证 source read/typing/permalinks/attachments 在 keel 权限下的行为；`board.manage` 与频道关系测试。Slack permalink 在 E2E 中作为 board link 原样保留，并以 alice 会话读回。（evidence: `evidence/phase-1-sync.md`；attachments/source read/typing/permalink 在 `evidence/phase-1-sync-e2e.html`）

## 13. Phase 1：门禁

- [x] 13.1 跑 keel 单测/集成/race + keel-board conformance；全绿并记录输出。（`evidence/phase-1-sync.md`：boardintegration + race + `go test -p 1 ./...` + v0.1.1 conformance；不修改 board 仓库）
- [x] 13.2 E2E：keel API + fake 平台 + fake bridge，覆盖 sync 全链路、superagent 镜像/命令桥、跨租户；脚本生成 HTML 报告。`cmd/board-sync-e2e` 现在包含真实 PG + HTTP 的 superagent bridge 场景（auth principal → Surface.Gate → bridge → Surface → 真实 board log，fake supervisor）：频道级授权拒绝（supervisor calls=0、head 不变）、operator 组 claim/complete/block、`board_disabled` 404、supervisor timeout/5xx/retry。（evidence: `evidence/phase-1-sync-e2e.html`）
- [ ] 13.3 独立子代理复核（禁 Codex）：身份/RBAC、sync 凭据/不可信输入、双真值、回滚；P0/P1 清零。
- [ ] 13.4 写 phase 1 evidence；完成阶段归档检查点并签收；change 保持 active 进入 Phase 2。

## 14. Phase 2：客户端数据与投影

- [ ] 14.1 实现 `web/src/data/board.ts`：归一化缓存、SSE 增量合并、游标重连、按 cursor 去重、乐观覆盖；fixture 测试。
- [ ] 14.2 实现 sync 状态数据源：entries/targets/lag/failures/dead letters/credential 状态；fixture 测试权限过滤。
- [ ] 14.3 实现 watcher/schedule 状态数据源与启停动作；fixture 测试失败/权限。

## 15. Phase 2：nsh UI

- [ ] 15.1 实现 BoardQueue：状态 tabs、channel/label/owner/for 过滤、oldest first、分页、行内状态/needs/租约/结果；组件测试。
- [ ] 15.2 实现 BoardTaskCard：全部动作与二次确认，按 moves 显示；测试每个动作的可见性与参数。
- [ ] 15.3 实现 BoardThread + Composer：根任务卡、回复、错误/空/分页；测试收敛与不重复。
- [ ] 15.4 实现租约倒计时、`lease ran out`、live tail 状态、刷新丢失 claim 提示；假时钟测试。
- [ ] 15.5 实现 BoardSyncStatus/BoardAutomation：entries/targets/lag/failures/dead letter/重试、watcher/schedule 启停；权限与错误态测试。
- [ ] 15.6 实现五态（loading/empty/error/permission/success）、web/TUI 同源、键盘可达、shadcn/主题令牌/lucide 检查；`nsh check`/TUI 测试。
- [ ] 15.7 删除旧 superagent 看板：移除 `/superagent` web/TUI 路由、页面、只读数据源与仅服务旧看板的 API；不保留兼容开关；回归测试证明工单/答复/完成旅程由 board 队列/线程承接，旧地址 404。

## 16. Phase 2：门禁与归档

- [ ] 16.1 跑 `nsh check`、`nsh test --target tui`、keel 单测/集成；全绿并记录。
- [ ] 16.2 脚本化最终 E2E：队列/任务卡/线程/租约/live/sync 状态/watcher 启停；每个屏幕/状态 before/after 截图，HTML 报告，`UNCOVERED=0`。
- [ ] 16.3 独立子代理复核（禁 Codex）：五态/同源/权限、sync 故障 UI、回归；P0/P1 清零。
- [ ] 16.4 更新 docs/README/NOTICE：keel-board module+commit+Apache-2.0、上游归属、sync 配置与运行手册；确认未复制 vercel/board 代码、未新增运行时。
- [ ] 16.5 破坏性变更收口：在 CHANGELOG/README/升级说明明确“旧 superagent 看板已删除，board 为唯一任务面”，确认仓库内无旧看板路由/数据源/兼容开关残留。
- [ ] 16.6 `openspec validate keel-board-v1 --strict`、`openspec archive keel-board-v1 --yes`、`openspec validate --specs --strict`；提交实现与 openspec（不 push，除非 Owner 要求），汇报 commit hash 与剩余 Owner 决策。
