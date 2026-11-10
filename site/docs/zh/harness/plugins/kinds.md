# 三种 kind

> **规划中。** 三种 kind、各自的 `entry` 形状与隔离承诺来自
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`（「三种 kind」）。
> sidecar 复用的进程监督机制已存在于 `internal/gateway`——那部分是**已实现**，下文给出出处。

插件恰为三种之一。kind 决定 `entry` 的含义、代码在哪运行、存在什么隔离、宿主必须处理哪些
失败模式。

| kind | 运行位置 | `entry` | 隔离 | 典型用途 |
| --- | --- | --- | --- | --- |
| `in-process` | 编译进宿主 | 空（只控制配置） | 无（受信） | 官方能力：`keel.core`、`keel.llm`… |
| `sidecar` | 外部进程 | `command/args/env` | 进程边界 | 任何不能拖垮宿主的东西 |
| `webview` | 宿主 WebView 中的 UI | `ui/` 包（可带 sidecar 后端） | UI 沙箱 + 桥 | 仪表盘、面板、创造模式产物 |

## 1. `in-process`

随二进制发布的官方插件。清单只控制启用与配置，代码已编译在内。不存在沙箱，也不做此声称：
进程内插件按构造即拥有宿主全部权限。这正是当前 `agentext` 运行时已为内置插件实现的 kind
（`internal/agentext/builtin.go:38`、`:256`）。

## 2. `sidecar`

一个外部进程，说 **stdio、换行分隔 JSON-RPC**（`spec: 三种 kind`）。方法集就是网关既有的
那一套——插件规格明确复用：

| 方法 | 方向 | 语义 |
| --- | --- | --- |
| `initialize` | 请求 | 协商协议版本、provider 类型、能力；版本不兼容直接拒绝激活 |
| `tools/list` | 请求 | 插件提供的工具 |
| `tools/call` | 请求 | 执行一次工具调用 |
| `health` | 请求 | `{status: ok|degraded|unavailable}` |
| `close` | 请求 | 带会话时关会话；不带时请求 backend 优雅停机 |

这些常量今天已为外部网关实现：`internal/gateway/protocol.go:86`（`initialize`）、`:100`
（`health`）、`:103`（`close`）、`:106`（`tools/list`）、`:108`（`tools/call`）。

插件从同一实现继承的能力（均为**已实现**，位于 `internal/gateway`）：

- **环境变量白名单**——被 spawn 的 provider 只拿到 `MinimalEnv` 加显式声明的变量名；宿主密钥
  默认不可见（`internal/gateway/process.go:106`）；
- **干净关闭**——关闭路径为：关 stdin → 宽限期 → 向**进程组**发 SIGTERM → SIGKILL，且幂等
  （`internal/gateway/stdio.go:228`、`:379`）；
- **进程组回收**——`KillTree` 存在，冒烟套件断言孙进程 0 残留
  （`internal/gateway/process_unix.go:58`、`scripts/external-gateway-smoke.sh`）；
- **重启策略**——崩溃的 backend 按 `gateway.RestartPolicy` 退避重启
  （`internal/gateway/process.go:26`）。

隔离承诺，用大白话复述（`spec: sidecar 崩溃隔离`）：

> 当某个 sidecar 插件进程被杀，其工具调用返回明确错误、插件状态变为 `failed`；其他插件与
> Harness 不受影响；重新启用即可恢复。

外部网关复核留下的两个诚实限制，必须对插件原样重申（`docs/DSH-REFERENCE.md` §4）：

1. **尚无资源预算。** 会话数有上限（`maxSessions`），但 CPU、内存与文件描述符是部署侧关注
   （cgroup/ulimit/节点隔离）——per-backend 预算是规划中的补强。
2. **宿主被 `SIGKILL` 后没有 sweep。** 正常回收只走关闭路径。宿主被杀时，契约是协议层的——
   父进程死亡关闭 stdin，子进程必须因 EOF 退出（DSH 实测：EOF → exit 0，
   `docs/DSH-REFERENCE.md` §3.2）。pidfile sweep 或外部 supervisor 是规划中的加固。

## 3. `webview`

`ui/` 下的 UI 包，由宿主托管并在 WebView 中渲染（`spec: 三种 kind` 与「UI 承载与桥接」）：

```
<plugins-root>/<id>/ui/
├─ index.html          入口文档（清单 provides.ui.entry）
└─ assets/**           JS/CSS/图片——从 /plugins/<id>/ 提供
```

- 宿主把该包托管在 **`/plugins/<id>/`**；从插件清单里打开插件即加载该路径
  （`spec: UI 承载与桥接`）。
- UI **只能**通过桥接 `keel.plugin.*` 与宿主对话（读自身状态、调用自身工具、请求审批、订阅
  事件），且永不获得宿主密钥（`spec: UI 承载与桥接`）。细节见
  [插件 UI](/zh/harness/plugins/ui)。
- webview 插件**可以带 sidecar 后端**；UI 经由自己的工具抵达自己的进程，绝不直接 IPC。
- **TUI 行为**（`spec: TUI 行为`，规划）：TUI 不得渲染插件 UI。插件行提供「打开」，通过
  `openUrl` 打开浏览器（web 深链）；宿主不支持时，必须改为显示可复制的 URL。

## 4. 如何选择 kind

| 你需要… | 选择 | 理由 |
| --- | --- | --- |
| 仪表盘或面板 | `webview` | UI 就是交付物；无需监督进程 |
| 一个要对接外部系统、自带运行时/工具链的工具 | `sidecar` | 崩溃隔离 + 协议边界 |
| 属于产品本身的能力 | `in-process` | 零 IPC 开销；官方代码受信 |
| 非 Go 语言实现、且带 UI 的一切 | `webview` + sidecar | UI 与逻辑都留在宿主进程之外 |

决定记录在 `plugin.yaml`（`kind`）中，且不可在运行时切换：换 kind 意味着发布另一个包。
