# 清单（`plugin.yaml`）

> **规划中。** 本页的包格式与全部规则来自
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`（「插件包与清单」「发现、
> 安装与包存储」）。当前检出中这些均未实现；已实现的进程内契约在本页末尾对照说明。

## 1. 包

插件**就是一个目录**：

```
<plugins-root>/<id>/
├─ plugin.yaml            清单（必需）
├─ ui/                    webview 资源（kind: webview 必需）
│   ├─ index.html
│   └─ assets/…
└─ bin/                   sidecar 入口（kind: sidecar 必需）
```

包内容存放于**受管目录**；清单不得指向任意外部路径，安装会记录包内容 hash
（`spec: 发现、安装与包存储`）。

## 2. 清单——全字段

```yaml
id: vendor.name                # ^[a-z][a-z0-9._-]{1,63}$；全局唯一；同 id 只能有一个启用版本
version: 1.2.3                 # semver
kind: in-process | sidecar | webview
description: 一句话
engines: { keel: ">=0.2.0 <0.3.0" }        # 宿主版本门
provides:
  tools: [name, ...]           # 面向模型的工具名；全局唯一（冲突 fail-closed）
  hooks: [event, ...]          # 必须属于 hook 闭集
  triggers: [id, ...]
  ui: { entry: ui/index.html, title: "..." }   # webview 必填
requires: [plugin-or-service-id, ...]          # 依赖缺失 ⇒ 不装载（不半装载）
permissions:
  fs: [workspace-write]        # 文件效应类别
  net: [host, ...]             # 允许的 host
  secrets: [NAME, ...]         # 允许的密钥引用
  tools: [name, ...]           # 允许调用的工具
entry: { command: [...], args: [...], env: [...] }   # sidecar 必填；in-process 可为空
source: { type: builtin | local | git | npm, ... }
```

逐字段语义与强制点：

| 字段 | 含义 | 在哪强制 |
| --- | --- | --- |
| `id` | 全局身份；目录名；审计主体 | 安装校验 |
| `version` | semver；同 id 第二个启用版本被拒 | 安装/升级 |
| `kind` | 三种之一；决定 `entry`/`ui` 的合法形状 | 安装校验、[三种 kind](/zh/harness/plugins/kinds) |
| `engines.keel` | 插件支持的 semver 区间；不匹配则安装**与**启用都被拒，并明示区间 | 版本门 |
| `provides.tools` | 插件贡献的工具名；启用插件间重名即 fail-closed 冲突 | 工具派发、装配 |
| `provides.hooks` | 对**闭集**事件的订阅（当前：`subagent.*`、`trigger.*`、`turn.*`、`decision.*`；`internal/agentext/hooks.go:18`） | 订阅注册 |
| `provides.triggers` | 插件拥有的触发器 id（`interval`/`cron`/`webhook`） | 触发器引擎 |
| `provides.ui` | webview 入口文档 + 标题；`kind: webview` 必填 | UI 托管 |
| `requires` | 必须存在的插件或服务 id；缺失 ⇒ `disabled` 并给原因，绝不半装载 | 生命周期装配 |
| `permissions` | 能力预算；见[权限](/zh/harness/plugins/permissions) | 派发、环境构造、审计 |
| `entry` | sidecar 进程的 command/args/env（env 只能取白名单名） | 进程监督 |
| `source` | 包的来源（`builtin`、`local`、`git`、`npm`…） | 安装 + 审计 |

不属于单字段级别、但同属契约的规则：

- **清单非法 → 拒绝安装并给出字段级原因**；插件目录不被采用（`spec: 清单非法即拒绝`）；
- **版本门**——`engines.keel` 不匹配时安装与启用都被拒，并给出支持的区间（`spec: 版本门`）；
- **内容 hash + 审计**——安装时校验清单、版本门与包内容 hash，并写审计记录
  （`spec: 发现、安装与包存储`）；
- **安装即出现**——安装后 `GET /v1/plugins` 立即列出该插件（状态
  `installed`/`enabled`），UI 清单同步刷新（`spec: 安装即出现`）。

## 3. 发现、安装与包存储（规划中的 API 面）

| 操作 | 面 | 语义 |
| --- | --- | --- |
| 发现 | 启动时扫描 `plugins-root` | 磁盘上发现的插件出现在清单里 |
| 安装 | 安装 API（本地目录 / git / 包） | 校验清单 + 版本门 + hash；写审计 |
| 列表 | `GET /v1/plugins`（由该 change 扩展） | 状态 `installed`/`enabled`/`disabled` + 诊断 |
| 卸载 | 卸载 API | 移除受管目录 + 审计；有活跃 run 使用时不可移除 |
| 升级 | 同 id 换新版本 | 版本替换；启用路径重新走依赖拓扑装配 |

今天的 `GET /v1/plugins` 已存在，返回的是装配结果——generation、插件状态、hooks 摘要、
工具名（`internal/agentapi/ext.go:18`、`internal/agentext/plugin.go:505`）。该 change 扩展这份
响应以包含已安装/已停用的包，而不是另造第二个端点。

## 4. 今天没有这份清单，有什么

已实现的进程内契约承载同样的*概念*，只是没有文件：`ID`、`Version`、`Provides`、
`Requires`、`Tools`、`Hooks`、`Triggers`、`Effects`、`Start`、`Stop`
（`internal/agentext/plugin.go:67`）。映射是单向的——规划中的清单是同一份声明的序列化，
外加 Go 类型刻意省略的一切（权限、来源、UI）。

| `plugin.yaml` 概念 | 今天的 Go 等价物 | 备注 |
| --- | --- | --- |
| `id` / `version` | `Plugin.ID` / `Plugin.Version` | 快照可查 |
| `provides` / `requires` | `Plugin.Provides` / `Plugin.Requires` | 同一依赖拓扑装配 |
| `provides.tools` | `Plugin.Tools` | 重名 fail-closed |
| `provides.hooks` | `Plugin.Hooks` | 闭集由 `Subscribe` 强制 |
| `provides.triggers` | `Plugin.Triggers` | owner 由插件 id 填充 |
| `effects`（反序契约） | `Plugin.Effects` | 反序清理，带部分卸载防重入 |
| `kind` / `permissions` / `source` | — | **今天无等价物**；进程内代码构造上受信 |
