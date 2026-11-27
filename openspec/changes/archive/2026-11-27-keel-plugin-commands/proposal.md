# Proposal: keel-plugin-commands

> 立项：2026-11-26。基线：`docs/PI-PARITY.md`（`/skill:name` 强制加载）、
> `docs/HARNESS.md` §2（`POST /v1/sessions/{id}/compact`）、
> `internal/pluginpkg/cli.go`（`provides.cli` 的 CLI 能力树）、
> `web/src/screens/composer-completion.tsx`（`/learn` 与 `$` 补全）、
> `web/src/screens/command-palette.tsx`（客户端导航面板）、
> `openspec/changes/archive/2026-11-24-keel-harness-v2/RECONCILIATION.md` 第 67 行
> （`/skill:name` 无注册表）。
> 参考：DSH `deepseek-ai/dsh-superagent-prototype@8c32a9dc` 的 “command” 是其 Engine/CLI
> 内部命令，不是聊天斜杠命令；本 change 不套用其命令模型，只对齐 PI 的宿主侧语义。
> 定位：命令是**有名字、有权限、可审计的宿主动作入口**，不是 prompt 模板，不是工具，
> 也不是 `keel <verb>` 的 CLI。

## Why

当前三类“命令”各自为政：

- `/learn` 是 Composer 里的**客户端提示词模板**：它拼一段文本再走普通发送，没有权限、
  没有 durable 记录，也不该被误认为宿主能力；
- `/compact` 有服务端 seam（`session.write` + requestId 幂等 + 压回策略），但没有聊天入口；
- `/skill:name` 是 PI parity 的**孤儿**：没有斜杠命令注册表，也没有强制加载路径；
- 插件已有 `provides.cli`（shell CLI 能力树），但聊天里无法贡献命令；`provides.tools`
  是模型面，不是用户意图面；
- 命令面板只是客户端导航搜索，不是命令通道。

如果下一波插件各自在 Composer 里加 `/foo`，会立刻出现命名冲突、权限绕过、prompt 注入、
TUI/web 行为分叉和第二套审计。本 change 先冻结注册表、执行语义与边界，再让内置命令与
插件命令接入。

## What Changes

1. **命令目录**：服务端为每个会话发布 `GET /v1/sessions/{id}/commands`，聚合内置命令、
   已启用插件的 `provides.commands`、已安装 skills 的 `/skill:<name>`；按固定顺序返回
   `name/summary/argsSchema/danger/idempotent/targets/source/requiredPermissions/available`。
2. **插件声明**：`plugin.yaml` 新增 `provides.commands[]`，每个命令 MUST 绑定本插件已声明的
   `provides.tools` 中的工具（不新增执行原语）；安装审批展示命令列表；停用/卸载即撤销。
3. **执行**：`POST /v1/sessions/{id}/commands/{name}`，body `{args, requestId}`；同一
   requestId 幂等；一次调用只产生一个 durable 结果；按 `session.write` 与命令/工具权限
   判定；destructive 命令必须走 durable decision，拒绝/超时 fail closed。
4. **命令不是 prompt 模板**：命令 MUST 映射到宿主动作或已审批工具；MUST NOT 通过拼接
   `user.message`/system prompt 来“实现”命令。`/learn` 保留为客户端模板，MUST NOT 进入
   命令目录；`/skill:<name>` 是宿主按引用加载技能，不是文本展开。
5. **内置命令**：`/compact` 委托既有压缩 seam；`/skill:<name>` 委托技能加载器；两者与
   HTTP/工具路径共用同一实现，不写第二套。
6. **客户端一致**：web 与 TUI 共用服务端目录、同一解析规则与同一结果语义；Composer 输入
   `/` 打开命令菜单，命令面板增加“命令”分组；不可用/无权限/目标不支持必须显式展示。
7. **审计与可观测**：每次调用写 durable `command.invoked` 记录（命令名、来源、actor、
   tenant/session、requestId、状态、耗时、参数哈希/长度、decisionId），不含参数正文、
   密钥或消息内容；插件拒绝写既有 `plugin_audit`。

## Capabilities

### New Capabilities

- `plugin-commands`: 命令注册/发现、插件声明、durable 执行、权限/审批、内置命令与
  web/TUI 一致性。

### Modified Capabilities

<!-- 复用 plugin-system 的生命周期/审批、tool-surface 的执行授权、session 的会话边界与
     compact seam；本 change 只新增命令 capability。若实现必须改既有 requirement 的正文，
     实现阶段补精确 delta，本提案不虚构修改。 -->

## Impact

- `internal/pluginpkg`（`provides.commands` 解析与字段级校验）、`internal/harnessext`
  （命令注册/撤销、工具映射、审批展示）、`internal/harnessapi`（目录与执行路由）、
  `internal/access`（复用既有 key，不新增身份枚举）。
- `web/src/screens/{composer,command-palette,messages}.tsx`、TUI 同一份 App.tsx；
  `docs/HARNESS.md`/`docs/PI-PARITY.md` 增补命令契约。
- 不新增命令执行引擎：插件命令 = 已审批工具的命名入口；内置命令 = 既有 seam 的命名入口。
- 测试：目录过滤/排序、字段校验、权限矩阵、decision、幂等、并发、租户、TUI/web 一致。

## Out of scope

- 命令别名、宏/脚本语言、参数表达式、远程命令市场。
- 运行中（有在途 turn）执行命令；首版所有命令要求 session idle，忙时 409。
- 把 `/learn` 升级为服务端命令；它仍是客户端提示词模板。
- 用命令替代工具、MCP、skills 或 `keel` CLI；四者边界见 design §2。
- 插件在命令里直接执行未声明的副作用或绕过 `plugin.invoke`。
