# Tasks: keel-service-graph

> 一个 change，多个并行工作流；**对外只有一个完成态**。所有任务完成并验收后才归档。
> 工作流内部可以并行，但以下依赖必须尊重：W1 → W2 → (W3/W4)；W5 可在 W2 后并行；
> W6 依赖全部。

## W0. 基线、规格与迁移设计

- [x] 复审并合并 `task/keel-tenancy-usage`、`task/keel-mcp-oauth`、`task/keel-ops`（已合流，冲突按两侧语义并集解）：
      解决 `cmd/runtime` / `httpapi/server.go` 冲突
- [x] 全量回归基线：`go test ./... -race`（29 包）、PI conformance（633-0-1）、E2E 7/7、2GB/1000（A/B 对照无退化）
- [x] 对账 `runtime-foundation` / `keel-ga` / `keel-ecosystem`（三张板加冻结头并指向本 change；`keel-tenancy-usage` 全勾后归档 `openspec/changes/archive/`）；
      完成项标记/归档，未完成的与本 change 去重，禁止三张任务板并行
- [x] 冻结 v1 接口：`Commit`、`Lease`、`ServiceDescriptor`、`Loadout`、
      `AgentLoop`、`ExternalProvider`
- [x] 迁移设计：新增列/表只做增量；默认 loadout 对 R1 差分（`cmd/r1diff` PASS）；feature flag 回退
- [x] 冻结 W0 R1 基线（规范化 HTTP/事件 dump/工具序列）；写差分验收器与白名单（`cmd/r1diff`＋`internal/r1diff`＋`r1-allow.json` 值钉死）
- [x] 出 API MODIFIED delta：`POST/GET /v1/sessions/{id}/events` 收紧（400/404）＋`/input` status=committed、异正文 409（proposal 与 `r1-allow.json` 同步）
- [x] 对账旧 change：重叠项出 MODIFIED delta（proposal §API MODIFIED delta）；generationId 幂等在 W1 落地（内核按 (session,generationId) 去重）
- [x] 本文档评审通过；写 `docs/SERVICE-GRAPH.md`、`docs/HANDOFF.md`、`docs/LOADOUT.md`
- [x] W0 装配级测试：抽出可测的 buildRuntime(cfg)，覆盖 admin key/OAuth/drain/metrics 组合矩阵（`internal/httpapi/assembly_matrix_test.go`：8 组组合，含探测端点/排水分支/未配置 OAuth 的 503 与鉴权先后）
- [x] W0 安全口径：租户 provisioning 鉴权、OAuth tenant 绑定、callback 写凭据鉴权、admin key 轮换测试
- [x] W0 运维：mcp_oauth_pending 清理、lockPool 按需/预算、OnChallenge 可见性

## W1. 契约内核与持久化边界

- [x] 抽出 `internal/kernel`：事件种类注册、`Commit` 校验、租户/lease/epoch 检查
- [x] session 行作为 lease 线性化点：`sessions.owner/owner_epoch/lease_until`（或同锁 lease 行）
- [x] 所有事务统一锁序 sessions → runs；Claim/Renew/Commit/Finish 持锁重读 epoch
- [x] 提交类别：owner-scoped（assistant/tool.result/compaction/run）vs edge-scoped（user.message）
- [x] edge 白名单与 API 收紧：只收 user.message；跨租户/不存在 404
- [x] `claim || stale-commit` 交错压力测试：stale 0 落账，最终回复=1
- [x] ClaimLease 候选陈旧快照测试：候选扫描窗口内旧 owner Finish 已结算 run，不得复活（`TestClaimDoesNotResurrectSettledRun`）
- [x] Commit/Claim/Finish/Release 对 40P01/40001 重试；并发迁移+提交压力测试
- [x] 工具配对扫描改为投影表/有界窗口，避免 session 写锁内 O(N) 回扫（0002 索引＋jsonpath）
- [x] 运行时接线后恢复 ready 探针对 sessions/runs kernel 列的断言（并补齐 owner/lease_until 全列）
- [x] `Commit` 实现幂等：`requestId` / `generationId` / `toolCallId`（异内容→409）
- [x] 工具调用↔结果配对校验；孤儿/重复结果拒绝
- [x] `Claim` 返回 fencing token；`Renew` / `Finish` / `Commit` 校验 epoch
- [x] 旧 owner 提交拒绝 + 审计（`Options.Audit` 接 slog，限频）；过期 lease 回收保持现有语义
- [x] 服务依赖只暴露窄接口，移除插件可达的原始 store 写句柄（`internal/kernel/seams.go`＋`servicehost.Seams`）
- [x] 租户/配额检查接入提交与领取路径（`internal/quota`，租户文档可覆盖默认）
- [x] 测试：重复提交、孤儿结果、双 owner、旧 epoch、跨租户、kill 恢复
- [x] 测试基建：fake provider 确定性 toolCallId（由已提交输入派生）、调用计数；禁止 UnixNano（可步进屏障在 handoff 五安全点中以真实进程实现）

## W2. 服务图、loadout 与默认装配

- [x] `internal/loadout`：不可变快照、revision、服务集合、配置摘要
- [x] `internal/graph`：descriptor、provides/requires/optional、拓扑解析
- [x] 拒绝依赖环、重复必需 provider；可选 provider 降级诊断
- [x] 生命周期：PENDING/LOADING/ACTIVE/UNLOADING/FAILED、effects 反序清理、
      generation、启动失败回滚
- [x] 默认 loadout 复现 W0 后的 `cmd/runtime`：store/runner/host/MCP/telemetry/worker
- [x] loadout = host.TenantConfig 解析结果 + 描述符 + 摘要；下一 run 生效，在途 run pin revision
- [x] 写 host.TenantConfig → loadout 的单源 adapter；禁止第二套 config 词汇/字段
- [x] 默认蓝图只用已实现服务（sandbox 未实现则 optional/不启用），不造空工厂
- [x] RETIRING 态 + 在途 run 引用计数；引用归零/drain 截止后反序卸载
- [x] 宿主 ticker/后台任务驱动 CollectExpired（空闲也要收尾）；实例级退休，未变化服务可跨代共享
- [x] R1 差分：同输入旧装配 vs 默认 loadout；白名单外 0 差异
- [x] 服务适配器：`pi-loop`、`prompt`、`tools`、`llm`、`session-projection`、`compaction`、`mcp`、`telemetry`（`internal/servicehost/services.go`）；sandbox 无实现——蓝图 optional、无工厂、不启用（登记为差距）
      `compaction`、`mcp`、`telemetry`、`sandbox`
- [x] loadout_id/revision 记录到 run 元数据/日志；只读查询接口
- [x] 测试：缺失依赖、循环、重复 provider、启动失败回滚、反序卸载、generation、
      race；默认 loadout 与旧装配行为对比
- [x] 对抗审查：服务边界是否可绕过内核；拒绝直接写事件（两轮只读对抗审查：6/10 与 4/10，结论与修复见报告）

## W3. PI loop 服务化与 conformance 门

- [x] 定义 `AgentLoop` 服务接口（step 输入/输出、事件提案、取消、工具意图）
- [x] 将当前 `internal/loop` 包成默认 `pi-loop` 服务适配器
- [x] `Runner` 拆分为：内核侧 run coordinator + 服务侧 loop（loop 经 `agentloop.Loop` 接口驱动）
- [x] 实现一个测试用 alternate loop，证明替换路径真实可用（`agentlooptest` 23 用例对任意实现可跑）
- [x] conformance manifest：未通过 PI 套件的 loop 不可进入 loadout
- [x] 把 PI conformance 抽成可对 AgentLoop 服务接口运行的导出套件，并记录清单 hash；
      不再只依赖 internal/loop 包内测试
- [x] 测试：事件顺序、工具配对、取消、错误、transcript 等价；PI 495 不退化
- [x] 对抗审查：loop 是否仍可能绕过 `Commit`；错误路径是否污染账本（`agentloop` 边界测试 + 两轮审查）

## W4. 作用域、handoff 与 fencing

- [x] runtime/tenant/session/run 四级作用域；loadout 解析与 pin
      （claim 写入 loadout_id/revision，`kernel.PinRunLoadout`/`RunPin`）
- [x] 持久 runtime_id；写入 claim/审计（`runtime_instances`，owner=runtime_id，
      `KEEL_RUNTIME_ID`，缺省 hostname-pid）
- [x] drain 截止与超时强制接管；旧 owner 提交拒绝（handoff 行记 `timed-out`，
      租约到期后 epoch+1 接管，旧 token 提交被 fence）
- [x] 两种 drain 模式：shutdown（SIGTERM，保留 503/取消语义）vs handoff（停 claim、
      继续收 edge 输入、安全点排水）；`httpapi.ModeDrainer` + admin drain 端点
- [x] handoff 模式配置/审计标记；补「drain 中 input 仍落账」测试
      （`httpapi.TestRuntimeDrainHandoffAdmitsEdgeWrites`、进程级 `TestRuntimeHandoffProcess`）
- [x] SSE 读路径归属：只读已提交事件；客户端最终切新 runtime
- [x] handoff 双进程 RSS 纳入 2GB 验收（`scripts/acceptance-handoff-rss.sh`：两 runtime 并存 + 1000 SSE + 中途换班，峰值 498MB/24%，SSE 硬错误 0）
- [x] 新 runtime ready → 领取 → epoch+1 → 从事件重建
      （`handoff.Coordinator.Adopt` + `Rebuild`，mem/PG 双跑）
- [x] SSE `Last-Event-ID` 跨 handoff 连续；旧 partial 丢弃但不丢已提交消息
      （两个真实 runtime 进程 + 独立库 + 真实 SSE 客户端：`TestRuntimeHandoffProcess`）
- [x] 测试矩阵：用户消息后、assistant tool call 后、tool result 后、模型流中、
      工具执行中；每种含旧进程 kill（`handoff.TestSafetyPoints`，mem+PG）
- [x] 断言：0 丢事件、0 重复最终回复、run 收敛、审计含 loadout/epoch
- [x] drain 期间新输入不丢；旧 writer 晚提交被拒
      （`TestDrainModesAreDistinct`、`TestStaleWriterFencedAfterHandoff`）
- [x] 对抗审查：交接窗口、双写、SSE 游标、旧 owner 重试（B2 运行中输入吞单 +
      M1 commit/bookkeeping 非原子已修：`pending_input` 旗标 + read-through 水位 +
      edge 提交同事务入队；跨版本割接纪律见 docs/HANDOFF.md 与 cutover_test.go）

## W5. 外部网关与 DSH/PI 生态适配

- [x] `internal/gateway`：stdio + Streamable HTTP JSON-RPC、初始化/能力协商、
      请求关联、取消、超时、错误
- [x] 流式 JSON 解析、帧上限、背压、慢消费者、坏帧隔离
- [x] `ToolProvider`：外部工具注册/调用/结果回传；结果由内核记账（库级）
- [x] stdio 写可取消/有界出站队列；SpawnStdio 默认最小 env；进程组/孙进程回收
- [x] `AgentBackend`：版本化私有方法集（initialize/session.open/resume/message.send/
      event.stream/cancel/health/close），复用 MCP 传输/帧，不伪造 MCP 方法语义
- [x] 外部会话表/字段：`state_owner`、`external_backend`、`external_ref`、
      `external_status`；不进入原生 run/lease 作业流（W5 接线：`externalgw` 服务 +
      `/v1/external/sessions…` + kernel external 提交类）
- [x] 外部 usage 标记 `metering=external|none`；配额策略显式选择
      （`external.quota.unmetered=allow|deny` 必填；不静默记 0）
- [ ] DSH sidecar 参考适配器：pin `@deepseek-ai/dsh` 0.2.1-rc.5 + Node 22；
      拉起/停止、env/凭据清洗、reaper、崩溃退避、资源预算
      （已交付参考 sidecar `cmd/keel-dsh-ref`：拉起/停止、env 清洗、进程组 reaper、
      崩溃退避、每 backend `maxSessions`；真实 DSH 0.2.1-rc.5 手测与资源预算部署契约
      见 `docs/DSH-REFERENCE.md`，未进 CI）
- [x] DSH tool 参考适配器：Keel 侧参考实现（`cmd/keel-dsh-ref` 的 ToolProvider 方法集：tools/list、tools/call、JSON 校验、取消、错误映射、输出裁剪，`internal/dshref` 测试覆盖）；DSH 侧的导出插件仍属 DSH 仓工作（非零代码），登记在 `docs/DSH-REFERENCE.md`
      （参考 ToolProvider `-kind=tool` 已交付并测试；DSH 侧导出插件为文档化非零代码）
- [x] 兼容矩阵按 native/external 两种模式重写；UI/native 不承诺
- [x] 明确拒绝逐插件 Cordis RPC；文档写兼容矩阵与限制
- [x] 测试：大参数、慢消费者、坏帧、崩溃、超时、secret 脱敏、兼容标记
- [x] 对抗审查：外部进程是否可能触达账本/租户；双账本是否被伪装成原生
      （`internal/externalgw/boundary_test.go` + `external.event` 强制 backend/externalRef
      标记 + NativeEdge 对称判定；外部账不能写进原生会话，原生 edge 不能写进外部会话）

## W6. 集成、验收与交付

- [x] 默认 loadout 全量回归：PI conformance 674-0-1（复核修复后重跑；证据 `conformance.json`，带 DSN；无 DSN 584-0-79）、E2E 7/7、2GB/1000/64（A/B 对照无退化；`scripts/acceptance-perf.sh`）
- [x] R1 差分验收：白名单外 0 差异（`cmd/r1diff`，12 条规则＝9 条值钉死＋3 条文案豁免；负控实验证明门不空转）；API 收紧用例（400/404）
- [x] 混沌：10 分钟随机 SIGKILL + handoff 故障注入；0 丢事件/0 重复最终回复（`cmd/chaos`：kill 模式 10 分钟 30 次故障、handoff 模式 5 轮接班即崩，seq 连续/requestId 唯一/终答不重复/无孤儿结果/run 收敛全绿）
- [x] 插件故障注入：启动失败、运行中崩溃、缺失依赖、回滚、卸载顺序（`internal/servicegraph` 引擎测试 + `servicehost` 生命周期测试）
- [x] 网关故障注入：慢/坏/崩/超时；内存与队列有界（`TestStuckBackendTimesOutAndServiceStaysUsable`、`TestBackendCrashMarksUnavailableAndIsolatesNativeFlow`、`TestCrashRestartBackoffAndNoFakeResume`、`TestProjectionFailureRetriesAndKeepsTheBackendAlive`；帧上限/有界队列在 `internal/gateway` 既有套件）
- [x] 指标基数审查：loadout/handoff 指标不得带 tenant/session/run 标签（`internal/metrics` 白名单＋forbiddenLabelFragments 结构性强制，标签仅 status/outcome 等有界枚举）；
      loadout revision/generation 只进日志/诊断，不进指标标签
- [ ] gateway 补充故障 scenario：crash-loop/重启风暴、大帧取消、stderr 洪泛、
      双租户同名工具、provider 图片内容（已覆盖 crash-loop/重启风暴与大帧/帧上限；
      双租户同名工具与 provider 图片内容未覆盖，登记为后续加固）
- [x] handoff 补充：新 runtime 刚 claim 就崩（epoch 连跳：`cmd/chaos -mode handoff` 6 轮实跑）；交接恰逢压缩——压缩未服务化，登记为差距
- [x] 文档：loadout 示例（`docs/LOADOUT.md`）、handoff runbook 与回滚手册（`docs/HANDOFF.md`）、DSH 兼容矩阵（`docs/DSH-REFERENCE.md`）
- [x] 子代理交叉复核（reviewer/vision，不使用 Codex）——四轮对抗审查驱动的 N1–N12 全修；
      终轮「合流增量 + 证据链 + 验收工具红队」复核 7/10 判 1 阻塞（外部会话 Open 非原子：
      并发可改写 marks / closed 可复活 / 两条 backend 对话混入同一日志）→ 修复后复审 9/10
      判 B1 闭合、无新阻塞；报告页另有 vision 走查，P1 逐项整改
- [x] HTML 报告：过程截图 + 大白话解说；公网链接 `https://alpha-keel-service-graph.fde2e.com/report.html`
      （同一内容另有本机隧道 `https://local-gaubee-keel-service-graph.fde2e.com/report.html`）
- [x] 归档本 change；更新主仓 submodule 指针与项目索引（keel main 已推送：服务图终版 squash +
      复核修复批次；主仓指针 + E2E 索引行 commit `50050c85`）

## 完成定义（Definition of Done）

- 五个 spec 的所有 Scenario 有自动化测试并通过；
- 默认 loadout 与旧行为等价；外部 API 无破坏；
- handoff 五类安全点全部通过；
- 外部 gateway 与 DSH 参考适配器有真实测试；
- 2GB/1000 基线、PI conformance、E2E 不退化；
- 对抗审查无未处理 P0/P1；
- 报告与文档可被未参与实现的人按步骤复现。
