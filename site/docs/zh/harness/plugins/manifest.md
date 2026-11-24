# 清单（`plugin.yaml`）

> **已实现。** 下文的包格式、校验器与存储均在本仓库交付；每条规则都附执行它的代码。
> 契约文本住在 `openspec/specs/plugin-system/spec.md`（「插件包与清单」「发现、安装与包存储」），
> 随 `keel-plugin-core` change 归档时晋升为活规格。只有一个字段是纯声明：
> `permissions.fs`（见[权限](/zh/harness/plugins/permissions)）。

## 1. 包

插件**是一个目录**：

```
<plugins-root>/<id>/
├─ plugin.yaml            清单（必需）
├─ ui/                    webview 资源（贡献 iframe 的 webview 必需）
│   ├─ index.html
│   └─ assets/…
├─ bin/                   sidecar 入口（kind: sidecar 必需）——实际路径由清单的
│                         entry.command 指定
└─ extensibility.json     机读扩展面（官方插件必需，第三方可选）
```

包在安装时被拷入**受管目录**（`<plugins-root>/<id>/`）；已存的插件绝不指向来源目录，安装会
记录包内容树的 hash（`internal/pluginpkg/package.go:74`、
`internal/harnessext/plugin_manager_ops.go:504`）。根目录由 `--plugins-root` /
`KEEL_PLUGINS_ROOT` / `$KEEL_HOME/plugins` 决定（`cmd/keel/main.go:262`），首次安装时创建。

## 2. 清单——全字段

```yaml
id: vendor.name                # ^[a-z][a-z0-9._-]{1,63}$；全局唯一；同 id 只能有一个启用版本
version: 1.2.3                 # semver
kind: in-process | sidecar | webview
layer: kernel | harness | provider | ui | service   # 可选；缺省按 kind 推导
description: 一句话
engines: { keel: ">=0.2.0 <0.3.0" }        # 宿主版本门
provides:
  tools: [name, ...]           # 面向模型的工具名；全局唯一（冲突 fail-closed）
  hooks: [event, ...]          # 必须属于闭集 hook 事件
  triggers: [id, ...]
  provider_types: [...]        # 本插件声明的 provider 能力族
  provider: { ... }            # 标记为 provider 插件（要求 layer: provider）
  feature_ports: [port, ...]
  agents: [id, ...]
  agent_teams: [id, ...]
  cli: [{ path: [...], summary, schemas, permissions, danger, idempotency, callers }]
  ui: { entry: ui/index.html, title: "..." }   # 贡献 iframe 时必填
  slots: [{ id, forms, arity, schema?, targets }]   # 本插件声明的槽位
  backends: [{ tool, action, impl }]                # 既有聚合工具 action 映射到本插件实现
contributes:
  - { slot: <slot-id>, form: chunk|package|iframe|data, entry?, payload?, capabilities?, targets?, tool?, part? }
extensibility: { doc: EXTENSIBILITY.md, schema: extensibility.json }  # 官方插件必填；第三方可省
requires: [plugin-or-service-id, ...]          # 依赖缺失 ⇒ 不装载（绝不半装载）
permissions:
  fs: [workspace-write]        # 文件效应类别（声明；尚未中介——见权限页）
  net: [host, ...]             # 允许的 host
  secrets: [NAME, ...]         # 允许的密钥引用
  tools: [name, ...]           # 允许调用的工具
entry: { command: [...], args: [...], env: [...] }   # sidecar 必填；in-process 可为空
source: { type: builtin | local | git | npm, path?, url?, ref?, package? }
```

结构体见 `internal/pluginpkg/manifest.go:142`；`permissions` `:381`、`entry` `:398`、
`source` `:413`；校验 `:653`。未知字段一律拒绝——拼错字段是错误，不是静默 no-op。

逐字段语义与强制点：

| 字段 | 含义 | 在哪强制 |
| --- | --- | --- |
| `id` | 全局身份、目录名、审计主体 | 安装校验 |
| `version` | semver；同 id 的第二个启用版本被拒 | 安装/升级 |
| `kind` | 三种之一；决定 `entry`/`ui` 的合法形状 | 安装校验、[三种 kind](/zh/harness/plugins/kinds) |
| `layer` | 架构角色，与 `kind` 正交；缺省按 kind 推导（`webview`→`ui`、`sidecar`→`provider`、`in-process`→`harness`） | 安装校验、发布到插件清单 |
| `engines.keel` | 支持的宿主 semver 区间；不匹配则安装**与**启用都被拒并明示区间 | 版本门 |
| `provides.tools` | 贡献的工具名；跨启用插件重名即 fail-closed；发布名归一为 `^[A-Za-z0-9_-]{1,64}$` | 安装校验、工具派发、装配 |
| `provides.hooks` | 对**闭集**事件的订阅（今天为 `subagent.*`、`trigger.*`、`turn.*`、`decision.*`；`internal/harnessext/hooks.go:18`） | 订阅注册 |
| `provides.triggers` | 插件拥有的触发器 id（`interval`/`cron`/`webhook`） | 触发器引擎 |
| `provides.provider_types` | provider 能力族：显示名、capabilities 与 settings UI 渲染的 config/auth/model/parameter schema | 设置面、provider registry |
| `provides.provider` | 标记 provider 插件；要求 `layer: provider` | 安装校验 |
| `provides.feature_ports` | 本插件自身服务的 feature port | 安装校验、清单 |
| `provides.agents` / `agent_teams` | 本插件贡献的 agent 与 agent-team id | 装配、清单 |
| `provides.cli` | 插件命令树：summary、schema、权限、danger、幂等、callers——`--help`、设置页与创造模式共用的单一真源 | 安装校验；**尚未**作为 `keel <cmd>` 面执行 |
| `provides.ui` | webview 入口文档 + 标题；贡献 iframe 的 webview 必填，纯 chunk 包可省 | UI 托管 |
| `provides.slots` | 本插件声明的槽位（供他人贡献的扩展点）；槽位 id 以声明者插件 id 为命名空间 | 扩展校验、清单 |
| `provides.backends` | 把既有聚合工具 action 映射到本插件实现；backend 不能新增 action 名 | 安装校验、工具派发 |
| `contributes` | 本插件的贡献：槽位 id、形态（`chunk`/`package`/`iframe`/`data`）、entry/payload 与 target 集 | 扩展校验、客户端槽位 registry |
| `extensibility` | `EXTENSIBILITY.md` + `extensibility.json`；官方插件必填，第三方可省（缺失 = 不可被创造模式利用） | 包校验（`internal/pluginpkg/extensibility.go:127`） |
| `requires` | 必须存在的插件或服务 id；缺失 ⇒ 不装载，绝不半装载 | 生命周期装配 |
| `permissions` | 能力预算；见[权限](/zh/harness/plugins/permissions) | 工具派发与 sidecar 环境已强制；`net`/`secrets` 有 gate 但尚无宿主动词路由；`fs` 仅声明；每次拒绝写审计 |
| `entry` | sidecar 进程的 command/args/env（env 只取白名单名字，值永远来自宿主） | 进程监督 |
| `source` | 包来源（`builtin`、`local`、`git`、`npm` + path/url/ref/package） | 安装 + 审计 |

不属于单字段、但同属一份契约的规则：

- **清单非法 → 带字段级原因拒绝**；插件目录不被采用，行也不落库
  （`internal/pluginpkg/manifest.go:653`）；
- **版本门**——`engines.keel` 不匹配时安装与启用都被拒，并明示支持区间
  （`internal/harnessext/plugin_manager_ops.go:577`、`:687`）；
- **内容 hash**——安装校验清单、版本门与包内容树，然后拷贝包并记录内容树的 SHA-256
  （`internal/pluginpkg/package.go:74`、`internal/harnessext/plugin_manager_ops.go:504`）；
- **安装即出现**——安装成功后 `GET /v1/plugins` 立即列出该插件（`installed` 或 `enabled`），
  清单 revision 递增。

## 3. 发现、安装与存储

| 操作 | 面 | 语义 |
| --- | --- | --- |
| 发现 | 启动时扫描 `plugins-root`（`internal/pluginpkg.Discover`） | 磁盘上的包出现在清单中；包缺失或非法的行保留状态，让原因可见 |
| 安装 | `POST /v1/plugins/install` `{path}` | 校验清单 + 版本门 + 内容 hash；拷入受管根；写审计行 |
| 列表 | `GET /v1/plugins` | root、revision、generation、逐插件视图、工具、prompt 分节、hooks、触发器、limits |
| 卸载 | `POST /v1/plugins/{id}/uninstall` | 必须先停用；移除受管目录（含归档版本）并写审计；内置插件只能停用，不能卸载 |
| 升级 | `POST /v1/plugins/{id}/update` `{path?}` | 同 id 新版本；空 path 取根目录下另一个最高版本；旧包归档到 `<root>/.versions/<id>/`；启用失败则回滚旧版本 |
| 审计 | `GET /v1/plugins/audit` | 生命周期轨迹，按 `after` 做 keyset 分页 |

安装 API 是**两阶段**的，因为采用第三方代码是一个决定而不是默认值
（`internal/harnessapi/plugin_approval.go:412`）：

```
POST /v1/plugins/install {"path": "..."}                      → 202 {"approvalRequired": true, "decision": …, "answerUrl"}
POST /v1/plugin-approvals/{decisionId} {"optionId": "allow"}   （人作答）
POST /v1/plugins/install {"path": "...", "decisionId": "..."}  → 201（或被拒 409）
```

状态码，精确地说：

| 码 | 在这里的含义 |
| --- | --- |
| `202` | 安装/启用/升级打开了 durable 审批问题；尚未采用任何东西 |
| `201` | 安装已执行：包在受管根目录中，行已落库 |
| `200` | 启用/停用/卸载/升级完成 |
| `409` | 状态机拒绝（已启用、未启用、有启用依赖方、版本门、该 kind 无 loader）或审批被拒/超时 |
| `400` | 请求或包不可用（路径错误、清单非法），附字段级细节 |
| `404` | 没有这个已安装 id |
| `503` | 未装配插件管理器，或生命周期写没有可发问的 decision 原语（fail closed） |

decision id 由 `(tenant, action, requestId-or-target)` 派生，因此为某个动作、某个包打开的问题
无法授权另一个；被拒或超时的答案永不抵达管理器，并以 `refused` 结果记入审计。
