# Design: keel-plugin-commands

## 1. 定位

命令是**用户意图面**的命名动作：用户在 Composer 输入 `/name`，宿主校验权限与参数，执行
一个已经存在的宿主动作或已审批工具，并留下 durable 结果。命令不产生新的执行原语。

```text
Composer/TUI "/name args"
  → 客户端解析（首个 token = 目录里的 name）
  → POST /v1/sessions/{id}/commands/{name} {args, requestId}
  → 宿主：session 边界 → 权限 → 参数 schema → idle 检查 →（destructive）decision
  → 执行：内置 seam 或 Manager.InvokePluginTool
  → 持久结果 + command.invoked 审计
  → 响应（200/202/4xx/5xx）
```

## 2. 与相邻概念的关系（边界）

| 概念 | 谁发起 | 执行什么 | 是否进模型上下文 | 本 change 的关系 |
|---|---|---|---|---|
| 命令 `/name` | 人（Composer/TUI） | 宿主动作或已审批插件工具 | 不自动进入 | 新增注册表/执行路由 |
| 工具 `provides.tools` | 模型 | 插件/宿主工具 | 是（工具调用） | 命令的工具后端；不改变工具面 |
| `provides.cli` | 人（shell） | `keel <verb>` 子命令 | 否 | 词汇相近但通道不同；不复用解析器 |
| Skill `/skill:name` | 人（命令） | 按引用加载技能 | 是（下一轮） | 内置命令的一种 |
| `/learn` | 人（Composer） | 拼一段提示词后普通发送 | 是（作为 user.message） | **不是命令**；保留客户端模板 |
| 命令面板 Mod+K | 人（导航） | 会话/路由/插件动作 | 否 | 新增“命令”分组；仍是客户端入口 |

“命令不是 prompt 模板”的判定：若一个功能的全部效果是“把文本拼进 user.message 或 system
prompt”，它 MUST 留在客户端模板（如 `/learn`）或由 skill/tool 表达，MUST NOT 注册为命令。
命令可以触发宿主**按引用**加载内容（`/skill:name`）或写领域事件（`/compact`），但不能由
客户端把任意文本冒充成命令执行结果。

## 3. 目录与聚合

`GET /v1/sessions/{id}/commands`（`session.read`，会话归属；跨租户 404）返回：

```json
{
  "commands": [
    {
      "name": "compact",
      "source": "builtin",
      "summary": "压缩当前会话上下文",
      "argsSchema": { "type": "object" },
      "danger": "write",
      "idempotent": true,
      "targets": ["web", "native", "tui"],
      "requiredPermissions": ["session.write", "model.invoke"],
      "available": true
    }
  ],
  "truncated": false
}
```

来源与顺序：

1. 内置命令（固定声明顺序）；
2. 已启用且装配成功的插件命令，按 `pluginId` 字典序、插件内声明顺序；
3. skills，按 `skill:<name>` 字典序。

过滤：

- 停用/卸载的插件命令**不出现**；
- 启用但 `PENDING`/`FAILED` 的插件命令仍列出，`available=false` + 稳定原因
  （`plugin_pending`/`plugin_failed`）；
- 调用者缺权限时列出 `available=false` + `missing_permissions`，执行仍 403；
- `targets` 不含当前客户端目标时 `available=false` + `target_unsupported`，TUI 显示原因，
  不静默消失；
- 目录最多 256 条；超过时 `truncated=true`，按上述顺序截断（技能最多占 128 条）。

## 4. 插件声明

```yaml
provides:
  tools: [reindex]
  commands:
    - name: reindex                 # 本地名：^[a-z][a-z0-9-]{0,31}$
      summary: Rebuild the search index.
      description: Rebuilds the current workspace index; does not change files.
      args_schema:                  # 可选；默认 {"type":"object"}
        type: object
        properties:
          path: { type: string }
      tool: reindex                 # MUST 属于本插件 provides.tools
      danger: write                 # read | write | destructive
      idempotent: false             # 缺省 false
      targets: [web, native, tui]   # 可选；缺省全部
```

发布名：`/<plugin-id>:<name>`（如 `/keel.web-search:refresh`）。插件 id 文法不含 `:`，
因此不存在歧义；内置裸名（`compact`、`skill:<name>`）保留，插件 MUST NOT 声明裸名。

校验（安装/更新时 fail closed，字段级）：

- `name` 文法、插件内唯一；发布名全局唯一；
- `tool` MUST 出现在同一清单的 `provides.tools`；MUST NOT 指向其他插件或宿主聚合工具；
- `danger` 闭集；`idempotent` 缺省 false（安全读法）；
- `args_schema` MUST 是 JSON object schema，≤16 KiB；未知字段拒绝；
- `targets` 显式声明时非空且闭集；默认 `[web, native, tui]`；
- `summary` 1..200 字符；`description` ≤2048；
- 每个命令 MUST 至少有一个可用 target 与一个工具后端（不得留死命令）。

安装审批展示命令发布名、summary、danger、targets；停用/卸载在同一 generation 撤销目录项
与执行入口。

## 5. 执行语义

`POST /v1/sessions/{id}/commands/{name}`，`session.write`；body：

```json
{ "args": { "path": "docs" }, "requestId": "req_..." }
```

顺序（任何一步失败都不产生副作用）：

1. 会话归属（跨租户/不存在 404）与 `session.read` 关系；
2. 命令解析：`name` 必须在当前目录中，否则 404；插件未启用/失败按 §3 的 available 语义
   返回 404/503；
3. 参数校验：按 `args_schema` 严格校验，字段级 400；`args` 缺失视为 `{}`；未声明字段
   一律 400，MUST NOT 把剩余文本隐式塞进 `text` 或拼成 prompt；
4. idle 检查：session 有在途 turn / compaction 时 409 `session_busy`；首版所有命令 idle-only；
5. `danger=destructive` → durable decision；202 + `decisionId`；
   拒绝 403，超时 408，取消 409；未获批准 MUST NOT 调用工具；
6. 执行：
   - 内置：调用既有 seam（compact、skill loader）；
   - 插件：`Manager.InvokePluginTool(pluginID, tool, args)`，沿用工具授权
     （`plugin.invoke` + `permissions.tools` + 插件自身权限）；
7. 持久化结果与 `command.invoked`，返回 200；工具错误 502。

**幂等**：`requestId` 是唯一身份。同 requestId 重放返回首次记录的结果，MUST NOT 二次
执行工具或二次开 decision；若首次结果是失败，重放仍返回该失败（不自动重试）；需要重试
必须换新 requestId。并发同 requestId 按 key 串行化。

**结果形状**：

```json
{
  "requestId": "req_...",
  "command": "keel.web-search:refresh",
  "status": "ok",
  "data": { "indexed": 42 },
  "eventSeq": 1234,
  "durationMs": 87
}
```

`data` 是命令自己的结构化结果，≤64 KiB；超出截断并标记 `truncated`。结果不进模型上下文，
除非命令显式调用既有输入 seam（本 change 无此命令）。

## 6. 内置命令

| 名称 | 权限 | 实现 | 语义 |
|---|---|---|---|
| `compact` | `session.write` + `model.invoke` | 既有 `CompactSession` seam | 手动压缩；busy 409；策略关闭 409；同 requestId 幂等；既有 HTTP 端点在本 change 同步收紧为同一权限 |
| `skill:<name>` | `session.write` + `skill.read` | 既有 skills loader | 为**下一轮**强制加载该技能；缺技能 404；`disable-model-invocation` 不阻止用户命令；不是文本展开 |

`/learn` 明确不注册：它由 Composer 构造普通消息，仍可工作；命令目录 MUST NOT 出现
`learn`。`/compact` 的 HTTP 端点保留，命令委托同一 seam，避免两套实现。

## 7. 客户端（web/TUI）

- Composer 输入以 `/` 开头且处于首 token 时打开命令菜单；数据来自目录，不本地硬编码
  （`/learn` 是唯一客户端模板，单独展示为模板而非命令）。
- 选择命令后：剩余文本在 schema 只有一个 `text` 字符串属性时作为 `args.text`；
  否则客户端展示由 `args_schema` 生成的简单表单（string/number/boolean/enum/array），
  不猜测参数；TUI 用同一 helper 和同一目录。
- 命令面板（Mod+K）增加“命令”分组；选择即执行同一路由；没有会话时命令分组提示先选会话。
- 五态：加载（目录请求中）、空（无可用命令）、错误（目录/执行失败）、无权限
  （`available=false` + missing key）、成功（结果行/事件行）。
- 执行结果作为 `command.invoked` 行进入对话投影（不伪装成 user/assistant 消息）；`compact`
  仍按既有 `compaction` 事件呈现。
- `command.invoked` 是审计/投影行，不进入模型上下文；会话事实仍以事件日志为准，命令
  MUST NOT 伪造 user/assistant 事件，`/compact` 只产生既有 compaction 事件。

## 8. 权限、租户与安全

- 路由权限：目录 `session.read`；执行 `session.write`。命令自身权限：
  - 内置按表声明；
  - 插件命令继承目标工具的 `plugin.invoke`、工具在 registry 声明的权限 key 与插件
    `permissions.tools`；目录中的 `requiredPermissions` 由 tool registry 派生，插件
    MUST NOT 自行声明更宽的 key。
- 参数是不可信输入：MUST NOT 拼进 SQL/shell/prompt；插件工具按自己的 schema 处理；
  审计只存 `sha256(args)` 与长度，不存正文。
- 跨租户：目录与执行都按 principal tenant + session 归属；foreign session 404；插件命令
  只来自当前 tenant 启用的插件。
- 拒绝（权限/审批/参数/忙）MUST fail closed，且不产生部分副作用；拒绝写审计。
- 命令名/摘要可展示；命令参数与结果不得出现在审计、日志或未授权客户端。

## 9. 失败模式

| 情形 | 响应 | 副作用/审计 |
|---|---|---|
| 未知命令 | 404 | 无；可选 `command.denied reason=unknown` |
| 缺 session.write/命令权限 | 403 | 无；`permission.denied` |
| 参数 schema 失败 | 400 字段级 | 无 |
| session busy | 409 `session_busy` | 无 |
| decision 拒绝 | 403 | 无；`decision` + `command.invoked refused` |
| decision 超时 | 408 | 无；同上 |
| 插件未启用 | 404 | 无 |
| 插件 PENDING/FAILED | 503 + reason | 无 |
| 工具错误 | 502 | 工具自己的事实保留；`command.invoked error` |
| 结果过大 | 200 + `truncated` | 结果截断，不丢状态 |

## 10. 兼容与迁移

- Alpha 零兼容：不保留旧客户端本地 `/learn` 之外的命令列表，不做 `/cmd` 双写、别名或
  重定向；旧 `POST /compact` 保留为既有端点，命令委托它。
- `provides.cli` 与 `provides.commands` 并存：前者是 shell CLI 能力树，后者是聊天命令；
  同名不互斥，但发布面不同。
- `provides.tools` 工具面不变；命令只是命名入口，不改变 `toolManifestHash` 语义。

## 11. 非目标

- 运行中命令、命令脚本/宏、别名、参数表达式、远程命令市场；
- 插件在命令里直接执行未声明的副作用；
- 把 `/learn`、`/help` 等纯文本模板纳入服务端命令；
- 用命令替代工具/MCP/skills/CLI。
