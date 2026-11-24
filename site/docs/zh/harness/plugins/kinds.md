# 三种 kind

> **已实现**，只留一个有名字的缺口。三种 kind 都由本构建承载：`in-process`（内置清单）、
> `sidecar`（外部进程、走网关的 stdio 协议）与 `webview`（UI 包托管于 `/plugins/<id>/`）。
> **Deferred** 的是 `agent_backend` sidecar：loader 只接受 `tool_provider` 连接，因此 OAR
> 适配器以目标清单形式存在，不是可装载的包（`plugins/keel.oar/plugin.yaml:3`）。

插件恰为三种之一。kind 决定 `entry` 的含义、代码在哪运行、存在什么隔离、宿主必须处理哪些
失败模式。

| kind | 运行位置 | `entry` | 隔离 | 典型用途 |
| --- | --- | --- | --- | --- |
| `in-process` | 编译进宿主 | 空（只控制配置） | 无（受信） | 随二进制发布的官方能力 |
| `sidecar` | 外部进程 | `command/args/env` | 进程边界 | 任何不能拖垮宿主的东西 |
| `webview` | 宿主 WebView 中的 UI | `ui/` 包（可带 sidecar 后端） | UI 沙箱 + 桥 | 仪表盘、面板、创造模式产物 |

本构建能*装载*哪些 kind 是第二个问题，由 loaders 映射回答。`keel serve` 只注册一个
loader——sidecar loader（`cmd/keel/serve.go:1769`），而 `loaderForKind` 还会把带
`entry.command` 的 `webview` 包也路由给它（`internal/harnessext/plugin_manager_ops.go:409`）。
不带后端的 `webview` 包没有任何运行时面：启用它只是为受托管 UI 开门，装配为空
（`internal/harnessext/plugin_manager_ops.go:447`）。从磁盘装进来的 `in-process` 包没有
loader——这种 kind 的代码必须编译进二进制，启用这类包会以「kind has no loader in this
build」返回 `409`（`internal/harnessext/plugin_manager.go:64`）。

## 1. `in-process`

随二进制发布的官方插件。清单只控制启用与配置，代码已编译在内。不存在沙箱，也不做此声称：
进程内插件按构造即拥有宿主全部权限。这正是当前 `harnessext` 运行时已为内置插件实现的 kind
（`internal/harnessext/builtin.go:38`、`:256`）。

内置集合是嵌在二进制里的十二份清单（`internal/pluginpkg/builtin/builtin.go:26`）：
`keel.file`、`keel.model`、`keel.bot`、`keel.team-bot`、`keel.spill`、`keel.tasks`、
`keel.shell`、`keel.approval`、`keel.notify`、`keel.creation`、`keel.example` 与
`keel.testkit`。除 `keel.testkit`（E2E grant 过滤夹具）外全部默认启用
（`cmd/keel/serve.go:1752`）。内置插件可经 API 停用，但不能卸载、也不能从包升级：它们的行
标记 `bundled: true`，动作被拒（`internal/harnessext/plugin_manager_ops.go:824`）。

## 2. `sidecar`

一个外部进程，说 **stdio、换行分隔 JSON-RPC 2.0**，复用网关既有契约——loader 不重造线上的
任何东西（`internal/harnessext/plugin_sidecar.go:22`）：

| 动词 | 在线上如何出现 | 语义 |
| --- | --- | --- |
| `initialize` | 握手（`gateway.Conn.Connect`） | 协商协议版本与 provider 类型；`sidecar` 必须回答 `provider{type: tool_provider}` 与 `capabilities.tools`，其他一切在任何工具执行前即被拒 |
| `tools/list` | `tools/list`（分页） | 插件提供的工具 |
| `tools/call` | `tools/call` | 执行一次调用，带请求超时、调用方取消（传递为 `notifications/cancelled`），连接断开即明确报错 |
| `health` | **不是该面上的线方法** | 仅监督层：loader 自己的健康报告（连接状态 + 进程存活 + 记录的失败）成为清单里该插件的状态 |
| `close` | 先尽力发通知，再走关闭阶梯 | 关 stdin → 向**进程组**发 `SIGTERM` → 向进程组发 `SIGKILL` |

`agent_backend` provider 类型（`session.open` / `message.send` / `event.stream`）刻意不被
接受：`tool_provider` 连接按构造拒绝 agent 面的方法，为插件另造第二套协议已被否决。这就是
`keel.oar` 以 **DEFERRED** 目标清单存在、而不是可安装包的原因。

插件从同一实现继承的能力（均为**已实现**，位于 `internal/gateway`）：

- **环境变量白名单**——被 spawn 的 provider 只拿到 `MinimalEnv` 加显式声明的变量名；宿主密钥
  默认不可见（`internal/gateway/process.go:106`）；
- **干净关闭**——关闭路径为：关 stdin → 宽限期 → 向**进程组**发 SIGTERM → SIGKILL，且幂等
  （`internal/gateway/stdio.go:228`、`:379`）；
- **进程组回收**——`KillTree` 存在，冒烟套件断言孙进程 0 残留
  （`internal/gateway/process_unix.go:58`、`scripts/external-gateway-smoke.sh`）；
- **逐调用错误面**——被取消的调用、超时与断连都是明确错误，绝不静默挂起。

隔离承诺，用大白话复述：

> 当某个 sidecar 插件进程被杀，其工具调用返回明确错误、插件状态变为 `failed`；其他插件与
> Harness 不受影响；重新启用即可重连恢复。

外部网关复核留下的两个诚实限制，必须对插件原样重申（`docs/DSH-REFERENCE.md` §4）：

1. **尚无资源预算。** 会话数有上限（`maxSessions`），但 CPU、内存与文件描述符是部署侧关注
   （cgroup/ulimit/节点隔离）——per-backend 预算未实现。
2. **宿主被 `SIGKILL` 后没有 sweep。** 正常回收只走关闭路径。宿主被杀时，契约是协议层的——
   父进程死亡关闭 stdin，子进程必须因 EOF 退出（DSH 实测：EOF → exit 0，
   `docs/DSH-REFERENCE.md` §3.2）。pidfile sweep 或外部 supervisor 未实现。

## 3. `webview`

`ui/` 下的 UI 包，由宿主托管并在 WebView 中渲染：

```
<plugins-root>/<id>/ui/
├─ index.html          入口文档（清单 provides.ui.entry）
└─ assets/**           JS/CSS/图片——从 /plugins/<id>/ 提供
```

- 宿主把该包托管在 **`/plugins/<id>/`**，且只对**已启用**插件开放；清单本身永不提供，
  `..` 穿越被拒（`cmd/keel/plugins_ui.go:17`）。
- UI **只能**通过桥接 `keel.plugin.*` 与宿主对话（读自身状态、调用自身工具、请求审批、订阅
  事件），且永不获得宿主密钥。细节见[插件 UI](/zh/harness/plugins/ui)。
- webview 插件**可以带 sidecar 后端**（`entry.command`）；UI 经由自己的工具抵达自己的进程，
  绝不直接 IPC。纯 UI 的 webview 会被启用但从不参与装配。
- webview 包也可以贡献 **chunk** 而非 iframe；纯 chunk 包可完全省略 `provides.ui`，经客户端
  槽位 registry 在宿主树内原生渲染（[槽位](/zh/harness/plugins/slots)）。
- **TUI 行为**：TUI 不得渲染插件 UI。插件行提供「打开」，对 web 深链调用 `openUrl`；平台拒绝
  这次交接时，行内改为保留可复制的 URL（`web/src/screens/plugins.tsx:13`、`:220`）。

## 4. 如何选择 kind

| 你需要… | 选择 | 理由 |
| --- | --- | --- |
| 仪表盘或面板 | `webview` | UI 就是交付物；无需监督进程 |
| 一个要对接外部系统、自带运行时/工具链的工具 | `sidecar` | 崩溃隔离 + 协议边界 |
| 属于产品本身的能力 | `in-process` | 零 IPC 开销；官方代码受信 |
| 非 Go 语言实现、且带 UI 的一切 | `webview` + sidecar | UI 与逻辑都留在宿主进程之外 |

决定记录在 `plugin.yaml`（`kind`）中，且不可在运行时切换：换 kind 意味着发布另一个包。
而且 kind 只有在本次构建有对应 loader 时才可用——`in-process` 意味着「编译进来」，不是
「丢进插件根目录」。
