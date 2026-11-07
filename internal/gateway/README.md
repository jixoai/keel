# internal/gateway —— 外部 provider 网关

> Keel 与「进程外能力」之间的协议与传输底座：**ToolProvider**（无状态工具）与
> **AgentBackend**（整机外部 agent runtime，如 DSH sidecar）。
> 本包只做传输 + 协议 + 隔离，**不接账本、不建 run、不持有 lease**；DSH sidecar
> 本身的实现不在本包内（见 §7 契约）。

依据：`openspec/changes/archive/keel-service-graph/specs/external-gateway/spec.md`（含 2026-11-07
P0 收敛）与 `specs/ecosystem-adapters/spec.md`。协议版本常量：`ProtocolVersion = "2026-11-07"`。

---

## 1. 协议方法

传输、帧、请求生命周期（id 关联 / 取消通知 / 超时）复用 MCP；**方法语义由本协议版本定义**，
不伪装成 MCP 标准方法。一个连接**只允许一种 provider kind**，两套方法面互斥，串用直接报
`*ProviderTypeError`（不是静默回退）。

### 1.1 握手（两种 kind 共用）

| 方法 | 方向 | 说明 |
|---|---|---|
| `initialize` | 客户端 → provider | 协商 `protocolVersion`、provider 类型、能力集；provider 返回 `provider{type,stateOwner,recovery,metering}` 与 `capabilities` |

- 版本不兼容 ⇒ 握手失败，连接**不激活**（`HandshakeError{Code: unsupported_protocol_version}`，
  错误里带 provider 的版本与本地支持列表）。
- 本协议**没有** `notifications/initialized`：`initialize` 的响应即握手完成。
- 拒绝规则：`tool_provider` 必须声明 `capabilities.tools`；`agent_backend` 必须声明
  `capabilities.agent`；`tool_provider` 不得声明会话状态字段；未知 `stateOwner`/`metering` 拒绝。

### 1.2 工具面（仅 `tool_provider`，MCP 形状逐字复用）

| 方法 | 说明 |
|---|---|
| `tools/list` | 工具目录；客户端跟 `nextCursor` 翻页（上限 1000 页 + 去重游标） |
| `tools/call` | `{name,arguments}` → `{content[],structuredContent?,isError?}` |

工具调用结果**回到调用方**，由内核 `Commit` 记账；provider 永远拿不到账本写权限。

### 1.3 Agent 面（仅 `agent_backend`，本协议私有版本化方法集）

| 方法 | 方向 | 说明 |
|---|---|---|
| `session.open` | 请求 | 开一个外部会话 → `{sessionId,stateOwner,recovery,metering,lastSeq,…}` |
| `session.resume` | 请求 | 恢复已有会话 → `{sessionId,resumed,lastSeq,…}`；`resumed=false` ⇒ 会话进入 `backend-unavailable` |
| `message.send` | 请求 | `{sessionId,message}` → `{turnId?}`（一次 send 不是 Keel 的 run） |
| `event.stream` | **请求**（带 `id`）= 订阅：`{sessionId,fromSeq?}` → `{streamId,cursor}` | 同一方法名**不带 `id`** 时是事件通知，`params` 即 `AgentEvent` |
| `cancel` | 请求 | `{sessionId,turnId?,reason?}`：取消某个 turn |
| `health` | 请求 | → `{status: ok\|degraded\|unavailable, version?, detail?}` |
| `close` | 请求 | 带 `sessionId` 关会话；不带则请求 backend 优雅停机 |

- 请求级取消（中止一个在途 JSON-RPC 请求，包括 `tools/*`）仍用 MCP 生命周期通知
  `notifications/cancelled`：它属于「复用 MCP 的请求生命周期」，不属于方法面。turn 级取消
  用 agent 面的 `cancel`。
- 事件里的 `type`/`data` 由 backend 定义，网关原样、按序透传；`usage` 只在 backend 上报时存在。

### 1.4 错误映射

| Go 类型 | 语义 |
|---|---|
| `*RPCError` | provider 返回的 JSON-RPC 错误（`Code` 见 `Code*` 常量，含保留段 `-32001 timeout / -32002 canceled / -32003 unavailable`） |
| `*TimeoutError` | 本地超时（确定错误；provider 迟到的响应被丢弃） |
| `*CanceledError` | 调用方 ctx 取消（会向 provider 发 `notifications/cancelled`） |
| `*UnhealthyError` | provider 被标记不健康 / 连接已死；后续调用**快速失败** |
| `*ConnClosedError` | 调用方已 `Close` |
| `*HandshakeError` | 握手拒绝（版本 / kind / 能力 / 标记 / 非法结果） |
| `*HTTPStatusError` | Streamable HTTP 非 2xx（`Body` 是**有界**摘录） |
| `*FrameError` | bug 帧：`malformed`/`too_large`/`desync`；`Direction=outbound` 表示拒发超限消息 |
| `*ProtocolError` | 合法 JSON 但不是合法 JSON-RPC / 结果形状不符 |
| `*TransportError` | 读写失败、传输被关闭 |

---

## 2. 帧格式

- **换行分隔 JSON**（MCP stdio 的帧格式）：一线一帧，紧凑 JSON，UTF-8。
- 边界规则：字符串外的换行**一定**结束帧；顶层容器闭合也结束帧（兼容漏写换行、`{}{}` 连写）。
  因此**坏帧只损失自己那一帧**（`坏帧隔离`），绝不会吞掉后续帧；格式化的多行 JSON 会被逐行
  拒绝并记录诊断（与 MCP 自身实现一致，README 明示不支持多行帧）。
- 帧上限 `DefaultMaxFrameBytes = 16MiB`（与 `internal/mcp` 一致），**双向**生效：
  - 入向：超限帧在**流仍在传**时就被切断（错误发生在 limit+1 字节处，而不是等对方传完），
    然后丢弃到帧边界并记诊断；若在预算内找不到边界 ⇒ `FrameDesync`（粘性致命错误，连接判死）。
  - 出向：超限消息**不写**（不会在流上留下半帧），返回 `*FrameError{Direction: outbound}`。
- 峰值内存：`frameReadBuffer(32KiB) + 一帧(≤limit) + 解码后的消息(≤limit)`，无其它缓冲。
- 读取是增量的：解析在字节到达时就开始，按 32KiB chunk 消费，不等整帧。

## 3. 背压模型

一条链：**provider 写 → 传输读 → 有界队列 → 连接/调用方**。

| 环节 | 机制 |
|---|---|
| 入向队列 | `messageQueue`：**同时**按条数（默认 64）与字节数（默认 16MiB）设限；`push` 越界即阻塞 |
| 慢消费者 | 队列满 ⇒ 读循环阻塞 ⇒ 不再读 socket/pipe/TCP ⇒ provider 的写阻塞。全程无界缓冲 = 0 |
| 出向 | stdio 用**独立 writer goroutine + 有界出站队列**（条数/字节同界）：队列满则 `Send` 阻塞（背压）；provider 不读 stdin 时 `Send` 仍能被 ctx 真正解阻，不会把调用方卡死在写锁上 |
| 写看门狗 | 一帧写超时（`WriteTimeout`，默认 10s）：流支持写 deadline（`os.File` 管道、`net.Conn`）则由 deadline 解除阻塞，否则由计时器关闭流；两种情况都把链路判死并对在途调用返回确定错误，而不是永久挂住 |
| 事件通道 | `Conn.Events()` 有界（默认 16；`EventsBuffer<0` 选严格模式 = 无缓冲）。消费停 ⇒ 收包循环停 ⇒ 反压回到 provider |
| HTTP | 响应体/SET 流由后台消费者读；队列满 ⇒ 停止读 body ⇒ TCP 窗口回压到服务端 |
| 模型选择 | JSON 响应与 SSE 响应都只经**同一个**有界队列；`Stats()` 暴露 `Queued/QueuedBytes/Peak*` 供监控与测试断言 |

取消/超时是**例外路径**：`TimeoutError`/`CanceledError` 立即返回确定错误，取消通知由独立的
短超时 goroutine 尽力送达（不阻塞调用方）。

## 4. 两类外部服务与外部会话数据模型

### 4.1 ToolProvider

无状态；`ListTools`/`CallTool`；结果交给调用方记账。路由在 `Conn.Provider().Type` 上强制校验。

### 4.2 AgentBackend（外部会话）

会话是**外部会话**：backend 拥有状态。网关给出、由调用方落库的标记（`ExternalSession.Markers()`）：

| 字段 | 取值 |
|---|---|
| `state_owner` | `external`（P0 数据模型；backend 只有在**自称** keel 时才不是） |
| `external_backend` | 握手得到的 backend 名 |
| `external_ref` | backend 的会话语柄（`ExternalSession.SessionID`，对 Keel 不透明） |
| `external_status` | `opening` / `active` / `backend-unavailable` / `closed` |
| `metering` | `external` / `none` |

纪律（P0 §4）：

- 外部会话**不进**原生 run/lease 作业流；`message.send` 不产生 run。
- Keel 的 SSE **只回放 Keel 记录的事件**；backend 事件需要调用方自行经内核提交，网关不会把它们
  直接塞进 Keel 的流。
- `backend-unavailable` 是**可查询终态**：连接的 `health` 或连接死亡后，`AgentClient.Status(id)`
  对未显式关闭的会话一律返回 `backend-unavailable`；`Sessions()` 给出本地已知快照。
- 双账本只允许以「外部会话 + 兼容标记」出现，不得伪装成原生恢复。

### 4.3 usage 计量边界（P0 §3）

- 网关**从不**合成 usage：`Usage` 只来自 backend 的事件；缺失 = `nil`（不可计量）。
- 会话的 `metering` 默认 `none`；只有 backend 声明 `capabilities.agent.usage=true`（或显式声明）
  才是 `external`。**禁止静默记 0**，也禁止把外部用量当原生 provider 用量。
- 配额策略（是否放行、如何限流）属于 loadout/租户配置，不在本包。

## 5. 安全边界

- **无账本访问**：本包运行时代码不得 import `internal/store`、`internal/kernel`、`internal/durable`、
  `internal/mcp` 或 `pgx`（`boundary_test.go` 是机器门）；外部进程经由本包可达的只有 JSON-RPC 方法，
  没有租户/lease/账本句柄，`external_ref` 对 Keel 是不透明串。
- **凭据不落明文**：`redact.go` 提供 `IsSensitiveKey`/`Fingerprint`/`RedactValue`/`RedactJSON`/
  `RedactText`/`RedactURL`。密钥只从租户配置/密钥引用解析后注入 header 或 env；日志出现的是引用名或
  `sha256:` 指纹。**诊断信息只含长度与错误文本，从不出现在帧字节或消息正文**（有测试断言）。
- **调用审计**：`ConnOptions.OnCall` 每次外部调用一条 `CallRecord`（provider/method/target/耗时/状态/错误码）。
  该结构**没有**可承载 payload 的字段——审计不可能顺手记下参数。
- **env/凭据清洗（fail-closed）**：`SpawnStdio` **拒绝 `Env == nil`**（否则子进程会继承整个宿主环境：
  数据库口令、frp token、无关 provider 的 API key），并要求每条目形如 `NAME=value`；安全起点是
  `MinimalEnv(os.Environ())`（仅 PATH/HOME/LANG 等进程基础变量），再由适配器显式追加 provider
  真正需要的变量。`RedactEnv` 供日志脱敏。空 `[]string{}` 是显式决定（不注入任何变量），与 nil 区别对待。
- **进程树与 reaper**：spawn 的 provider 成为**独立进程组组长**（`Setpgid`），`Close` 走
  「关 stdin → 宽限 → 组 SIGTERM → 组 SIGKILL」并逐级确认**整组已消失**（只杀直接子进程会把
  Node/DSH 的孙进程留成孤儿）；`KillTree(pid)` 是对外暴露的 reaper 原语，供 supervisor 在
  sidecar 崩溃后用 pidfile 里的 pid 清理整棵遗留进程树。
- **握手收紧**：能力/类型/标记不合法一律拒绝激活，不允许「先连上再说」。

## 6. 传输

| 传输 | 说明 |
|---|---|
| `StdioTransport` | 任意 `io.ReadWriteCloser`（`net.Pipe`/`io.Pipe`/子进程管道）。读循环 + **writer goroutine**（有界出站队列、`WriteTimeout` 看门狗）；`SpawnStdio(SpawnOptions)` 拉起子进程：**要求显式 Env**、独立进程组、stdin/stdout 走协议、stderr 直通；`Close` 走停机阶梯：关 stdin → 宽限 → **组 SIGTERM → 组 SIGKILL**（`CloseGrace` 可调），并回收进程状态（`ExitState()` 用于判定崩溃） |
| `HTTPTransport` | Streamable HTTP：每消息一个 POST，响应可为 `application/json`（单帧或数组）或 `text/event-stream`；捕获/回传 `Mcp-Session-Id`、`MCP-Protocol-Version`；AgentBackend 在握手后由连接层打开 GET 事件流（`EventStreamStarter`），SSE 事件按 `Last-Event-ID` 续传、有界重连退避；`Close` 尽力 `DELETE` 会话 |

超时与取消：`ConnOptions.RequestTimeout`（默认 60s，`<0` 关闭计时器）配合 ctx；超时/取消都会
向 provider 发 `notifications/cancelled` 并返回确定错误类型。

## 7. DSH sidecar 契约（适配器工作流必读）

> 本包**不含** DSH sidecar 实现；以下是 sidecar/工具桥适配器必须满足的契约，网关为其提供
> `SpawnStdio`/停机阶梯/健康/退避工具。

**版本 pin（硬要求）**

- `@deepseek-ai/dsh@0.2.1-rc.5`（精确版本，禁止 `latest`/semver 区间），Node **22**。
- 升级必须走兼容矩阵 + 回归测试（AgentsUI/工具面/取消/事件顺序），不得自动跟随上游。

**生命周期（谁拉起、谁停止）**

- 拉起：上层 supervisor（service-graph 装配）用 `SpawnStdio` 起进程，**env 只放白名单**（见下）；
  协议面用 `Conn.Connect` 握手，`provider.type=agent_backend`。
- 停止：顺序 = `Conn.Close()`（拒绝新调用、失败在途）→ `StdioTransport.Close()`（关 stdin 宽限 +
  **进程组** SIGTERM + 进程组 SIGKILL，并确认整组消失）；sidecar **必须**在 stdin EOF 时自行退出
  （这是父进程被 SIGKILL 时的第一道孤儿防线），进程组是第二道，`KillTree` + pidfile 是第三道。
- 健康：`health` 方法（业务可用性）+ 进程存活（`PID`/`Done`/`ExitState`）双轨；两者都进审计。

**env / 凭据清洗**

- 宿主环境一律不继承，且这是**机器强制**的：`SpawnStdio` 拒绝 nil Env；起步用
  `MinimalEnv(os.Environ())`，需要什么就显式 `append`（模型密钥按租户 provider 配置单独注入），
  需要更宽的集合时用 `FilterEnv(base, ...)` 自己列白名单。
- 日志用 `RedactEnv`；协议日志用 `redact.go` 的助手。禁把 token 写进命令行参数（进程表可见）。

**reaper（防孤儿）**

- 记录 pidfile（含启动时间/命令与**进程组 id**）供宿主重启后清理；sidecar 自身在 stdin EOF 或收到
  `close` 后必须退出；`Close` 之后 supervisor 必须确认进程已回收（`Transport.Done()`）再放行新实例；
  宿主机重启/崩溃后遗留的进程树用 `KillTree(pid)` 清理。

**崩溃退避**

- `RestartPolicy`（初始 250ms、×2、上限 30s、抖动、默认 5 次预算）计算重启等待；
  预算耗尽 ⇒ 该 backend 会话标 `backend-unavailable` 并上报，不无限重启（禁止 crash loop）。
- 重启成功后会话走 `session.resume`；`resumed=false` ⇒ 仍为 `backend-unavailable`（不假装恢复）。

**资源预算**

- sidecar 要有 CPU/内存上限（cgroup/ulimit）与并发会话上限；`2GB/1000 会话` 基线不得因 sidecar 退化。

**DSH 纯工具桥（不是零代码）**

- 需要一个 **DSH 侧导出插件**把工具暴露成 `tool_provider` 面（`tools/list`/`tools/call`），
  **必须**处理：工具白名单、参数/结果 JSON 可序列化校验（不可序列化 ⇒ 拒绝暴露 + 可读诊断）、
  错误映射、取消、输出裁剪。
- 网关上不需要、也**不承诺** DSH UI/native/Electron 插件；禁止把 Cordis `ctx` 逐对象 RPC 化。

**兼容矩阵（native / external 两种模式）**

| 能力 | native（Keel 账本） | external（AgentBackend） |
|---|---|---|
| 工具调用记账 | 内核 `Commit`，唯一真值 | 调用方按需提交；backend 事件≠Keel 事件 |
| skills/资源 | 原生 | 支持：整机 backend 内沿用 |
| LLM adapter | 原生 | 部分（backend 内自持） |
| hooks/扩展 | 原生 | 有条件：需事件映射 + conformance |
| 会话恢复 | kill -9 后从事件日志重建 | **不承诺**：`backend-unavailable` + `session.resume` |
| Web/Desktop UI、Electron/native、本机路径插件、DSH plugin manager/HMR | — | **不支持** |

## 8. 测试与使用

```bash
GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=go1.27.3 go test -race ./internal/gateway
GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=go1.27.3 go vet ./internal/gateway
```

测试全部在进程内（`net.Pipe` / `httptest` / 测试二进制自举子进程），不依赖外网与外部工具。
重点覆盖：大帧分块与峰值内存上界（死循环流证明切断）、慢消费者背压（条数/字节双上界）、
坏帧隔离与重同步、peer 崩溃、超时与取消通知、握手拒绝、两套方法面隔离、外部会话标记与
`backend-unavailable` 终态、usage 不合成、secret 脱敏、边界 import 门；写路径另覆盖
「provider 停读时取消能解阻 + 出站队列有界 + 写看门狗判死链路」，spawn 覆盖
「nil Env 拒绝 / 宿主密钥不下传 / 进程组信号与孙进程回收 / 崩溃后无 Close 也能读到 EOF」。

最小用法：

```go
conn := gateway.NewConn(gateway.ConnOptions{})
handshake, err := conn.Connect(ctx, transport)   // stdio 或 HTTP
tools := gateway.NewToolProvider(conn)           // 工具面
agent := gateway.NewAgentBackend(conn)           // agent 面（互斥）
session, err := agent.Open(ctx, gateway.SessionOpenRequest{Reference: "opaque-ref"})
markers := session.Markers()                     // state_owner/external_backend/external_ref/...
```
