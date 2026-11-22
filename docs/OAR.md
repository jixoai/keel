# OAR — 外部运行时与带外记录流（契约 v1）

> 冻结：2026-11-22（W9 `keel-harness-v2`）。实现：`internal/oar/**`、`plugins/keel.oar/**`。
> 变更纪律：本文件是契约正文；协议帧/schema/码表/进程回收/网关复用关系冻结在这里。
> 改契约走新 change，不得只改实现。

## 1. 定位

OAR 是 **外部工具** 的适配契约，不是第二个 agent 运行时，也不是第二套外部会话模型。

```
Keel Session (账本真值)
   │  external.backends / AgentService（既有 externalgw）
   ▼
外部会话 state_owner=external ── agent_backend 协议（internal/gateway）──┐
   │                                                                     │
   │  event.stream: type=oar.record, data=Record                          │
   ▼                                                                     │
OAR 记录桥（internal/oar）── NDJSON journal（durable）── oar-view / Projection
                                                                         │
                                              OAR sidecar（keel-oar）────┘
                                                   │
                                                   ├─ fake：确定性测试运行时
                                                   └─ codex：codex exec --json（真实模型）
```

- 内部 Keel Bot：真 agent，有任务卡。
- Codex/Claude/DSH：外部工具，**不建子卡**；它的调用归属发起它的任务卡。
- OAR 只负责：能力协商、带外 record stream、typed rejection、进程组回收、渲染投影。
- **不给外部运行时注入「请记录任务卡」**。归属由 `session.open` 的带外
  `attribution` 提供，并在每条记录上回显；prompt 不含任何此类指令。

## 2. 协议与网关复用

OAR sidecar 说 **既有 external-gateway agent_backend 协议**（`internal/gateway`）：

| 方法 | OAR 用法 |
|---|---|
| `initialize` | 协商网关版本/能力；OAR 宣言放在 `capabilities.experimental.oar` |
| `session.open` | 开外部会话；`config.attribution` 带外归属，`config.workdir/model/sandbox` 交给运行时 |
| `session.resume` | 运行时自行声明；fake 支持，codex v1 不支持（`resume=false`） |
| `message.send` | 一次 turn；OAR 不修改 prompt |
| `event.stream` | 订阅；每帧 `type=oar.record`，`data` 是一条 Record |
| `cancel` | 取消 turn；sidecar 杀运行时进程组 |
| `health` | sidecar 健康 + runtime 可用性 |
| `close` | 有 `sessionId` 关会话；无 id 优雅停机 |

复用既有约定：`state_owner=external`、`external_status=opening|active|backend-unavailable|closed`、
`metering=external|none`、原生 `/input` 409、`external.event` 投影。**不新增网关方法、不改网关帧格式。**

### 2.1 initialize 的 OAR 宣言

```json
{
  "protocolVersion": "2026-11-07",
  "capabilities": {
    "agent": {"events": true, "cancel": true, "usage": true, "streaming": false},
    "experimental": {
      "oar": {
        "schema": "keel.oar.capabilities/1",
        "contractVersion": "1",
        "runtime": "codex",
        "runtimeVersion": "codex-cli 0.166.0",
        "simulated": false,
        "capabilities": ["records", "agent_path", "calls", "results", "status",
                          "messages", "usage", "cancel", "live_model"]
      }
    }
  },
  "provider": {"type": "agent_backend", "stateOwner": "external",
               "recovery": "none", "metering": "external"}
}
```

缺 `experimental.oar`、schema/版本不符、能力名不在闭集、重复能力、模拟运行时声明
`live_model`、缺基础能力集（`records/agent_path/calls/results/status`）都会在
**打开任何会话之前**被拒绝（typed，见 §7）。

## 3. 运行时能力矩阵

| 能力 | codex | fake | 含义 |
|---|---:|:---:|---|
| `records` | ✓ | ✓ | record stream 本体 |
| `agent_path` | ✓ | ✓ | 每条记录带 `agentPath` |
| `calls` | ✓ | ✓ | 调用开始记录 |
| `results` | ✓ | ✓ | 与 `callId` 关联的结果 |
| `status` | ✓ | ✓ | turn/session 生命周期、degraded |
| `messages` | ✓ | ✓ | assistant/reasoning/error 文本 |
| `usage` | ✓ | ✓* | token 用量；缺失=不可计量，绝不补 0 |
| `cancel` | ✓ | ✓ | turn 取消 |
| `resume` | ✗ | ✓ | 同一 sidecar 生命周期内按 seq 续订 |
| `streaming` | ✗ | ✗ | 增量 token（v1 不做；Codex exec JSONL 是事件流，不是 token delta） |
| `subagents` | ✗ | ✓ | 子 agent 路径 |
| `live_model` | ✓ | ✗ | 真实模型；fake 永不声明 |

`*` fake 只有显式 `-usage` 才声明/上报用量。

宿主用 `oar.BaseCapabilities` + 调用方 `Required` 做启动前校验；要求 `live_model` 的宿主
拿到 fake 会得到 `simulated_runtime`，**不会**把 fake 当真实模型用。

## 4. Record schema

```json
{
  "schema": "keel.oar.record/1",
  "seq": 3,
  "agentPath": "/main",
  "sessionId": "oar-1",
  "turnId": "turn-1",
  "kind": "result",
  "callId": "call-1",
  "name": "shell",
  "status": "completed",
  "output": {"stdout": "OAR-OK", "exitCode": 0},
  "attribution": {"sessionId": "<keel-session-uuid>", "taskId": "<task-card-id>"},
  "runtime": "fake",
  "ts": "2026-01-01T00:00:00.003Z"
}
```

| 字段 | 规则 |
|---|---|
| `schema` | 固定 `keel.oar.record/1` |
| `seq` | **每后端会话严格递增**，跨所有 agentPath；重放同 seq 同字节幂等，seq 回退/复用不同内容=typed `record_order`；允许缺口（崩溃可丢在途记录） |
| `agentPath` | `/segment(/segment)*`，根 `/main`，子 agent `/main/worker-1`；拒绝 `..`、空段、尾斜杠 |
| `sessionId` | 后端会话语柄（gateway 的 opaque id） |
| `kind` | 闭集 `call|result|status|message|usage|error` |
| `callId` | call/result 必填，结果与调用配对 |
| `status` | `started|running|degraded|completed|failed|cancelled` |
| `input`/`output` | 任意 JSON；单条记录编码 ≤1 MiB，超出=typed `record_too_large` |
| `attribution` | **带外归属**；sidecar 原样回显，宿主缺省填充；OAR 不据此建卡、不改 prompt |
| `ts` | RFC3339Nano；fake 用固定 epoch 派生，测试可复现 |

### 4.1 不注入任务卡

- `message.send` 的 prompt 原样交给运行时；sidecar 不追加任何 system/developer 文本。
- `session.open` 只传 `attribution` 与运行时配置。
- 测试 `TestBridgeFakeSidecarRecordStream` 扫描所有记录，断言没有
  `task card` / `任务卡` / `please record` 字样。

## 5. 进程模型与回收

```
host (gateway.SpawnStdio)
  └─ sidecar 进程组（Setpgid，leader=sidecar pid）
       ├─ fake child（测试用，继承 sidecar 组）
       └─ codex turn（自己一组：shell → 子进程）
```

| 场景 | 回收路径 |
|---|---|
| 正常 `close` / `Service.Stop` | gateway 停机阶梯：关 stdin → 宽限 → 组 SIGTERM → 组 SIGKILL，确认整组消失 |
| sidecar 崩溃 | host 的 `Conn` 观察到 EOF/退出；`Service.Stop` 仍走组关闭 |
| host 崩溃（stdin EOF） | sidecar 读循环结束 → `shutdown()` 杀 codex 组与测试 child |
| sidecar 被 SIGKILL | `-codex-pid-file` 记录的 pid → host `gateway.KillTree(pid)` 兜底；fake child 随 sidecar 组一起死 |
| cancel | sidecar 对 codex 进程组 SIGTERM，2s 后 SIGKILL；turn 记 `cancelled` |
| codex 提前退出 | `Wait` 后仍对组 `SIGKILL`，确保 shell 孙进程不遗留 |

证据测试：`TestBridgeReapsSidecarProcessGroup`、`TestServiceCodexCancelReapsProcessGroup`、
`TestGateway...`（网关既有）；测试结束断言 pid 不再存活。

## 6. 持久 journal、CLI 与渲染 seam

- Host `Journal`：每个后端会话一个 `session-<id>.ndjson`，append-only；默认每次
  `fsync`；重启后从最后一个记录恢复 seq；同 seq 同字节幂等。
- 开发视图：`go run ./internal/oar/cmd/oar-view -journal <file|dir>`；`-format json`、
  `-agent`、`-kind`、`-after`、`-session` 过滤。
- 渲染 seam：`oar.Projection`（`keel.oar.render-projection/1`）把 call/result 折叠成
  一行一项，保留 status/message/usage/error 顺序；**W12 UI owns the slot**，本包不建
  web renderer。固定 fixture：`internal/oar/testdata/tool-renderer.fixture.json`
  （`plugins/keel.oar/fixtures/tool-renderer.fixture.json` 同文件副本）。
- 重生成 fixture：`UPDATE_OAR_FIXTURE=1 go test ./internal/oar -run TestProjectionFixture`。

## 7. Typed 码表

| code | 触发 |
|---|---|
| `oar_disabled` | 未配置 OAR；`NewService` 返回并包 `ErrDisabled` |
| `runtime_unavailable` | codex 未安装/不可执行；sidecar 不存在；spawn 失败 |
| `runtime_unsupported` | codex 存在但 `exec --json` 不可用 |
| `invalid_capability` | 能力名越界/重复/声明自相矛盾（如 simulated+live_model） |
| `unsupported_capability` | 能力协商通过但缺少调用方必需位 |
| `simulated_runtime` | 要求 `live_model` 但运行时 `simulated=true` |
| `invalid_record` | 记录 schema/字段/agentPath 非法，或归属与 open 不一致 |
| `record_order` | seq 回退，或同 seq 不同字节 |
| `record_too_large` | 单条记录超过 1 MiB |
| `unknown_session` | 记录/调用指向未 open 或已关闭的会话 |
| `protocol` | 握手/OAR 宣言缺失、运行时不匹配等线级错误 |

Go 类型：`*oar.Error{Code, Runtime, Detail, Required, Declared, Err}`；`oar.CodeOf(err)`、
`oar.IsCode(err, code)`、`oar.IsDisabled(err)`。

## 8. Codex 适配器

- `oar.CodexPreflight`：解析 `codex`（PATH 或显式路径）→ `codex --version` →
  `codex exec --help` 必须含 `--json`。缺失/过旧分别为 `runtime_unavailable` /
  `runtime_unsupported`。
- sidecar `-runtime=codex` 每次 turn 执行：
  `codex exec --json --skip-git-repo-check --ephemeral --color never
   [--sandbox read-only] [-C workdir] [-m model] -- <prompt>`
- JSONL 映射：`thread.started`/`turn.started` → status；`item.started` → call；
  `item.completed` → result（失败带 error）；`agent_message`/`reasoning` → message；
  `turn.completed.usage` → usage；`turn.failed` → error；`error`（含 Reconnecting）→
  degraded status；未知事件 → `codex.<type>` status + 原始输出（有界），不丢行。
- 单条输出超过 `-max-output-bytes`（默认 64 KiB）时写成
  `{"truncated":true,"bytes":N,"preview":"…"}`，记录不超 1 MiB。
- **一个 sidecar 同时只跑一个 Codex turn**：`-codex-pid-file` 是单值，并发会话的 turn
  在 sidecar 内排队；排队中的 turn 在获取执行权前先检查取消。
- **不静默 fake**：`runtime=codex` 但二进制缺失时初始化失败；`runtime=fake` 声明
  `simulated=true`，要求 `live_model` 的调用方会被拒绝。

### 8.1 fake sidecar

- `keel-oar sidecar -runtime=fake [-usage] [-step-delay=…] [-crash-after=N]
  [-spawn-child] [-child-pid-file=…]`。
- 确定性记录：`/main` 的 shell call/result、`/main/worker-1` 的 read_file call/result、
  agent_message、可选 usage、turn 状态；时间戳由 `-fake-epoch`（默认
  2026-01-01T00:00:00Z）+ seq×1ms 派生。
- 永远 `simulated=true`，永不 `live_model`。

## 9. 宿主接入

### 9.1 独立 OAR 服务（本切片）

```go
journal, _ := oar.NewJournal(dir, oar.JournalOptions{})
bridge, err := oar.Dial(ctx, oar.SpawnOptions{
    Command: "keel-oar", Args: []string{"sidecar", "-runtime=codex"},
    Runtime: oar.RuntimeCodex, Required: []oar.Capability{oar.CapabilityLiveModel},
    Journal: journal,
})
session, _ := bridge.Open(ctx, oar.OpenRequest{
    HostSessionID: keelSessionID, TaskID: taskCardID, Workdir: workdir,
})
bridge.Send(ctx, session.Ref, json.RawMessage(`{"text":"…"}`))
```

或 `oar.NewService(oar.Config{...})` → `Start` → `Open/Send/Cancel/CloseSession` → `Stop`。
`oar.Register` 是留给 `cmd/keel/serve.go` / `cmd/runtime` 的最小注册缝（本切片不改装配）。

### 9.2 externalgw 模式（既有网关，W11 接线）

`Config.ExternalBackendConfig()` 产出 `host.ExternalProviderConfig`（stdio、
`keel-oar sidecar …`），直接放进 `host.ExternalConfig.Backends`，由既有
`externalgw.AgentService` 负责 open/send/cancel、进程监督与 `external.event` 投影。
记录 (`type=oar.record`) 原样进入外部事件（`data` 含完整 Record）；OAR journal 只是
开发视图。两种模式共用同一 sidecar、同一协议、同一会话标记，不存在第二套外部会话模型。
测试 `TestExternalGatewayProjectsOARRecords` 覆盖这条链路（`externalgw` 源码零改动）。

## 10. 测试与证据

```bash
go test ./internal/oar/... -count=1
go test -race ./internal/oar/... -count=1
go vet ./internal/oar/...
```

覆盖：能力矩阵/typed rejection、record schema、seq 顺序与幂等、journal 重开、投影折叠、
fixture 稳定、fake sidecar 端到端、attribution、无任务卡注入、进程组回收、sidecar 崩溃、
Codex 缺失 typed、fake codex 端到端、取消杀组。证据：
`openspec/changes/keel-harness-v2/evidence/w09-oar.md`。

## 11. 本切片不做（W11 前）

- Claude 适配器；OAR marketplace 打包（`plugins/keel.oar` 目前是目标包，当前插件
  loader 只接受 tool_provider sidecar，agent_backend 需 W12 支持）。
- 完整 W11 任务图证明（外部调用→发起卡归属→UI 渲染的宿主闭环）。
- token 级 streaming、approval 桥、跨 sidecar 生命周期的 resume。
