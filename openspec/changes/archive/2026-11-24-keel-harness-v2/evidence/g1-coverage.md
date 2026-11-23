# G1 状态机覆盖表（2026-11-24）

口径：`state-machines.md` 的每台机器必须落到「合法转移测试 + 非法/拒绝测试/事实」或明确的
`Deferred(scope)`。`matrix` = 表驱动矩阵测试；`suite` = 包级既有测试（列出代表用例）；
`static` = `scripts/state-facts.mjs` 的 state→fact 行；`gap` = 本轮未闭合、需后续补。

## 覆盖表

| 机器 | 权威状态（节选） | 合法转移测试 | 非法/拒绝测试/事实 | 方式 |
|---|---|---|---|---|
| A1 会话 | idle / running / waiting_human / paused / archived | `TestCrashThenRestartResumesWithoutADuplicateFinal`、`TestKillBetweenLoadAndFinishNeitherDuplicatesNorMisses`、`TestNormalHandoff`、`TestArchiveSessionRoute` | 租约/epoch：`kernel_test.go` stale-epoch 提交 + `stale-epoch violations audited`；**archived→running：`TestCommitClientInputRefusesArchivedSession`（409/`ErrInputArchivedSession`，零事件；`/run`、`/events` 同守卫）** | suite + matrix |
| A2 run | pending / running / settling / done/failed/cancelled | `TestPGClaimEpochAndLiveLease`、`TestPGConcurrentClaimsSingleWinner`、`TestFinishProbeIgnoresTerminalFailures`、`run_finished_test.go` | 同 epoch 双持有 / 终态再提交：kernel stale-epoch 断言与审计；settle 后提交拒绝 | suite |
| A3 上下文/压缩 | within_budget / near_limit / compacting / compacted | `compaction_checkpoint_test.go`（`TestCompactionPersistsEventAndCheckpoint`、`TestCheckpointCutInMovedUnreadInputFailSafe`）、`compaction` hygiene 套件（`TestPruneIsIdempotent`、`TestOffloadImagesOutsideRecentWindow`） | **compaction 中提交工具：derived**——runner 在回合之间串行压缩，没有并发的工具提交 API；切点/顺序由上述两个测试承接 | suite + static |
| A4 配置版本 | active / superseded | `internal/configfile` 轮转套件（`TestRotateReplacesTheFileAtomically`、`TestRotationKeepsInFlightAttemptOnItsPinnedVersion`、`TestRestartPicksTheRotatedVersion`） | `TestRotateRejectsInvalidDocumentsWithoutTouchingTheActiveFile`、`TestRotationWriteFailureLeavesNoPartialFile` | suite |
| A5 事件 kind | class / key 闭集 | `TestKindRegistryDeclaresClassAndKey`（新增）、`TestEventKindsMatchStore`、`TestDedupeKeyNamespace` | `TestV2KindsRequireTheirKey`、`TestCommitClassesAndEdgeWhitelist` | matrix |
| B1 Group | active / draining / retired | `TestPGDrainingGroup`、placement 套件 | draining 新准入拒绝；`TestPGDrainingGroup` 断言 | suite |
| B2 Worker 进程 | starting / draining / stopped | `TestNormalHandoff`、`TestDrainModesAreDistinct`、`TestRuntimeHandoffProcess` | `TestStaleWriterFencedAfterHandoff`、`TestDrainTimeoutForcedTakeover` | suite |
| B3 放置 lease | unplaced / placed / expiring / adopted / orphaned | `TestPGClaimEpochAndLiveLease`、`TestPGQueuedRunIsNeverClaimable` | `TestPGConcurrentClaimsSingleWinner`（同 epoch 单胜者）、更低 epoch 提交拒绝 | suite |
| B4 槽位/配额 | free / leased / queued | `TestPGRunSlotGroupLimit`、`TestPGTenantQuotaAcrossGroupsNoOversell`、`TestPGQueueFIFOAndPromotionOnRelease` | `TestPGConcurrentAdmissionsNoOversell`（30 并发/配额 5 → 恰 5）、`TestPGQueuedReAdmissionCannotJumpTheQueue` | suite |
| C1 任务卡 | open / in_progress / blocked / closed | `TestTerminalCardIsReadOnly`（新增矩阵）、tasklog 套件 | **`TestTerminalCardIsReadOnly`：`ErrCardClosed`，账本不增行；跨会话不可达** | matrix |
| C2 工具归属 | unbound（禁止）/ bound | `TestTaskGateStampsTheAmbientCard`、`TestTaskGatePassesAValidTaskIDThrough` | `TestTaskGateRejectsMissingOrForeignCards`（`task_id_required`/`task_not_found` 落 tool.result） | suite |
| C3 任务树/fork | root / child / fork 边 | `TestForkCreatesAPrefixSessionCardAndInheritedBinding`、`TestForkCardIsIdempotentAndReopens`、`TestSubagentCardLinksToParentActiveCard` | fork 原点缺失/环：tasklog 校验 + `TestTaskCycle` 路径 | suite |
| C4 资源关联 | attached / detached | `TestAttachDetachNoteAndCloseAreEvents`、panel 资源折叠 | detach 不存在 hash 拒绝；引用计数 | suite |
| D1 审批 | opened / delivered / answered / timed_out / cancelled | `TestDecisionHTTPIdempotency`、`approve_plugin_test`（timeout） | 同答案 `duplicate`、不同答案 **409**、超时后再作答拒绝 | suite |
| D2 通知 | created / delivered / consumed | `TestNotificationRoutesAckListAndURL`、`TestNotificationPixelAndExpiry`、`TestPostgresNotificationStore` | 已 consumed 再推/重复推送：ack 持久 + 三态真值表测试 | suite |
| D3 后台作业 | foreground / promoted / running / exited / killed / lost | `TestTimeoutContinuesAsJobAndKeepsOutput`、`TestKillCancelsJobAndRecordsTerminalState`、`TestRestartConvergesRunningJobsToInterrupted` | `TestLimitsRejectNewJobsWithoutKillingExisting`（超限拒绝不杀已有） | suite |
| E1 插件生命周期 | installed / verified / available / removed / reloading / rolled_back；租户 enabled/disabled/PENDING/FAILED | `TestPluginManagerInstallEnableDisableUninstall`、`TestPluginManagerUpdateSwapsAndRollsBack`、`TestPluginManagerBootDisableEnableRoundTrip` | `ErrPluginExists`/`ErrPluginState`/`ErrPluginBundled`/`ErrPluginDependency` + `install:refused` 审计；`TestPluginManagerIsolatesInvalidPackages` | suite |
| E2 扩展面声明 | missing / declared | `TestManifestExtensionSurfaceAcceptsValidUIContribution`、`TestManifestPureChunkWebviewNeedsNoUIEntry` | `TestManifestExtensionSurfaceRejectsBrokenSlots`、`...RejectsBrokenContributions`、`TestReservedSlotIDsAreNamespacedAndUnique` | suite |
| E3 槽位/贡献 | declared / contributed | `TestDecodeExtensionKeepsAnonymousContributionsDistinct`、`TestManagerInstallCarriesContributionAndUninstallLeavesNone` | 贡献指向未声明槽位：pluginpkg 校验拒绝 | suite |
| E4 UI surface | chunk / package / webview / sidecar | `TestManifestPureChunkWebviewNeedsNoUIEntry`、vertical 的 UI fixture 安装/卸载 | 坏包隔离 `TestPluginManagerIsolatesInvalidPackages`；卸载零残留（vertical §1.7） | suite |
| F1 执行空间 | idle / running / captured | `TestProductionWiringWriteThenBashRead`、`TestSessionsGetIndependentWorkspaces` | 独立 workspace/命令失败：bashbridge 套件 | suite |
| F2 capture | per-tool-call / checkpoint / manual | `TestBashWriteCapturedIntoManifestAndTree`、`TestCaptureOneHundredThousandFilesIsOneEvent`、capturepolicy 套件 | `TestCaptureFailureIsOneSystemEvent`（失败一条系统事件）；**崩溃格在 G2** | suite + static |
| F3 文件写/冲突 | write / conflict | `TestWriteReadPagingAndPreconditions`、`TestWriteIsIdempotentForTheSameToolCall`、`TestEditExactUniqueAndReplaceAll` | 前置条件不命中 `conflict`；同 toolCall 幂等 | suite |
| G1–G4 恢复/升级 | scan / backoff / resume / exhausted / fallback | `TestScanAutoResumesFailedRun`、`TestScanManualParksPendingRun`、`TestScanNeverParksAndDoesNotResumeFailures`、`TestBackoffDelay` | **G3：`TestScanProviderExhaustedWithoutFallbackEscalates` / `...InvalidEscalatesWithoutResuming`（不重排、落 `provider_exhausted_no_fallback`）** | suite + matrix |
| H1 MCP server | discovered / connected / failed | `internal/mcphost` 套件（`TestServerStateReportsEraVersionAndCapabilities` 等） | 诊断有界、坏 resource meta 拒绝；`internal/mcphost` 套件 | suite |
| H2 外部运行时 | starting / ready / stopped | `internal/externalgw` 套件 | 进程组回收/握手失败：externalgw 套件 | suite |
| H3 技能 | discovered / active / pinned | `internal/skills` 套件（`TestLoadSkillsFromDir_*`） | 非法名字/未知字段拒绝；skills 套件 | suite |
| H4 触发器 | registered / fired | `TestTriggerWebhookHMACVerification`、`TestTriggerConcurrencyCapRefusesInsteadOfQueueing`、`TestTriggerRunRequiresExistingSession` | 无 secret fail-closed、并发超限拒绝、缺失会话 404 | suite |
| I 前端连接 | connecting / live / degraded | web TUI/browser 套件（395/396） | 降级/断线重连：web 套件 | suite |
| J 跨域不变量 | — | `internal/architecture` 16 项 + R1 差分（152/103/0） | 边界/权限/幂等负控见 R1 与 architecture | suite |

## 静态检查与 kind 单测

- `node scripts/state-facts.mjs`：5 台表格机器 / 22 个状态，0 问题；输出 `machine | state | authority`。
  已接 `scripts/acceptance-all.sh`（`state-facts.txt`）。
- `TestKindRegistryDeclaresClassAndKey`：每个 kind 的 class/key、edge 白名单、双通道 kind。
- 散文机器（B3/B4/C2–C4/D1–D3/E2–E4/F1–F3/G1–G4/H1–H4/I/J）没有独立表格；其状态由
  section 文案指名权威事实，转移/拒绝由 `suite` 列出的测试承接。

## 显式 gap（本轮未闭合）

**0 个开放 gap。** 两处原先的缺口已处置：

1. **A1 archived→running 已修**：`CommitClientInput` 在 `SessionOwner` 之后校验
   `SessionActive`，归档会话的输入返回 409/`ErrInputArchivedSession` 且零事件；`/run` 与
   `/events` 走同一个 `requireWritableSession` 守卫；缺失会话仍走 `ErrNoSession`/404。
   测试 `TestCommitClientInputRefusesArchivedSession`（`g1-archive-guard.txt`）。
2. **A3 compaction 中提交工具 = derived**：runner 在回合之间串行压缩，没有并发提交 API；
   压缩事件/切点顺序由 `TestCompactionPersistsEventAndCheckpoint` 与
   `TestCheckpointCutInMovedUnreadInputFailSafe` 承接。负控不可达，不伪造测试。

> G1 的判定口径 = 本覆盖表（每台机器有归属）+ 静态 state→fact（5 表/22 状态）+ kindRegistry
> 单测 + C1/A1 的新矩阵/守卫。散文机器的“逐台独立矩阵文件”不是本轮载体，由 suite 列承接。

