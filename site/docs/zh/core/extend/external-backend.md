# 接外部 backend

**外部 backend** 是拥有自己会话状态的 agent 运行时，Keel 通过版本化协议驱动它。Keel 不重造它、不 import
它，也不为它承诺原生恢复。本页覆盖协议、配置词汇、状态所有权标记、失败隔离，以及项目诚实登记的缺口。

## 1. 两种 provider，一层线协议

外部网关（`internal/gateway`）在 **stdio** 或 **Streamable HTTP** 上讲 JSON-RPC 2.0，方法集是版本化的
私有协议（`internal/gateway/protocol.go:32`）：

```go
const ProtocolVersion = "2026-11-07"           // 日期式版本，与 MCP 修订号一致
var SupportedProtocolVersions = []string{ProtocolVersion}
```

存在两种 provider，一个连接只允许一种：

- **AgentBackend** —— 外部 agent 运行时（例如 DSH 兼容 sidecar），Keel 追踪其会话但不拥有其状态；
- **ToolProvider** —— 无状态外部工具服务，其工具进入 prompt registry。

传输、帧格式与请求生命周期复用 MCP；**方法语义不属于 MCP**（`internal/gateway/protocol.go:26`）。
只有传输是共享的——别指望一个 MCP server 能回答这些方法。

## 2. 方法集（AgentBackend）

| 方法 | 方向 | 说明 |
|---|---|---|
| `initialize` | 请求 | 协商 `protocolVersion`、provider 类型与能力；不兼容版本必须拒绝激活——客户端不会激活自己说不出的修订 |
| `session.open` | 请求 | 打开外部会话；返回 backend 自己的 `sessionId`（对 Keel 不透明）、`stateOwner=external`、`metering`、`lastSeq` |
| `session.resume` | 请求 | 恢复会话；`resumed=false` 会把会话置为 `backend-unavailable`——Keel 不伪造恢复 |
| `message.send` | 请求 | 投递一条消息；返回 `turnId` |
| `event.stream` | 带 id 的请求（订阅）/ 不带 id 的通知（事件） | 订阅事件 / 推送一条事件 |
| `cancel` | 请求 | 取消一个 turn；**不能取消的 backend 必须明确报错**，绝不得报成功 |
| `health` | 请求 | `{status: ok\|degraded\|unavailable}` |
| `close` | 请求 | 带 `sessionId`：关闭该会话；不带：请求优雅停机 |

协议保留了三个错误码，供 provider 表达否则只能自造私有码的情形：`-32001` 超时、`-32002` 已取消、
`-32003` 不可用（`internal/gateway/protocol.go:48`）。

## 3. 配置：唯一一套词汇

外部 provider 声明在与其它一切相同的租户配置文档里（`host.TenantConfig.External`），绝不另造第二套
（`docs/SERVICE-GRAPH.md:147`）：

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
    "tools": {
      "docs": {
        "transport": "http",
        "url": "https://tools.example.com/mcp",
        "headers": { "Authorization": "secret:DOCS_TOKEN" },
        "timeoutMs": 30000
      }
    },
    "quota": { "unmetered": "deny", "maxSessionsPerBackend": 32 }
  }
}
```

（完整文件：`docs/examples/external-gateway.json`。）启动进程对它的处理：

- `KEEL_EXTERNAL_CONFIG` 可把 runtime 的 boot 文档指向该文件；loadout 只按文档开/关服务，不另造字段
  （`docs/SERVICE-GRAPH.md:149`）。
- `external.backends` 非空 ⇒ `external-agent` 服务激活；`external.tools` 非空 ⇒ `external-tools` 激活，
  其工具进入同一个 prompt registry（`docs/LOADOUT.md:66`）。
- **只接受密钥引用**。`env`/`headers` 的值只接受 `${NAME}`、`env:NAME`、`secret:NAME` 三种形态；
  字面密钥（包括 `Bearer <token>`）被拒绝，且永不落 schema（`docs/HOST.md:61`）。
- **环境是最小集**。stdio provider 只拿到 `MinimalEnv`（白名单：PATH、HOME 等）加它自己声明的变量
  （`internal/gateway/process.go:103`）；`RedactEnv` 擦除审计副本。参考 sidecar 带
  `-forbid-env=A,B` / `-require-env=C` 开关，可在测试里证明这一点。
- **`external.quota.unmetered` 是显式选择**。backend 声明 `external` 或 `none` 的计量口径后，租户必须
  决定：`allow` 记录 `metering=none` 并放行会话，`deny` 拒绝打开。缺失的 usage 报告永不被静默记为 0
  （`docs/SERVICE-GRAPH.md:163`）。

## 4. 状态所有权：打标记，绝不伪造

外部会话存在同一张 `sessions` 表里，但带着把她们排除在原生机制之外的标记
（`internal/store/schema.sql:32`）：

| 标记 | 含义 |
|---|---|
| `state_owner=external` | 状态归 backend 所有；Keel 只记录事实，不拥有恢复 |
| `external_backend`、`external_ref` | 是哪个 backend，以及它自己的不透明会话句柄 |
| `external_status` | 生命周期：`active`、`closed`、`backend-unavailable`（可查询终态） |
| `metering` | `external`（backend 上报 usage）或 `none`（显式不计量） |

内核强制的硬边界（fail-closed，在提交门而非 HTTP 预检里判定）：

- 外部会话**永不进入原生 run/lease 流**：不建 `runs` 行，claim 谓词按 `state_owner <> 'external'` 过滤，
  即使存在脏 run 行也不会被领取（`docs/SERVICE-GRAPH.md:208`）。
- 向外部会话写原生 edge 被 **409** 拒绝（在会话行锁内做 `NativeEdge` 判定）；
  `POST /v1/sessions/{id}/run` 同样拒绝。
- 外部提交类是对称的：`external.event` 只对「处于 external 且未关闭」的会话接受。
- **SSE 只回放 Keel 记录过的事件**。backend 的推送被投影为 `external.event` 记录（payload 带
  `backend`/`externalRef`/`stateOwner`/`direction`/…），普通事件端点只回放这些——不做 backend 帧的实时
  透传（`docs/SERVICE-GRAPH.md:159`）。

## 5. 失败隔离

生命周期归服务图：effects 反序释放、代际递增，坏 provider **只降级、不拖垮 runtime**——重连按
`gateway.RestartPolicy` 退避（`docs/SERVICE-GRAPH.md:152`）。具体而言：

- backend 崩溃或连接死亡 ⇒ 其所有未关闭会话标记 `backend-unavailable`（可查询终态，而不是挂死）。
- 重连后 Keel 对已知会话尝试 `session.resume`；`resumed=false` 则保持 unavailable。
- 无法送达的取消是错误而不是成功——API 映射为 5xx，而不是假装 turn 已停。
- 关闭 stdio provider 的流程：关 stdin → 宽限 → 进程组 SIGTERM → SIGKILL；冒烟断言**孙进程 0 残留**
  （`docs/SERVICE-GRAPH.md:168`）。

## 6. 参考实现与可复跑性

| 产物 | 是什么 |
|---|---|
| `cmd/keel-dsh-ref` | 最小参考 sidecar，实现该方法集；开关可模拟慢流、忽略取消、崩溃、握手卡死、版本不兼容、env 清洗、孙进程（`docs/DSH-REFERENCE.md:41`） |
| `scripts/external-gateway-smoke.sh` | 真实 runtime + 真实 PG + 参考 sidecar 的 15 步冒烟；期望 `SMOKE PASS (15 steps)` |
| `docs/examples/external-gateway.json` | 上面的配置样例 |
| `docs/DSH-REFERENCE.md` | 真实 DSH（`@deepseek-ai/dsh@0.2.1-rc.5`）的手测路径，含必需的协议桥与排查表 |

快速自动核对：

```bash
go build -o /tmp/keel-dsh-ref ./cmd/keel-dsh-ref
/tmp/keel-dsh-ref -kind=agent -usage -prompt=ref:      # 直接在 stdio 上讲协议
scripts/external-gateway-smoke.sh                      # 端到端 15 步，0 残留进程
```

## 7. 已知缺口（项目自己的登记表）

摘自 `docs/DSH-REFERENCE.md:228` 的诚实缺口表：

| # | 缺口 | 当前状态 | 补强方向 |
|---|---|---|---|
| 1 | sidecar 的 CPU/内存预算未在 Go 侧实施 | `maxSessions` 只限会话数；CPU/内存/fd/子进程数由部署侧承担（cgroup、ulimit、节点隔离） | 在 gateway 的 spawn/transport 层接入 per-backend 预算（cgroup v2 或 rlimit），超限拒绝激活并进审计 |
| 2 | **runtime 被 SIGKILL 后，遗留 sidecar 没有任何 sweep** | 正常回收走 `StdioTransport.Close`；runtime 被 SIGKILL 时这条路径不执行。`gateway.KillTree` 与 `RedactEnv` 存在但没有生产调用点。唯一防线是协议约定：父进程死亡 ⇒ stdin EOF ⇒ sidecar 自行退出 | pidfile + 下次启动 sweep（需校验 pid 复用），或交给外部 supervisor（systemd `KillMode=control-group`、容器 PID namespace + tini、k8s pod 级回收） |

风险：若 sidecar 忽略 stdin EOF、自行 `setsid` 或让孙进程脱离进程组，可能继续持有模型凭据与网络连接；
而新 runtime 的 `maxSessions` 记账从零开始，会低估在跑会话。SIGKILL 是本运行时的既定输入
（`cmd/chaos`），不是理论场景——部署 sidecar 时请把它当作未关闭的加固项。
