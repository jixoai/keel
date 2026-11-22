# W09 OAR 外部工具切片 — 证据

> 日期：2026-11-22。Worktree：`.worktrees/keel-w09-oar`，branch `task/keel-w09-oar`（基于 main `548e3ae`）。
> 契约冻结：[`docs/OAR.md`](../../../docs/OAR.md)。实现：`internal/oar/**`、`plugins/keel.oar/**`。

## 1. 交付范围

| 项 | 结果 | 位置 |
|---|---|---|
| OAR 契约 v1 | 冻结 | `docs/OAR.md`（能力矩阵/record schema/typed 码表/进程组回收/网关复用） |
| 能力协商 + typed rejection | 实现 | `internal/oar/capability.go`、`errors.go`、`bridge.go` |
| record stream（`seq` + `agentPath`） | 实现 | `internal/oar/record.go`、`journal.go` |
| 宿主桥（复用 `internal/gateway` agent_backend） | 实现 | `internal/oar/bridge.go`、`service.go` |
| Codex 适配器 | 实现 | `internal/oar/codex.go`、`internal/oar/sidecar/codex.go`、`sidecar/mapper.go` |
| 确定性 fake sidecar | 实现 | `internal/oar/sidecar/fake.go`、`cmd/keel-oar` |
| durable journal + CLI/JSON 视图 | 实现 | `internal/oar/journal.go`、`view.go`、`cmd/oar-view` |
| 渲染 seam + fixture | 实现（UI 槽位留给 W12） | `internal/oar/projection.go`、`testdata/tool-renderer.fixture.json`、`plugins/keel.oar/fixtures/` |
| 插件包（目标形态） | 提供，安装延后 | `plugins/keel.oar/**`（当前 loader 只接受 tool_provider sidecar） |

未改：`internal/pluginpkg/**`、`plugins/keel.plugin-market/catalog.json`、`web/**`、
`internal/store/schema.sql`、`internal/harnessext/**`、`cmd/keel/serve.go`。

## 2. 门禁命令

```text
$ go test ./internal/oar/... -count=1
ok  	github.com/jixoai/keel/internal/oar	7.685s
?   	github.com/jixoai/keel/internal/oar/cmd/keel-oar	[no test files]
?   	github.com/jixoai/keel/internal/oar/cmd/oar-demo	[no test files]
?   	github.com/jixoai/keel/internal/oar/cmd/oar-view	[no test files]
ok  	github.com/jixoai/keel/internal/oar/sidecar	0.606s

$ go test -race ./internal/oar/... -count=1
ok  	github.com/jixoai/keel/internal/oar	12.627s
?   	github.com/jixoai/keel/internal/oar/cmd/keel-oar	[no test files]
?   	github.com/jixoai/keel/internal/oar/cmd/oar-demo	[no test files]
?   	github.com/jixoai/keel/internal/oar/cmd/oar-view	[no test files]
ok  	github.com/jixoai/keel/internal/oar/sidecar	2.073s

$ go vet ./internal/oar/...
（无输出）
```

测试清单（40 个顶层用例）：

```text
TestBridgeFakeSidecarCrashesCleanly
TestBridgeFakeSidecarRecordStream
TestBridgeReapsSidecarProcessGroup
TestBridgeRejectsMissingCapability
TestBridgeRejectsRuntimeMismatch
TestBridgeRejectsSimulatedRuntimeForLiveModel
TestBuildProjectionFoldsCallsAndResults
TestChildAgentPath
TestDeclarationForKnownRuntimesIsValid
TestDeclarationMatchesRuntimeMatrix
TestDeclarationRejectsUnknownCapabilityAndDuplicates
TestExternalBackendConfigTargetsExistingGateway
TestExternalGatewayProjectsOARRecords
TestFilterRecords
TestJournalAppendReadAndReopen
TestJournalDuplicateIsIdempotentAndRegressionIsTyped
TestJournalFileNameIsSafe
TestMapCodexLine (+8 子用例)
TestMapCodexLineBoundsOutput
TestMapCodexLineLinksItemIDs
TestMapCodexLineRejectsNonJSON
TestParseDeclarationRejectsEmpty
TestParseJournalRejectsCorruption
TestPluginPackageManifestIsValid
TestProjectionFixture
TestProjectionSortsBySeq
TestReadJournalMissingFile
TestRecordValidationAcceptsEveryKind
TestRecordValidationRejectsBadShapes (+10 子用例)
TestRequireReturnsTypedRejection
TestResultRequiresOutputOrError
TestResultWithoutCallStaysAStandaloneItem
TestSeqTracker
TestServiceCodexAdapterWithFakeCodex
TestServiceCodexCancelReapsProcessGroup
TestServiceCodexUnavailableIsTyped
TestServiceDisabled
TestServiceMissingSidecarIsTyped
TestSimulatedRuntimeCannotClaimLiveModel
TestWriteTable
```

关键用例 verbose 结果：

```text
=== RUN   TestBridgeRejectsMissingCapability
--- PASS: TestBridgeRejectsMissingCapability (0.26s)
=== RUN   TestBridgeRejectsSimulatedRuntimeForLiveModel
--- PASS: TestBridgeRejectsSimulatedRuntimeForLiveModel (0.08s)
=== RUN   TestBridgeReapsSidecarProcessGroup
--- PASS: TestBridgeReapsSidecarProcessGroup (0.12s)
=== RUN   TestServiceCodexUnavailableIsTyped
--- PASS: TestServiceCodexUnavailableIsTyped (0.01s)
=== RUN   TestServiceCodexAdapterWithFakeCodex
--- PASS: TestServiceCodexAdapterWithFakeCodex (0.35s)
=== RUN   TestServiceCodexCancelReapsProcessGroup
--- PASS: TestServiceCodexCancelReapsProcessGroup (0.25s)
```

覆盖对应关系：

| 判据 | 用例 |
|---|---|
| typed rejection（启动前） | `TestBridgeRejectsMissingCapability`、`TestBridgeRejectsSimulatedRuntimeForLiveModel`、`TestRequireReturnsTypedRejection`、`TestServiceCodexUnavailableIsTyped` |
| 进程树回收 | `TestBridgeReapsSidecarProcessGroup`（sidecar + fake child）、`TestServiceCodexCancelReapsProcessGroup`（codex shell + sleep）、`TestBridgeFakeSidecarCrashesCleanly` |
| record 顺序/归属 | `TestBridgeFakeSidecarRecordStream`（seq 1..N、跨 agentPath、attribution、journal 一致、无任务卡字样） |
| Codex 适配器（fake codex） | `TestServiceCodexAdapterWithFakeCodex`（call/result/message/usage 映射） |
| OAR 关闭/不可用 | `TestServiceDisabled`、`TestServiceMissingSidecarIsTyped`、`TestServiceCodexUnavailableIsTyped` |
| 渲染投影 | `TestBuildProjectionFoldsCallsAndResults`、`TestProjectionFixture`、`TestResultWithoutCallStaysAStandaloneItem` |
| 既有 externalgw 桥 | `TestExternalGatewayProjectsOARRecords`（OAR sidecar 作为 `host.ExternalConfig.Backends` 后端，`external.event` payload 含完整 record） |

## 3. fake sidecar 端到端（确定性）

命令（宿主 demo 使用 `oar.Service`；sidecar 由 `gateway.SpawnStdio` 拉起）：

```text
$ go build -o /tmp/keel-oar-w09 ./internal/oar/cmd/keel-oar
$ go run ./internal/oar/cmd/oar-demo -runtime=fake -sidecar=/tmp/keel-oar-w09 \
    -journal=/tmp/oar-journal-fake -prompt="inspect the repo"
runtime=fake sidecar_pid=93289 declaration={Schema:keel.oar.capabilities/1 ContractVersion:1 Runtime:fake RuntimeVersion:1.0.0 Simulated:true Capabilities:[records agent_path calls results status messages cancel resume subagents]}
{"schema":"keel.oar.record/1","seq":1,"agentPath":"/main",...,"kind":"status","name":"turn.started","status":"started",...}
{"schema":"keel.oar.record/1","seq":2,"agentPath":"/main",...,"kind":"call","callId":"call-1","name":"shell","status":"started","input":{"command":"printf OAR-OK"}}
{"schema":"keel.oar.record/1","seq":3,"agentPath":"/main",...,"kind":"result","callId":"call-1","name":"shell","status":"completed","output":{"exitCode":0,"stdout":"OAR-OK"}}
{"schema":"keel.oar.record/1","seq":4,"agentPath":"/main/worker-1",...,"kind":"call","callId":"call-2","name":"read_file","status":"started","input":{"path":"README.md"}}
{"schema":"keel.oar.record/1","seq":5,"agentPath":"/main/worker-1",...,"kind":"result","callId":"call-2","name":"read_file","status":"completed","output":{"bytes":42}}
{"schema":"keel.oar.record/1","seq":6,"agentPath":"/main",...,"kind":"message","name":"agent_message","message":"fake answer to: inspect the repo"}
{"schema":"keel.oar.record/1","seq":7,"agentPath":"/main",...,"kind":"status","name":"turn.finished","status":"completed"}
journal=/tmp/oar-journal-fake/session-oar-1.ndjson
```

CLI 视图：

```text
$ go run ./internal/oar/cmd/oar-view -journal /tmp/oar-journal-fake
SEQ  AGENT           KIND     NAME           STATUS     CALL    SUMMARY
1    /main           status   turn.started   started            turn started
2    /main           call     shell          started    call-1  {"command":"printf OAR-OK"}
3    /main           result   shell          completed  call-1  {"exitCode":0,"stdout":"OAR-OK"}
4    /main/worker-1  call     read_file      started    call-2  {"path":"README.md"}
5    /main/worker-1  result   read_file      completed  call-2  {"bytes":42}
6    /main           message  agent_message                     fake answer to: inspect the repo
7    /main           status   turn.finished  completed          turn completed

$ go run ./internal/oar/cmd/oar-view -journal /tmp/oar-journal-fake -projection
{
  "schema": "keel.oar.render-projection/1",
  "backendSessionId": "oar-1",
  "hostSessionId": "demo-host-session",
  "taskId": "demo-task-card",
  "runtime": "fake",
  "simulated": true,
  "agents": [
    {"agentPath": "/main", "runtime": "fake", "status": "completed", "calls": 1, "failures": 0},
    {"agentPath": "/main/worker-1", "runtime": "fake", "status": "completed", "calls": 1, "failures": 0}
  ],
  "items": [ ... call/result folded, message/status in seq order ... ]
}
```

`TestProjectionFixture` 把同一投影与
`internal/oar/testdata/tool-renderer.fixture.json` 逐字段比对（`UPDATE_OAR_FIXTURE=1`
是唯一允许的重生成方式）。

### 3.1 既有 externalgw 桥（未改 externalgw）

`TestExternalGatewayProjectsOARRecords` 把 OAR sidecar 配成
`host.ExternalConfig.Backends["oar"]`，由既有 `externalgw.AgentService`：

```text
NewAgentService -> Start -> WaitReady -> Open(scope,"oar",{config.attribution})
  -> Send -> external.event(type=oar.record) -> kernel.LoadEvents
```

断言：`state_owner=external`、`metering=external`、`external.event.direction=receive`、
`payload.data.schema=keel.oar.record/1`、`data.agentPath` 非空、`data.attribution`
的 `sessionId/taskId` 与 open 时一致。结果：

```text
=== RUN   TestExternalGatewayProjectsOARRecords
--- PASS: TestExternalGatewayProjectsOARRecords (0.25s)
```

这证明 OAR 是既有网关的一种 backend，没有第二套外部会话模型；`externalgw` 源码零改动。

## 4. Codex 真实适配器证据

环境：`codex-cli 0.166.0`（`command -v codex` 可用）。**未让 Codex 生成或评审代码**，
它只作为被适配的外部运行时。

能力协商（真实 sidecar initialize）：

```text
runtime=codex sidecar_pid=5850 declaration={Schema:keel.oar.capabilities/1 ContractVersion:1
Runtime:codex RuntimeVersion:codex-cli 0.166.0 Simulated:false Capabilities:[records agent_path
calls results status messages usage cancel live_model]}
```

真实 turn（`timeout=25s`，prompt `Reply with exactly OAR-OK`）：

```text
$ go run ./internal/oar/cmd/oar-demo -runtime=codex -sidecar=/tmp/keel-oar-w09 \
    -journal=/tmp/oar-journal-codex -codex-path="$(command -v codex)" \
    -timeout=25s -prompt="Reply with exactly OAR-OK"

stderr:
runtime=codex sidecar_pid=14024 declaration={... RuntimeVersion:codex-cli 0.166.0 Simulated:false ...}
Reading additional input from stdin...
2026-11-22T13:36:19Z ERROR rmcp::transport::worker: worker quit with fatal:
  Transport channel closed, when UnexpectedServerResponse("HTTP 403: ... Forbidden ...")
journal=/tmp/oar-journal-codex/session-oar-1.ndjson

records:
1 status turn.started started turn started
2 status thread.started started codex thread started
3 status codex.item.error degraded Codex is ignoring 1 unrecognized configuration setting...
4 status codex.item.error degraded Codex is ignoring 1 unrecognized configuration setting...
5 status codex.turn.started started
6 message agent_message "OAR-OK"
7 usage turn.usage total=17538 (input=17530, output=8, metering=external)
8 status turn.finished completed turn completed
```

关键原始记录（截断）：

```json
{"schema":"keel.oar.record/1","seq":6,"agentPath":"/main","kind":"message",
 "name":"agent_message","message":"OAR-OK",
 "attribution":{"sessionId":"demo-host-session","taskId":"demo-task-card"},
 "runtime":"codex","ts":"2026-11-22T13:36:25.4774Z"}
{"schema":"keel.oar.record/1","seq":7,"agentPath":"/main","kind":"usage",
 "name":"turn.usage","usage":{"metering":"external","inputTokens":17530,
 "outputTokens":8,"totalTokens":17538},"runtime":"codex"}
{"schema":"keel.oar.record/1","seq":8,"agentPath":"/main","kind":"status",
 "name":"turn.finished","status":"completed","message":"turn completed"}
```

结论（诚实口径）：
- 适配器确实 spawn 了 `codex exec --json`，解析了真实 JSONL：`thread.started`、
  配置 warning item、`codex.turn.started`、`agent_message`（模型回答 `OAR-OK`）、
  `turn.completed.usage`，最终 `turn.finished completed`；record stream 带
  `seq`/`agentPath`/out-of-band attribution。
- 本机模型出口确实返回过 **HTTP 403 Forbidden**（Codex 记 `error`/Reconnecting）；
  该错误映射为 `degraded` status，不污染 turn 终态。10s timeout 的运行曾在收到
  `turn.completed` 前超时；25s 运行完成。`turn.completed` 到达后 sidecar 主动回收
  lingering 的 Codex 进程组，不再等 CLI 的 transport teardown。
- **Codex 真链路在 OAR record 层面已绿**（真实模型回答 + usage + completed，无 fake）。
  W11-2 仍未勾：专用渲染器 UI（`keel.ui.tool-renderer`）和宿主任务图闭环属于 W12/W11
  后续，不在本切片。

缺失 Codex 的 typed 路径：

```text
$ go run ./internal/oar/cmd/oar-demo -runtime=codex -sidecar=/tmp/keel-oar-w09 \
    -journal=/tmp/oar-journal-missing -codex-path=/nonexistent/codex -timeout=5s
start: oar: runtime_unavailable (runtime codex): codex --version failed:
  fork/exec /nonexistent/codex: no such file or directory
```

`TestServiceCodexUnavailableIsTyped` 断言：preflight 失败时 `service.PID()==0`，没有
半拉起的 sidecar。

fake codex 路径（`TestServiceCodexAdapterWithFakeCodex`）：测试内生成确定性 shell
`codex`（`--version`、`exec --help`、`exec --json` JSONL），断言 call/result/
`agent_message`/usage(total=14)/attribution/最终 completed 全部映射正确。取消路径
（`TestServiceCodexCancelReapsProcessGroup`）让 fake codex 在独立进程组内 spawn
`sleep 300`，cancel 后断言 child pid 与 codex 组均消失。

## 5. 进程回收证据

- `TestBridgeReapsSidecarProcessGroup`：fake sidecar `-spawn-child -child-pid-file`，
  `Bridge.Close()` 后 child 与 sidecar pid 都在 5s 内消失（gateway 进程组阶梯）。
- `TestServiceCodexCancelReapsProcessGroup`：codex shell + `sleep` 子进程，cancel 后
  child pid 5s 内消失。
- `TestBridgeFakeSidecarCrashesCleanly`：fake sidecar `-crash-after=2` 直接 `os.Exit(3)`，
  host 观察到连接终止并可幂等 Close。
- 真实 Codex 跑完/超时后 `pgrep -fal 'codex exec|keel-oar'` 无残留（仅匹配到执行命令的
  shell 自身）。

## 6. 未完成 / 顺延

- **W11-2 未勾**：Codex 真链路受本机 403 网络限制，未拿到成功 turn；专用渲染器由 W12
  UI 槽位实现，本切片只交 projection + fixture。
- Claude 适配器、OAR marketplace 安装（当前 loader 仅 tool_provider sidecar）、完整
  W11 任务图证明（外部调用归属发起卡的宿主闭环）顺延。
- `plugins/keel.oar/plugin.yaml` 是目标清单；当前安装会因 provider kind 不匹配被
  harnessext 拒绝，README/EXTENSIBILITY.md 已显式标注。
