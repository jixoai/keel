# 插件权限与审计

> 权限强制（白名单、审批、审计）为**规划中**，见
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`（「权限与沙箱」「审计」）。
> 它所基于的凭据引用纪律与进程/环境隔离在 `internal/gateway`、`internal/host`、
> `internal/servicehost` 中**已实现**，下文就地给出出处。

## 1. 先讲诚实的基线

keel **不对插件代码做沙箱**，并且把这句话说清楚——与本设计所参照的 DSH 模型立场一致：

- **进程内**插件被编译进宿主，以宿主权限运行；`permissions` 块是策略契约，在宿主可控的接缝
  处强制（工具派发、密钥解析、进程派生）并写入审计——**它不是内存沙箱**；
- **sidecar** 插件运行在自己的进程里；该进程边界加环境变量白名单*就是*隔离；
- 安装插件意味着执行第三方代码。因此**安装/启用/升级都需要显式的 durable 审批**——权限模型
  的全部意义在于：「它能运行」是一个决定，而不是默认值。

## 2. 清单声明什么

```yaml
permissions:
  fs: [workspace-write]        # 文件效应类别
  net: [api.example.com, …]    # 允许的 host
  secrets: [OPENAI_API_KEY, …] # 允许的密钥引用（只有名字，绝无值）
  tools: [example_status, …]   # 允许调用的工具
```

强制点（规划）及规格要求的失败模式：

| 授予 | 在哪强制 | 越权时 |
| --- | --- | --- |
| `tools` | 工具派发：执行前按调用方插件 id 校验白名单 | 调用以明确错误失败 + 审计 `(插件, 动作, 目标, 结果)` |
| `net` | 宿主可控的出站接缝（今天是 provider 调用；未来直连出口按声明 host 集） | 拒绝 + 审计 |
| `secrets` | 密钥引用解析：只有声明的名字可以解析 | 拒绝 + 审计；值永不进日志 |
| `fs` | 工作区访问外的文件效应类别检查 | 拒绝 + 审计 |
| sidecar env | 进程构造：`MinimalEnv` + 仅声明白名单 | 白名单外的名字不出现在子进程环境中 |

最后一行对外部网关**已实现**，对 sidecar 原样适用：被 spawn 的进程只拿到 `MinimalEnv` 加
显式声明的变量，引用经 `${NAME}` / `env:NAME` / `secret:NAME` 解析，且**字面密钥在校验期即
被拒绝**（`internal/gateway/process.go:106`、`internal/servicehost/models.go:546`、
`internal/host/config.go:203`）。同一条纪律今天也在保护模型凭据：租户密钥存储不可用时
`secret:` 引用 fail-closed——绝无明文存取（`cmd/keel/serve.go:165`）。

## 3. 审批：由 decision 原语发问

安装/启用/升级就是 durable、可回答的问题；Harness 已经为这种形状准备了原语
（[审批与问句](/zh/harness/approvals-and-questions)）：

```
安装请求 ──▶ decision（kind=approval, options=[allow, deny]）
               │
               ├─ 作答 allow ──▶ 校验清单 + 版本门 + hash → installed → 审计
               ├─ 作答 deny  ──▶ 不生效；审计记录拒绝
               └─ 超时 / 未作答 ──▶ 不生效（fail-closed）
```

规格明确的规则（`spec: 权限与沙箱`）：审批门禁安装/启用/升级；拒绝则不生效；审批本身是带
答案日志的 durable 行（绝不是一次性的弹窗）。超时按 fail-closed 处理。

## 4. 审计

每一次生命周期动作（`install`/`enable`/`disable`/`uninstall`/`update`）与每一次权限拒绝，都
记录**时间、操作者、插件 id、版本、动作、结果**，可通过 `GET /v1/plugins/audit` 查询
（`spec: 审计`）。当前代码库已在两个层面演示了该模式：

- 触发器审计保留逐次触发历史与最近 N 条审计窗口
  （`internal/agentext/triggers.go:86`、`:135`）；
- 内核审计 sink 把每次拒绝投影为结构化日志字段（reason/op/tenant/session/kind/owner/epoch）
  并限频（`internal/servicehost/audit.go`、`docs/LOADOUT.md` §6）。

插件审计沿用同一风格：有界记录、只含标识、无载荷无密钥。

## 5. 存量风险，如实列出

以下风险已知且刻意不隐藏（`spec` + `docs/DSH-REFERENCE.md` §4）：

| 风险 | 为何存在 | 缓解路径 |
| --- | --- | --- |
| 进程内插件代码可以越出其声明权限 | 无内存沙箱 | 官方插件按构造受信；第三方逻辑应放 sidecar |
| 无视 stdin EOF 的 sidecar 能在宿主被杀后存活 | 正常回收只走关闭路径 | 协议义务（子进程 EOF 即退）+ 规划中的 pidfile sweep / 外部 supervisor |
| 暂无 per-plugin CPU/内存预算 | 预算今天属部署侧 | 规划在 spawn 处接 per-backend 预算（cgroup/rlimit），越限拒绝激活 |
| 密钥引用是名字而非作用域 | 授予名字即授予值 | 只声明最窄的名字；值永不进日志或账本 |

## 6. 复核者应能核对的清单

对任一启用插件，仅凭 API 面就应能回答：

1. 它声明了什么（清单）——[清单](/zh/harness/plugins/manifest)；
2. 它被允许触碰什么（权限）——本页；
3. 它实际做了什么（审计）——§4；
4. 谁在何时批准了它（decision 行）——§3；
5. 它此刻提供什么（快照：tools/hooks/triggers/diagnostics）——
   `GET /v1/plugins`（`internal/agentapi/ext.go:18`）。
