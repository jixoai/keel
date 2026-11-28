# 插件

> **本节统一使用的状态标注**
>
> - **已实现**——本仓库当下已交付；每条断言附可打开的 `path:line` 出处。
> - **部分实现**——机制已交付，但某一部分缺失或比契约更窄；缺口在出现处写明。
> - **Deferred（推迟）**——已定规格并登记、不在本构建中。活契约是
>   `openspec/specs/plugin-system/spec.md`（引入它的 `keel-plugin-core` change 已归档于
>   `openspec/changes/archive/2026-11-11-keel-plugin-core/`）；覆盖矩阵是
>   `plugins/COVERAGE.md`，机读真源为 `plugins/keel.plugin-market/catalog.json`
>   （`scripts/plugin-catalog.mjs:13`）。
>
> 插件包标准、生命周期 API、工具与进程边界上的权限强制、webview 宿主与创造模式插件均已交付。
> 仍然比契约更窄的部分集中写在 §2.3，而不是靠一条「规划中」横幅暗示。

## 1. 命题：账本之上，一切皆装配

keel 与 DeepSeek Harness 共享的设计宣言是「一切皆插件」——keel 的修正在于：**契约内核不是**。
账本、事件顺序、幂等键与 fencing 是让其他一切可替换的不变量；它们的实现是服务图（通用装配
关注点）加插件宿主（产品扩展），前面再加一层插件包。

Go 里用三个机制表达同一套语义：

| 机制 | 装配什么 | 出处 |
| --- | --- | --- |
| **服务图**（`internal/servicegraph` + `internal/loadout`） | 一方运行时服务：store、llm、prompt、tools、loop、compaction、projection、telemetry、外部网关 | `internal/loadout/loadout.go:63` |
| **插件运行时**（`internal/harnessext`） | 产品扩展：工具、hooks、触发器、prompt 分节及其清理 | `internal/harnessext/plugin.go:163` |
| **插件包**（`internal/pluginpkg` + `internal/harnessext` 管理器） | 已安装的 `plugin.yaml` 包：清单校验、受管根目录、生命周期、权限、审计 | `internal/pluginpkg/manifest.go:142`、`internal/harnessext/plugin_manager_ops.go:504` |

三者的词汇相同——`provides`/`requires`/`optional`、有序激活、对每个注册副作用做**反序撤销**、
generation 计数、可观测诊断。装配身份未变的服务跨代共享（`docs/LOADOUT.md` §4）；插件则在
每次 `Apply` 时重新装配（`internal/harnessext/plugin.go:209`）。

它与 DSH 插件模型的对应是刻意的，但属于语义对应而非机械移植——见 §4。

## 2. 今天交付了什么

### 2.1 进程内契约

`harnessext` 插件契约是一个 Go 值：

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

（`internal/harnessext/plugin.go:67`；`Start` `:78`、`Stop` `:80`、`Effect` `:51`、
`PluginEnv` `:57`。）

宿主提供的保证，每条都在 `internal/harnessext/harnessext_test.go` / `e2e_test.go` 有测试：

- **依赖拓扑**——按依赖顺序激活；依赖缺失的插件保持 `PENDING` 并给出 `waiting for: …`
  诊断，绝不半装载（`internal/harnessext/plugin.go:291`）；
- **可逆注册**——hook 订阅、触发器注册与声明的 effects 按精确反序释放；一次卸载不可能留下
  监听器、定时器或工具（`internal/harnessext/plugin.go:386`、`:417`）；
- **fail-closed 工具命名空间**——两个插件声明同名工具时，第二个插件进入 `FAILED`；模型永远
  看不到有歧义的名字（`internal/harnessext/plugin.go:236`）；
- **generation**——每次 `Apply` 令 generation 恰好 +1；快照可查（`internal/harnessext/plugin.go:213`、
  `:505`）；
- **工具发布**——每次装配后，激活的工具集经唯一 sink 推入宿主 registry（`keel serve` 中为
  `rt.RefreshProductTools`，`cmd/keel/serve.go:464`）。

### 2.2 插件包层

包契约端到端已实现：

- **清单**——`plugin.yaml` 严格解析（未知字段即错误）并按字段级错误校验：
  `id/version/kind/layer/description/engines/provides/contributes/extensibility/requires/
  permissions/entry/source`（`internal/pluginpkg/manifest.go:142`，校验在 `:653`）。包加载器
  校验包内容树（UI 入口、贡献入口、sidecar 命令）并做内容哈希
  （`internal/pluginpkg/package.go:74`）。
- **受管根目录**——包位于 `<plugins-root>/<id>/`，安装时拷入并做内容哈希；启动时发现。根目录
  默认 `$KEEL_HOME/plugins`，可用 `--plugins-root` / `KEEL_PLUGINS_ROOT` 覆盖
  （`cmd/keel/main.go:175`、`:246`）。
- **生命周期**——六个状态（`installed`、`enabled`、`disabled`、`failed`、`invalid`、
  `uninstalled`；`internal/harnessext/plugin_manager.go:29`）上的 `install`、`enable`、
  `disable`、`uninstall`、`update`。API 为 `GET /v1/plugins`、
  `POST /v1/plugins/install`、`POST /v1/plugins/{id}/enable|disable|uninstall|update`、
  `GET /v1/plugins/audit`（`internal/harnessapi/plugins.go:56`）。安装、启用、升级由 durable
  审批把门（`internal/harnessapi/plugin_approval.go:412`）。
- **权限**——`permissions.tools` 在工具派发处校验，sidecar 环境按声明的名字构造；每次拒绝写一条
  `plugin_audit` 行（`internal/harnessext/plugin_permissions.go:19`）。`permissions.net` 与
  `permissions.secrets` 有策略、gate 与审计路径，但尚无宿主动词路由到它们；`permissions.fs`
  只声明、已发布、尚未中介——见[权限](/zh/harness/plugins/permissions)。
- **三种 kind**——编译进宿主的 `in-process`；经网关 stdio JSON-RPC 的 `sidecar` 进程
  （`internal/harnessext/plugin_sidecar.go:22`）；其余 kind 由 loaders 映射解析，`keel serve` 中
  该映射只含 sidecar loader（`cmd/keel/serve.go:1769`）。`webview` 包托管于
  `/plugins/<id>/`，经 `keel.plugin.*` 桥接（`cmd/keel/plugins_ui.go:17`、
  `internal/harnessapi/plugin_bridge.go:75`）。
- **槽位与贡献**——`provides.slots` 与 `contributes[]` 被解析、校验并发布到插件清单
  （`internal/harnessext/plugin_extension.go:65`、`internal/harnessext/plugin_manager_view.go:198`），
  客户端经统一槽位 registry 渲染 `chunk`、`iframe` 与 `data` 贡献；签名 `package` 形态今天
  在客户端是显式的不支持面（`web/src/plugins/slots.tsx:1`、`:590`）。
- **创造模式**——`keel.creation` 是内置插件，提供六个动作（`catalog`、`scaffold`、`validate`、
  `install`、`list`、`remove`；`internal/harnessext/creation.go:91`），与除 `keel.testkit` 外的
  所有内置插件一样默认启用（`cmd/keel/serve.go:1752`）。

开箱即用，`keel serve` 装配内置集合——`keel.example`（`internal/harnessext/builtin.go:38`）、
`keel.file`、`keel.model`、`keel.bot`、`keel.team-bot`、`keel.spill`、`keel.tasks`、
`keel.shell`、`keel.approval`、`keel.notify`、`keel.creation`——加上默认关闭的
`keel.testkit`（`internal/pluginpkg/builtin/`、`cmd/keel/serve.go:1660`）。`plugins/` 下的官方
可安装包另有 `keel.plugin-market`、`keel.web-search` 与 `keel.context-memory`；其状态以
`plugins/COVERAGE.md` 为准。子代理原语是 harness 内建的 `agent` 工具，不是插件工具
（`internal/toolsurface`）。设置界面渲染装配结果——generation、逐插件状态、提供的工具、
hooks、触发器、诊断（`web/src/screens/settings.tsx:1148`）。

### 2.3 仍然推迟的部分

| 推迟项 | 原因 | 记录在哪 |
| --- | --- | --- |
| `agent_backend` sidecar（OAR 适配器） | sidecar loader 只接受 `tool_provider` 连接；`plugins/keel.oar/plugin.yaml` 作为 **DEFERRED** 目标清单保留 | `plugins/keel.oar/plugin.yaml:3` |
| 创造模式端到端（G8） | 动作后端、模板与 webview E2E 均已存在；「一句话→运行中的插件」端到端门未通过 | `openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md:20` |
| 创造模式 v2 扩展面聚合、原生 chunk 主路径 | 声明格式已冻结并校验；把每个已安装插件的 `extensibility.json` 聚合为创造面尚未完成 | `plugins/COVERAGE.md`（状态 `partial`） |
| `net` 与 `secrets` gate 的受中介 fetch/密钥读取动词 | 策略、gate 与审计路径已存在（`PermissionedBroker.Fetch` / `ReadSecret`），但没有已交付的宿主动词路由到它们，因此它们尚未拒绝过任何请求 | `internal/harnessext/plugin_permissions.go:648` |
| per-plugin CPU/内存预算、宿主 `SIGKILL` 后的 pidfile sweep | 进程边界与进程组回收已有；资源预算与遗留进程清扫没有 | [权限](/zh/harness/plugins/permissions) §5 |
| 客户端安装/更新卡 | 后端安装 API 与生命周期端点已交付；web 插件页在卡片位置仍渲染 Empty | `web/src/screens/plugins.tsx:1` |

## 3. 标准一览

插件**是一个包**：目录 `<plugins-root>/<id>/` 加 `plugin.yaml` 清单、三种 kind 之一、生命周期
状态机、被强制执行的权限集与审计轨迹（`openspec/specs/plugin-system/spec.md`）。

| 主题 | 状态 | 交付了什么 | 页面 |
| --- | --- | --- | --- |
| 包与清单 | **已实现** | `plugin.yaml`：id/version/kind/layer/engines/provides/contributes/extensibility/requires/permissions/entry/source；清单非法 → 安装被拒并给字段级原因 | [清单](/zh/harness/plugins/manifest) |
| 三种 kind | `in-process`（内置）与 `sidecar`（tool provider）**已实现**；`agent_backend` sidecar **Deferred** | `sidecar` = 外部进程、stdio 换行 JSON-RPC；`webview` = 宿主托管的 UI 包（可带 sidecar 后端） | [三种 kind](/zh/harness/plugins/kinds) |
| 生命周期 | **已实现** | `installed/enabled/disabled/failed/invalid/uninstalled`；启用走 `harnessext` 的依赖拓扑装配与反序卸载；停用零残留 | [生命周期](/zh/harness/plugins/lifecycle) |
| 权限 | **部分实现**：工具与 sidecar 环境已强制；`net`/`secrets` 有 gate 但无宿主动词路由；`fs` 仅声明 | 工具白名单在派发处校验；sidecar 环境变量白名单；安装/启用/升级需 durable 审批；每次拒绝写审计 | [权限](/zh/harness/plugins/permissions) |
| UI | **已实现** | webview 资源托管于 `/plugins/<id>/`，经 `keel.plugin.*` 桥接，主题取自宿主令牌；TUI 绝不渲染插件 UI（改为打开 URL） | [插件 UI](/zh/harness/plugins/ui) |
| 槽位与贡献 | **已实现**（声明 → 清单 → 客户端槽位 registry）；`package` 形态与创造模式聚合 **Deferred** | `provides.slots[]`、`contributes[]`、`extensibility.json`；chunk/iframe/data 今天可渲染，package 是显式不支持面 | [槽位](/zh/harness/plugins/slots) |
| 测试 | 要求、覆盖矩阵与 catalog 门**已实现** | 单测 + 集成 + conformance + 负控；`plugins/COVERAGE.md`；`scripts/plugin-catalog.mjs --check` | [测试](/zh/harness/plugins/testing) |
| 创造模式 | 动作与模板**已实现**；端到端（G8）**Deferred** | `keel.creation` 本身是一个插件：NL → 生成 → 校验 → 安装 → 启用 → 深链，带审批与回滚 | [创造模式](/zh/harness/creation-mode) |

## 4. DSH 对照——语义映射，不是移植

DeepSeek Harness 是**参考**，不是 keel 的事实源。其插件模型（按本次对照所载）映射到 keel
语义如下：

| DSH 插件模型 | keel 对应 | 状态 |
| --- | --- | --- |
| 插件 = `apply(ctx)` + 依赖注入 + 可逆副作用 | `Plugin.Start(ctx, *PluginEnv)` + `Provides`/`Requires` + 按反序释放的 `Effects`（`internal/harnessext/plugin.go:57`、`:78`、`:51`、`:387`） | **已实现**（进程内） |
| 能力三角：Definition / Provider / Consumer | 服务图：描述符 `provides` 一个 capability，依赖方声明 `requires`/`optional`，绑定被断言为窄接口（`internal/servicegraph/descriptor.go`、`docs/LOADOUT.md` §7） | **已实现** |
| bundle = 一层补丁（配置/插件） | assembly：四个有序层（base → mode → preset → `config.toml`）加 row id 覆盖（`docs/ASSEMBLY.md` §2）；*插件 bundle* 仍是覆盖矩阵里的一行，不是已交付产物 | **已实现**（装配），**Deferred**（插件 bundle） |
| profile = 有序的 bundle 栈 | loadout：不可变快照 `{id, revision, version, summary, services}`，按 run 钉住（`docs/LOADOUT.md` §1）；assembly 把配置层线性化 | **已实现** |
| 插件可提供 tool / hook / command / UI slot / provider / storage | 工具、hooks、触发器、prompt 分节、槽位/贡献与 provider 类型均已声明并校验（`internal/pluginpkg/manifest.go:210`）；`provides.cli` 能力树已声明并校验，但尚未作为 `keel <cmd>` 面执行；storage 域是注册行，不是插件 | **部分实现** |
| 安装第三方代码即执行；需显式许可 | 已交付：安装/启用/升级是 durable 审批，权限在宿主接缝强制，拒绝写审计（`internal/harnessext/plugin_permissions.go:19`） | **已实现** |
| Cordis 运行时（进程内对象图、同步 `get/provide`、effect 系统、事件总线） | **不移植。** keel 用 Go 的 `harnessext` + `servicegraph` 表达同一语义；引入第二套插件运行时会违背单一真值（`plugins/COVERAGE.md` §D，`vendor/cordis` = absent 并附理由） | **刻意差异** |

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
3. [生命周期](/zh/harness/plugins/lifecycle)——状态、迁移、安装 API、可逆卸载、热更、审计。
4. [权限](/zh/harness/plugins/permissions)——白名单、审批、审计，以及沙箱边界的真实位置。
5. [插件 UI](/zh/harness/plugins/ui)——webview + react + solid-glass、`keel.plugin.*` 桥、
   主题令牌、TUI 行为。
6. [槽位](/zh/harness/plugins/slots)——槽位、贡献与客户端槽位 registry。
7. [测试](/zh/harness/plugins/testing)——单测、集成、conformance 与负控。
