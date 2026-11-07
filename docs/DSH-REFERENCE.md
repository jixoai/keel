# DSH 参考适配器与手测手册（W5）

> 本文是 `keel-service-graph` W5 的外部网关交付说明：**Keel 侧**如何声明、连接、
> 驱动一个 DSH 风格的 AgentBackend，以及**真实 DSH** 的手测步骤、预期输出和失败排查。
>
> 结论先行：
> - 自动化路径 = 仓库内 `cmd/keel-dsh-ref`（最小参考 sidecar）+ `internal/externalgw`
>   测试 + `scripts/external-gateway-smoke.sh`（15 步、含进程树回收断言）。
> - 真实 DSH 路径 = Node + `@deepseek-ai/dsh@0.2.1-rc.5`，需要 DSH 侧一层协议桥
>   （DSH SDK JSON-RPC ↔ Keel AgentBackend），并需要模型凭据；因此**不进 CI**，按本文手测。

## 1. Keel 侧协议（适配器必须实现的方法集）

版本常量：`gateway.ProtocolVersion = "2026-11-07"`。传输：stdio 新行分隔 JSON-RPC 2.0
（或 Streamable HTTP）。一个连接只允许一种 provider kind。

| 方法 | 方向 | 说明 |
|---|---|---|
| `initialize` | 请求 | 协商 `protocolVersion`、provider 类型、能力；不兼容版本直接拒绝激活 |
| `session.open` | 请求 | 开外部会话，返回 `sessionId`（对 Keel 不透明）、`stateOwner=external`、`metering`、`lastSeq` |
| `session.resume` | 请求 | 恢复会话；`resumed=false` ⇒ 会话进入 `backend-unavailable`，不假装恢复 |
| `message.send` | 请求 | 投递一条消息，返回 `turnId` |
| `event.stream` | 请求（带 id） | 订阅事件流；同一方法不带 id 时是事件通知 |
| `cancel` | 请求 | 取消一个 turn（不支持时必须明确报错，不得静默成功） |
| `health` | 请求 | `{status: ok\|degraded\|unavailable}` |
| `close` | 请求 | 带 `sessionId` 关会话；不带时请求 backend 优雅停机 |

Keel 侧行为（`internal/externalgw`）：

- 打开会话时先落 `state_owner=external` / `external_backend` / `external_ref` /
  `external_status` / `metering`（`kernel.ExternalStore`），再订阅事件流；订阅失败会关闭
  backend 会话并拒绝打开，不留孤儿。
- backend 事件经内核 **external 提交类**落成 `external.event`（payload 带
  `backend/externalRef/stateOwner/compatibility/direction/seq/type/data/usage`），
  普通 `GET /v1/sessions/{id}/events` 与 SSE 只回放这些 **Keel 已记录**的事件。
- backend 崩溃/连接死亡 ⇒ 该 backend 的所有非 closed 会话标记 `backend-unavailable`
  （可查询终态）；重连按 `gateway.RestartPolicy` 退避，重连后对已知会话尝试
  `session.resume`，`resumed=false` 保持 unavailable。
- 外部会话永不进入原生 run/lease 流；原生 `/input`、`/events` 对它们返回 409。

## 2. 自动化路径：参考 sidecar

```bash
# 构建（仓库根目录）
go build -o /tmp/keel-dsh-ref ./cmd/keel-dsh-ref

# 直接跑协议（stdin/stdout 是新行 JSON-RPC；stderr 是诊断）
/tmp/keel-dsh-ref -kind=agent -usage -prompt=ref:
```

常用开关：

| 开关 | 用途 |
|---|---|
| `-kind=agent\|tool` | AgentBackend / ToolProvider 两种方法面 |
| `-usage` | 声明并上报 token usage（`metering=external`）；缺省 `metering=none` 且无 usage |
| `-metering=external\|none` | 覆盖计量声明 |
| `-slow=<duration>` | 每个流式块延迟（取消/背压测试） |
| `-ignore-cancel` | cancel 返回成功但不停止 turn（坏 backend 测试） |
| `-crash-on-send` | 收到 message.send 即崩溃（崩溃隔离测试） |
| `-crash-after=<n>` | 发出 n 个事件后崩溃 |
| `-handshake-delay=<d>` / `-open-delay=<d>` | 握手/打开卡死（超时测试） |
| `-protocol-version=v0` | 返回不兼容版本（握手拒绝测试） |
| `-forbid-env=A,B` / `-require-env=C` | env 清洗断言：宿主密钥不得可见，显式注入必须存在 |
| `-spawn-child` | 拉起一个 `sleep 300` 孙进程（进程组回收断言） |

一键冒烟（真实 runtime + 真实 PostgreSQL + 参考 sidecar）：

```bash
scripts/external-gateway-smoke.sh
# 15 步：建库→构建→起 runtime→backend ready→孙进程存活→开外部会话→原生 /input 409
# →send→事件投影（assistant/send/usage）→查询→SSE 回放（external.event+标记）
# →cancel→close→SIGTERM→0 残留
# 期望最后一行：SMOKE PASS (15 steps)
```

配置样例：`docs/examples/external-gateway-ref.json`（同一 `host.TenantConfig` 词汇）。

## 3. 真实 DSH 手测

### 3.1 版本 pin 与安装

硬要求：`@deepseek-ai/dsh` **精确 0.2.1-rc.5**，Node **22**（本仓手测环境 Node 26
也能启动；生产按 pin 用 22）。禁止 `latest`/semver 区间。

```bash
node --version
# v22.x（本机手测为 v26.13.0，DSH 0.2.1-rc.5 可启动；生产仍按 pin）

npm install -g @deepseek-ai/dsh@0.2.1-rc.5
dsh --version
# 0.2.1-rc.5
```

> npm 11 可能因包内 `devEngines.packageManager=pnpm` 对 `npm view` 报
> `EBADDEVENGINES`；`npm install`/`npm pack` 不受影响。需要严格环境时用 `pnpm`。

### 3.2 进程/stdio 生命周期手测（无需模型凭据）

```bash
rm -rf /tmp/dsh-home && mkdir -p /tmp/dsh-home
DSH_HOME=/tmp/dsh-home DSH_TELEMETRY=0 dsh --profile sdk-minimal </dev/null
echo "exit=$?"
```

预期（本机 2026-11-06 实测）：

```text
dsh: all_proxy names a SOCKS proxy, which is not supported; connecting directly for that scheme — set an http:// or https:// proxy URL instead
exit=0
```

- stdout **零字节**（SDK 协议独占 stdout，新行 JSON-RPC 帧）；
- stdin EOF ⇒ 进程有界优雅退出，exit 0（这就是 Keel `StdioTransport` 关 stdin 后的
  第一道孤儿防线）。

> 若你的 shell 设了 `all_proxy=socks5://...`，上面那行 SOCKS 警告是 DSH 的正常诊断
> （它直连）；不影响启动，但真实模型请求需要可用的 HTTP(S) 代理或直连网络。

### 3.3 协议桥（DSH 侧非零代码）

DSH 0.2.1-rc.5 的 SDK profile 说的是 **DSH SDK JSON-RPC**（`@deepseek-ai/dsh-sdk-protocol`），
**不是** Keel 的 AgentBackend 方法集。两者差异（以 0.2.1-rc.5 的 `types.d.ts` 为准）：

| Keel AgentBackend | DSH SDK 协议 | 桥接方式 |
|---|---|---|
| `initialize` | `initialize {cwd, provider, model, reasoningEffort?, maxTokens?}` | 桥启动 `dsh --profile sdk-minimal`，先做 DSH initialize；`cwd/provider/model` 来自桥的环境配置（`DSH_PROVIDER`/`DSH_MODEL`，cwd 默认进程工作目录） |
| `session.open` | 无 open；`session/prompt {sessionId, contentBlocks}` 对未知 id 懒创建 | 桥生成/复用外部 sessionId，open 只登记；首次 prompt 创建 DSH 会话 |
| `session.resume` | `session/resume {sessionId}` | 直接映射；失败 ⇒ `resumed:false` |
| `message.send` | `session/prompt` → `{messageId}` | `turnId=messageId`；`contentBlocks=[{type:"text",text}]` |
| `event.stream`（订阅） | 无订阅请求；服务端主动推 `session.event`（全 runtime 事件） | 桥按 `sessionId` 过滤后转成 Keel `event.stream` 通知；`seq` 取 Keel 侧递增 |
| `cancel` | **无 cancel**（官方限制：客户端只能关进程放弃 turn） | 桥必须明确报错（如 `-32003 unavailable` + 可读说明），**不得**静默成功；Keel 侧会把它映射成 5xx |
| `health` | 无 health；用子进程存活 + `session.status` 判断 | 子进程存活且未退出 ⇒ `ok`，否则 `unavailable` |
| `close(sessionId)` | 无 session close | 桥只回 `{closed:true}`（DSH 会持久化会话，无关闭面） |
| `close`（无 sessionId） | `shutdown` + stdin EOF | 发 DSH `shutdown`，等进程退出 |

桥的最小职责（DSH 侧插件/独立 Node 脚本，非零代码）：

1. 以 stdio 起 `dsh --profile sdk-minimal`，**stdout 只转发协议帧**，stderr 透传到
   Keel 的 sidecar stderr（诊断，不污染协议）；
2. 实现上表映射，且 `initialize` 失败时不回 Keel 的成功结果（让 Keel 握手拒绝、
   provider 不激活）；
3. 处理 `shutdown`/stdin EOF：先发 DSH `shutdown`，再有界等待退出；
4. 不把模型密钥写进命令行参数（进程表可见）；只从 env 读。

配置样例（`docs/examples/external-gateway.json`，把 `command/args` 换成你的桥）：

```json
{
  "external": {
    "backends": {
      "dsh": {
        "transport": "stdio",
        "command": "node",
        "args": ["/opt/keel/dsh-keel-sidecar.mjs"],
        "env": {
          "DSH_PROVIDER": "env:DSH_PROVIDER",
          "DSH_MODEL": "env:DSH_MODEL",
          "DSH_API_KEY": "secret:DSH_API_KEY"
        },
        "timeoutMs": 60000,
        "maxSessions": 16
      }
    },
    "quota": {"unmetered": "deny", "maxSessionsPerBackend": 32}
  }
}
```

启动 runtime（boot 文档源）：

```bash
KEEL_DSN='postgres://postgres:<password>@127.0.0.1:13741/runtime?sslmode=disable' \
KEEL_ADMIN_KEY='<admin-key>' \
KEEL_EXTERNAL_CONFIG=docs/examples/external-gateway.json \
  go run ./cmd/runtime -addr 127.0.0.1:13740
```

预期日志：

```text
external agent backend connected backend=dsh provider=deepseek-harness-sdk-runtime ...
external agent backend ready backend=dsh transport=stdio
```

打开/发送（与参考 sidecar 完全同一 HTTP 面）：

```bash
TENANT=<tenant-uuid>
curl -s -XPOST localhost:13740/v1/external/sessions -H "X-Tenant-ID: $TENANT" \
  -H 'Content-Type: application/json' -d '{"backend":"dsh","reference":"manual-1"}'
# 201 {"sessionId":...,"externalBackend":"dsh","stateOwner":"external",
#      "externalStatus":"active","metering":"none"|"external","compatible":true,
#      "nativeRecovery":false}

curl -s -XPOST localhost:13740/v1/external/sessions/<sessionId>/messages \
  -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' \
  -d '{"text":"用一句话回答：1+1=?"}'
# 202 {"turnId":"...","recorded":true}

curl -s localhost:13740/v1/sessions/<sessionId>/events -H "X-Tenant-ID: $TENANT"
# 事件流里出现 kind=external.event、payload.direction=receive 的记录
```

### 3.4 失败排查

| 现象 | 原因 | 处理 |
|---|---|---|
| 启动日志 `provider selected "v0", supported: [...]` | 桥返回了旧协议版本 | 桥的 `initialize` 必须回 `2026-11-07`（或 Keel 支持的版本） |
| `handshake failed (transport_start)` / `exec: "node": ...` | `command` 不在 PATH 或 env 白名单没给 PATH | 用绝对路径；`SpawnStdio` 的最小 env 含 PATH/HOME |
| 后端一直 `degraded`，`Open` 503 | DSH `initialize` 失败（缺 provider/model/凭据） | 检查 `DSH_PROVIDER`/`DSH_MODEL`/密钥；看 runtime 日志的 `external call` 与 sidecar stderr |
| `external call ... status=timeout` | 模型首包慢于 `timeoutMs` | 调大 backend `timeoutMs`（上限由 HTTP 端点再兜底） |
| 收到 `-32003`/`cancel` 报错 | DSH SDK 协议无 cancel | 这是官方限制；关闭会话/进程放弃 turn，文档已登记 |
| 会话 `backend-unavailable` 且不恢复 | DSH 进程崩溃/退出，或 `session/resume` 返回失败 | 查 sidecar stderr；Keel 不伪造恢复，需重新打开会话 |
| 关 runtime 后仍有 `node`/`dsh` 进程 | 桥没在 stdin EOF 时退出，或自行 setsid 脱离进程组 | 桥必须继承进程组并在 stdin EOF/`shutdown` 后退出；必要时用 pidfile + `gateway.KillTree`；runtime 被 SIGKILL 时没有自动 sweep，见 §4 第 2 条 |
| stdout 出现非 JSON 行（握手解析失败） | 桥把日志写到了 stdout | stdout 只放协议帧，日志一律 stderr |

### 3.5 本轮实测结论（2026-11-06）

- `npm install -g @deepseek-ai/dsh@0.2.1-rc.5` 成功（616 包）；
- `dsh --version` → `0.2.1-rc.5`；
- `DSH_HOME=/tmp/dsh-home dsh --profile sdk-minimal </dev/null` → **exit 0**，stdout 0 字节，
  stderr 仅一行 SOCKS 代理提示（本机环境变量导致，非 DSH 故障）；
- 未完成：真实模型 turn（需要 DSH provider/model 凭据 + §3.3 的桥）。桥是 DSH 侧
  非零代码，未纳入本仓 CI；参考 sidecar 的自动化测试覆盖了 Keel 侧全部语义
  （`go test -race ./internal/externalgw ./internal/dshref` + 冒烟 15 步）。

## 4. 差距与已知限制（诚实登记）

| # | 差距 | 当前状态/依赖 | 补强方向 |
|---|---|---|---|
| 1 | sidecar 资源预算未在 Go 侧实施 | `maxSessions` 只限会话数；CPU/内存/文件描述符/子进程数由部署侧（容器 cgroup、ulimit、节点隔离）承担，Keel 不做运行时配额 | 在 gateway 的 spawn/transport 层接入 per-backend 预算（cgroup v2 或 rlimit），超限拒绝激活并进审计 |
| 2 | **runtime 被 SIGKILL 后，遗留 sidecar 没有任何 sweep** | 正常回收只走 `StdioTransport.Close`：关 stdin → grace → 进程组 SIGTERM → SIGKILL（`internal/gateway/stdio.go`）。进程被 SIGKILL 时这条路径不执行；`gateway.KillTree`（进程组 reaper）与 `RedactEnv` 目前只有测试调用，没有生产调用点。唯一防线是**协议约定**：父进程死亡 → stdin EOF，sidecar 必须自行退出（真实 DSH `--profile sdk-minimal` 实测 EOF → exit 0，见 §3.2；DSH 桥义务见 §3.3 第 3 条） | pidfile（记录 sidecar pid/进程组）+ 下次 runtime 启动时 sweep，用 `gateway.KillTree` 回收（需校验 pid 复用）；或交给外部 supervisor（systemd `KillMode=control-group`、容器 PID namespace + tini、k8s pod 级回收）保证父死子亡 |

风险：sidecar 若忽略 stdin EOF、自行 `setsid` 或让孙进程脱离进程组，会继续持有模型
凭据/网络连接；新 runtime 的 `maxSessions` 记账从零开始，遗留会话不计入，可能重复
计量或突破单 backend 并发上限。SIGKILL 是混沌/运维的既定输入（`cmd/chaos`），不是
理论场景。
