# 插件权限与审计

> **已实现**，但边界要讲清。今天真正在强制的是两项：`permissions.tools`（工具派发点）与
> sidecar 环境白名单（进程构造）。每次拒绝都写审计。`permissions.net` 与 `permissions.secrets`
> 有已编译的策略、gate 与审计路径（`PermissionedBroker.Fetch` / `ReadSecret`），但**本构建没有
> 任何宿主中介的 fetch/密钥读取动词路由到它们**，因此它们今天还没有拒绝过任何东西。
> `permissions.fs` 连 gate 都没有——只声明、已发布（`internal/harnessext/plugin_permissions.go:19`）。
> 本页刻意把这些区分写明，而不是把整个 `permissions` 块都说成已强制。

## 1. 先讲诚实的基线

keel **不对插件代码做沙箱**，并且把这句话说清楚——与本设计所参照的 DSH 模型立场一致：

- **进程内**插件被编译进宿主，以宿主权限运行；`permissions` 块是策略契约，在宿主可控的接缝
  处强制（今天为工具派发与进程派生；宿主中介出站与密钥解析是已建好、等待路由动词的 gate）
  并写入审计——**它不是内存沙箱**；
- **sidecar** 插件运行在自己的进程里；该进程边界加环境变量白名单*就是*隔离；
- 安装插件意味着执行第三方代码。因此**安装/启用/升级都需要显式的 durable 审批**——权限模型
  的全部意义在于：「它能运行」是一个决定，而不是默认值。

## 2. 清单声明什么

```yaml
permissions:
  fs: [workspace-write]        # 文件效应类别（仅声明，见 §3）
  net: [api.example.com, …]    # 允许的 host
  secrets: [OPENAI_API_KEY, …] # 允许的密钥引用（只有名字，绝无值）
  tools: [example_status, …]   # 允许调用的工具
```

强制点及其在本构建中的真实状态：

| 授予 | 在哪强制 | 状态 |
| --- | --- | --- |
| `tools` | **派发点**：代表某插件发起的调用只能命名该插件自己提供的工具或 `permissions.tools` 声明的工具（在 `Manager.InvokePluginTool` 校验，不在注册时）——插件 UI 桥与创造模式都走这里 | **已生效**：调用以明确错误失败 + 审计行 `(插件, 版本, kind=tool, target, reason)` |
| `net` | 策略与 gate 已存在（`PermissionedBroker.Fetch`：精确 host、`*.suffix`、可选 `:port`，仅 `http`/`https`），但没有任何已交付的宿主中介 fetch 动词路由到它 | **gate 就绪、尚未路由**：不经 broker 的 fetch 不受校验 |
| `secrets` | 策略与 gate 已存在（`PermissionedBroker.ReadSecret`：只有声明的名字可解析），但没有已交付的宿主密钥读取动词路由到它 | **gate 就绪、尚未路由**；经 gate 时值永不进日志 |
| `fs` | **连 gate 都没有**——声明被解析、校验并发布，没有任何东西中介文件访问 | **仅声明** |
| sidecar env | 进程构造：宿主安全默认值加恰好声明的**名字**，值永远来自宿主 | **已生效**：白名单外的名字不出现在子进程环境中 |

sidecar 那行由 loader 实现（`internal/harnessext/plugin_sidecar.go:22`），它复用的环境纪律
来自外部网关：被 spawn 的进程只拿到 `MinimalEnv` 加显式声明的变量，引用经 `${NAME}` /
`env:NAME` / `secret:NAME` 解析，且**字面密钥在校验期即被拒绝**
（`internal/gateway/process.go:106`、`internal/servicehost/models.go:546`、
`internal/host/config.go:203`）。同一条纪律今天也在保护模型凭据：租户密钥存储不可用时
`secret:` 引用 fail-closed——绝无明文存取（`cmd/keel/serve.go:165`）。

### 边界，只讲一次

进程内插件跑在宿主进程里；sidecar 是宿主控制的进程。**两者都没有沙箱。** 被强制的是每一个
真正跨过宿主自控路径的capability 请求：工具派发与子进程环境。fetch 与 secret 的 gate 已建好并
带审计，但前面还没有已交付的动词，因此应把它们看作未来受中介能力将采用的形状，而不是当前的
在线过滤器。

由此有两条后果，都是刻意的：

1. sidecar 自己开的 socket 不受 `permissions.net` 约束——声明的 host 列表管的是*宿主*替它
   发起什么；插件作者的规则是「通过 broker 向宿主请求」，而 loader 的环境白名单无论如何都把
   宿主凭据挡在进程之外；
2. 进程内插件可以越出其声明预算行事。官方进程内代码按构造受信；第三方逻辑应放 sidecar。

## 3. 审批：由 decision 原语发问

安装/启用/升级就是 durable、可回答的问题；Harness 已经为这种形状准备了原语
（[审批与问句](/zh/harness/approvals-and-questions)）：

```
POST /v1/plugins/install {"path": ...}                        → 202 {"approvalRequired": true, "decision": …}
POST /v1/plugin-approvals/{decisionId} {"optionId": "allow"}   （人作答）
POST /v1/plugins/install {"path": ..., "decisionId": ...}      → 201（或被拒 409）

                      ├─ allow   ──▶ 校验清单 + 版本门 + hash → installed → 审计
                      ├─ deny    ──▶ 不生效；审计记录拒绝
                      └─ 超时 / 未作答 ──▶ 不生效（fail-closed）
```

规则，每条都在代码里可见：

- decision id 由 `(tenant, action, requestId-or-target)` 派生，因此为某个动作、某个包打开的
  问题无法授权另一个（`internal/harnessapi/plugin_approval.go:412`）；
- 拒绝（deny，或已过截止）永不抵达管理器，并以 `refused` 结果记入审计；
- 审批本身是带答案日志的 durable 行——不是一次性弹窗；超时按 fail-closed 处理。

## 4. 审计

每一次生命周期动作（`install`/`enable`/`disable`/`uninstall`/`update`）与每一次权限拒绝，都
记录**时间、操作者、插件 id、版本、动作、结果**，可通过 `GET /v1/plugins/audit` 查询
（`internal/harnessapi/plugins.go:256`）。拒绝是 `*PermissionDeniedError`
（`errors.Is ErrPermissionDenied`），写一条动作 `deny`、结果 `refused`、detail
`{kind,target,reason}` 的行——与生命周期动作同一张表（`internal/store/schema.sql:603`）。

插件审计沿用代码库既有的风格：有界记录、只含标识、无载荷无密钥。触发器审计保留逐次触发历史
与最近 N 条窗口（`internal/harnessext/triggers.go:86`、`:135`），内核审计 sink 把每次拒绝
投影为结构化日志字段并限频（`internal/servicehost/audit.go`、`docs/LOADOUT.md` §6）。

## 5. 存量风险，如实列出

以下风险已知且刻意不隐藏（`docs/DSH-REFERENCE.md` §4）：

| 风险 | 为何存在 | 缓解路径 |
| --- | --- | --- |
| 进程内插件代码可以越出其声明权限 | 无内存沙箱 | 官方插件按构造受信；第三方逻辑应放 sidecar |
| sidecar 自己的出站网络调用不被过滤 | `permissions.net` 管的是宿主中介出站；子进程是普通进程 | 环境白名单把凭据挡在外面；把 fetch 路由进 `PermissionedBroker.Fetch` 才是校验落点 |
| `permissions.net` / `permissions.secrets` 今天不会真的拒绝 | gate 已存在，但没有已交付的宿主动词路由到它们 | 接线受中介的动词；在此之前声明只表达意图，审计只能看到被路由的部分 |
| 无视 stdin EOF 的 sidecar 能在宿主被杀后存活 | 正常回收只走关闭路径；没有 pidfile sweep | 协议义务（子进程 EOF 即退）+ 规划中的 pidfile sweep / 外部 supervisor |
| 暂无 per-plugin CPU/内存预算 | 预算今天属部署侧 | 规划在 spawn 处接 per-backend 预算（cgroup/rlimit），越限拒绝激活 |
| `permissions.fs` 仅声明 | keel 目前不为插件中介文件访问 | 未来受中介的文件 API 才是校验落点；在此之前该声明只表达意图 |
| 密钥引用是名字而非作用域 | 授予名字即授予值 | 只声明最窄的名字；值永不进日志或账本 |

## 6. 复核者应能核对的清单

对任一启用插件，仅凭 API 面就应能回答：

1. 它声明了什么（清单）——[清单](/zh/harness/plugins/manifest)；
2. 它被允许触碰什么（权限）——本页；
3. 它实际做了什么（审计）——§4；
4. 谁在何时批准了它（decision 行）——§3；
5. 它此刻提供什么（快照：tools/hooks/triggers/diagnostics）——
   `GET /v1/plugins`（`internal/harnessapi/plugins.go:56`）。
