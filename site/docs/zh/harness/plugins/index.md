# 插件

> **本节统一使用的状态标注**
>
> - **已实现**——本仓库当下已交付；每条断言附可打开的 `path:line` 出处。
> - **规划中**——开发中的 `keel-plugin-core` change 所定规格
>   （`openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`、覆盖矩阵
>   `plugins/COVERAGE.md`）。这两个文件在当前检出中都不存在，随该 change 一同到达。
>
> 下面的插件*标准*是规划中的那一份。今天存在的是一个编译进二进制的进程内插件宿主——
> 先如实描述它。

## 1. 命题：账本之上，一切皆装配

keel 与 DeepSeek Harness 共享的设计宣言是「一切皆插件」——keel 的修正在于：**契约内核不是**。
账本、事件顺序、幂等键与 fencing 是让其他一切可替换的不变量；它们的实现是服务图（通用装配
关注点）加一个小的进程内插件运行时（产品扩展）。

Go 里用两个机制表达同一套语义：

| 机制 | 装配什么 | 出处 |
| --- | --- | --- |
| **服务图**（`internal/servicegraph` + `internal/loadout`） | 一方运行时服务：store、llm、prompt、tools、loop、compaction、projection、telemetry、外部网关 | `internal/loadout/loadout.go:63` |
| **插件运行时**（`internal/agentext`） | 产品扩展：工具、hooks、触发器及其清理 | `internal/agentext/plugin.go:186` |

两者的词汇相同——`provides`/`requires`/`optional`、有序激活、对每个注册副作用做**反序撤销**、
generation 计数、可观测诊断。装配身份未变的服务跨代共享（`docs/LOADOUT.md` §4）；插件则在
每次 `Apply` 时重新装配（`internal/agentext/plugin.go:209`）。

它与 DSH 插件模型的对应是刻意的，但属于语义对应而非机械移植——见 §4。

## 2. 今天已实现的部分

`agentext` 插件契约是一个 Go 值：

```go
type Plugin struct {
    ID, Version string
    Provides    []string      // 本插件对外提供的 capability
    Requires    []string      // 依赖的 capability（缺失则保持 PENDING）
    Tools       []ToolSpec    // 面向模型的工具
    Hooks       []HookSpec    // 对闭集事件的订阅
    Triggers    []TriggerSpec // interval / cron / webhook
    Effects     []Effect      // 具名清理，按反序执行
    Start       func(ctx context.Context, env *PluginEnv) error
    Stop        func(ctx context.Context) error
}
```

（`internal/agentext/plugin.go:67`；`Start` `:78`、`Stop` `:80`、`Effect` `:51`、
`PluginEnv` `:57`。）

宿主提供的保证，每条都在 `internal/agentext/agentext_test.go` / `e2e_test.go` 有测试：

- **依赖拓扑**——按依赖顺序激活；依赖缺失的插件保持 `PENDING` 并给出 `waiting for: …`
  诊断，绝不半装载（`internal/agentext/plugin.go:291`）；
- **可逆注册**——hook 订阅、触发器注册与声明的 effects 按精确反序释放；一次卸载不可能留下
  监听器、定时器或工具（`internal/agentext/plugin.go:387`、`:417`）；
- **fail-closed 工具命名空间**——两个插件声明同名工具时，第二个插件进入 `FAILED`；模型永远
  看不到有歧义的名字（`internal/agentext/plugin.go:236`）；
- **generation**——每次 `Apply` 令 generation 恰好 +1；快照可查（`internal/agentext/plugin.go:213`、
  `:505`）；
- **工具发布**——每次装配后，激活的工具集经唯一 sink 推入宿主 registry（`keel serve` 中为
  `rt.RefreshProductTools`，`cmd/keel/serve.go:464`）。

开箱即用，`keel serve` 装配两个内置插件——`keel.example`（完整示例：两个工具、三条 hook
订阅、两个触发器、两个 effect；`internal/agentext/builtin.go:38`）与 `keel.subagents`（暴露
`spawn_subagent` 工具；`internal/agentext/builtin.go:256`）。设置界面渲染装配结果——
generation、逐插件状态、提供的工具、hooks、触发器、诊断
（`web/src/screens/settings.tsx:1148`）。

**它不是**：没有清单文件、没有安装步骤、没有权限模型、没有 UI 托管。今天的「插件」就是
编译进二进制的代码，构造上即受信。下面的一切，是把这套运行时变成包体系的规划标准。

## 3. 规划标准一览

`keel-plugin-core` change 规定插件是一个**包**：目录 `<plugins-root>/<id>/` 加
`plugin.yaml` 清单、三种 kind 之一、生命周期状态机、被强制执行的权限集与审计轨迹
（`openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`）。规格的关键决定：

| 主题 | 决定（规划） | 页面 |
| --- | --- | --- |
| 包与清单 | `plugin.yaml`：id/version/kind/engines/provides/requires/permissions/entry/source；清单非法 → 安装被拒并给字段级原因 | [清单](/zh/harness/plugins/manifest) |
| 三种 kind | `in-process`（官方、编译进）、`sidecar`（外部进程、stdio 换行 JSON-RPC）、`webview`（宿主托管的 UI 包） | [三种 kind](/zh/harness/plugins/kinds) |
| 生命周期 | `installed → enabled → disabled → uninstalled`；启用走 `agentext` 的依赖拓扑装配与反序卸载；停用必须零残留 | [生命周期](/zh/harness/plugins/lifecycle) |
| 权限 | 工具白名单在派发处校验；`net` host 白名单；`secrets` 引用白名单；sidecar 环境变量白名单；安装/启用/升级需 durable 审批；越权写审计 | [权限](/zh/harness/plugins/permissions) |
| UI | webview 资源托管于 `/plugins/<id>/`，经 `keel.plugin.*` 桥接，主题取自宿主令牌；TUI 绝不渲染插件 UI（改为打开 URL） | [插件 UI](/zh/harness/plugins/ui) |
| 测试 | 单测 + 集成 + conformance + 负控（崩溃隔离、越权拒绝、回滚） | [测试](/zh/harness/plugins/testing) |
| 创造模式 | `keel.creation` 本身是一个插件：NL → 生成 → 校验 → 安装 → 启用 → 深链，带审批与回滚 | [创造模式](/zh/harness/creation-mode) |
| 覆盖矩阵 | 官方套件以 DSH 能力组与 bundle 为矩阵，按 W1/W2/W3 波次跟踪 | `plugins/COVERAGE.md`（规划） |

## 4. DSH 对照——语义映射，不是移植

DeepSeek Harness 是**参考**，不是 keel 的事实源。其插件模型（按本次对照所载）映射到 keel
语义如下：

| DSH 插件模型 | keel 对应 | 状态 |
| --- | --- | --- |
| 插件 = `apply(ctx)` + 依赖注入 + 可逆副作用 | `Plugin.Start(ctx, *PluginEnv)` + `Provides`/`Requires` + 按反序释放的 `Effects`（`internal/agentext/plugin.go:57`、`:78`、`:51`、`:387`） | **已实现**（进程内） |
| 能力三角：Definition / Provider / Consumer | 服务图：描述符 `provides` 一个 capability，依赖方声明 `requires`/`optional`，绑定被断言为窄接口（`internal/servicegraph/descriptor.go`、`docs/LOADOUT.md` §7） | **已实现** |
| bundle = 一层补丁（配置/插件） | 规划中的启用清单 + `plugin.yaml`（`keel-plugin-core`）；今天最接近的类比是默认 loadout 蓝图（`internal/loadout/loadout.go:63`） | **规划中** |
| profile = 有序的 bundle 栈 | loadout：不可变快照 `{id, revision, version, summary, services}`，按 run 钉住（`docs/LOADOUT.md` §1） | **已实现**（服务层面），插件入集为规划 |
| 插件可提供 tool / hook / command / UI slot / provider / storage | 工具、hooks、触发器已实现（`internal/agentext/plugin.go:67`）；command、UI slot、provider 与 storage 在 W1/W2 波次（`plugins/COVERAGE.md`） | **部分实现 / 规划中** |
| 安装第三方代码即执行；需显式许可 | 今天：代码编译进宿主，信任是隐含的；规划：权限强制执行 + durable 安装审批（`keel-plugin-core`「权限与沙箱」） | **规划中** |
| Cordis 运行时（进程内对象图、同步 `get/provide`、effect 系统、事件总线） | **不移植。** keel 用 Go 的 `agentext` + `servicegraph` 表达同一语义；引入第二套插件运行时会违背单一真值（`plugins/COVERAGE.md` §D，`vendor/cordis` = absent 并附理由） | **刻意差异** |

阅读插件各页之前，还有两点差异值得知道：

1. **状态所有权。** DSH runtime 可以作为*外部* AgentBackend 接入，由 backend 拥有自己的会话
   状态，keel 只记录兼容日志（`docs/DSH-REFERENCE.md`）。这条路径关乎*外部智能体*而非插件，
   且从不声称原生恢复。
2. **跨进程接缝是显式的。** DSH 的进程内对象图没有无损的网络等价物；keel 的服务图因此把服务
   绑定到窄 Go 接口，凡需跨进程的都以显式协议进行（sidecar JSON-RPC、MCP、AgentBackend），
   绝不做对象图的透明代理（`docs/SERVICE-GRAPH.md` §4）。

## 5. 阅读顺序

1. [清单](/zh/harness/plugins/manifest)——包与 `plugin.yaml` 全字段。
2. [三种 kind](/zh/harness/plugins/kinds)——in-process、sidecar、webview，含 sidecar 协议与
   webview 资源布局。
3. [生命周期](/zh/harness/plugins/lifecycle)——状态机、依赖规则、可逆卸载、热更。
4. [权限](/zh/harness/plugins/permissions)——白名单、审批、审计，以及沙箱边界的真实位置。
5. [插件 UI](/zh/harness/plugins/ui)——webview + react + solid-glass、`keel.plugin.*` 桥、
   主题令牌。
6. [测试](/zh/harness/plugins/testing)——单测、集成、conformance 与负控。
