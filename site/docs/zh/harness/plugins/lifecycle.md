# 插件生命周期

> **已实现。** 生命周期 API——`installed → enabled → disabled → uninstalled`，加上 `failed` 与
> `invalid`、安装/升级路径、durable 审批与审计轨迹——均在本仓库交付
> （`internal/harnessext/plugin_manager.go:29`、`internal/harnessapi/plugins.go:56`）。
> 它所依赖的装配机制来自 `harnessext` 运行时，下文逐一给出出处。

## 1. 状态机

```
  （不存在）──安装──▶ installed ──启用──▶ enabled ──停用──▶ disabled
                        ▲                 │                 │
                        └──── 升级 ───────┴─────────────────┘
                                 （同 id、新版本）

  enabled | installed | disabled ──（包在磁盘上不再通过校验）──▶ invalid
  installed | disabled ──（启用无法装载；宿主编排回滚）──▶ failed
  failed ──启用（重连）──▶ enabled
  disabled ──卸载──▶ uninstalled    （行被删除；仅审计可见）
```

六个状态是闭集（`internal/harnessext/plugin_manager.go:29`，取值自 `:33`；
`LifecycleStates` 在 `:49`）：

| 状态 | 含义 |
| --- | --- |
| `installed` | 受管根目录中的合法包，尚未启用 |
| `enabled` | 运维要求启用，且运行时（对纯 UI 的 webview 则是 UI 宿主）正在服务它 |
| `disabled` | 曾经启用、现已完全卸载；包仍在 |
| `failed` | 启用未达到活跃状态；保留运行时自己的诊断 |
| `invalid` | 包在磁盘上不再通过校验；行保留状态，让原因可见 |
| `uninstalled` | 终态；该状态的行会被删除，因此只出现在审计轨迹中 |

- `安装`校验清单、`engines.keel` 版本门与包内容树，把包拷入受管根目录并写审计行
  （[清单](/zh/harness/plugins/manifest)）。
- `停用`卸载并保留包；`卸载`移除受管目录（含归档版本），在插件处于 `enabled` 或有启用依赖方
  时被拒（`internal/harnessext/plugin_manager_ops.go:817`）。内置插件只能停用，不能卸载
  （`:824`）。
- `升级`以同 id 替换版本——任一时刻每个 id 只有一个启用版本；旧包归档到
  `<root>/.versions/<id>/`，启用失败则回滚旧版本
  （`internal/harnessext/plugin_manager_ops.go:987`）。
- `failed` 不是半装载：materialize 失败或运行时激活失败时，宿主先**不含该插件**重新装配，再
  记录 `failed`（若只是在等依赖则记 `disabled`）及原因
  （`internal/harnessext/plugin_manager_ops.go:713`）。

**启用与停用是装配操作**，不是翻标志位。启用让插件走依赖顺序激活；停用必须移除它注册的
*每一个*副作用：

| 承诺 | 机制（已实现） | 出处 |
| --- | --- | --- |
| 依赖顺序激活 | 迭代式激活循环；依赖被 provide 后依赖方才激活 | `internal/harnessext/plugin.go:237` |
| 依赖缺失 ⇒ 不装载且可见 | 状态 `PENDING` + `waiting for: <ids>` 诊断 | `internal/harnessext/plugin.go:291` |
| 停用移除工具 | 每次装配后重算已发布工具集 | `internal/harnessext/plugin.go:438` |
| 停用移除订阅/触发器 | `unsubs` 与 `triggerStops` 按反序释放 | `internal/harnessext/plugin.go:386` |
| 停用时声明的清理恰好执行一次 | `effectsRun` 记账防止部分卸载重复执行 | `internal/harnessext/plugin.go:396` |
| 激活失败回滚自身 effects | 失败路径在返回前调用 `cleanup` | `internal/harnessext/plugin.go:317` |
| 每次变化可见 | 每次装配 generation 恰好 +1；快照报告状态与诊断 | `internal/harnessext/plugin.go:209`、`:505` |

规格的验收场景把这同一件事说成产品语言：*启用一个提供工具 `x` 与 webhook 触发器的插件，
停用它——模型侧不再有 `x`，触发器不再触发，清单显示 `disabled`，且无任何报错*
（`openspec/specs/plugin-system/spec.md`）。

## 2. 启动时持久意图获胜

`Manager.Boot` 把数据库、磁盘与内置集合对账（`internal/harnessext/plugin_manager_ops.go:32`）：

- 内置插件的行被刷新，**持久化**的状态获胜——被运维停用的插件重启后仍然停用；只有首次启动
  才应用默认启用值；
- 插件根下发现的包被登记（已安装版本优先，否则取最高版本）；
- 包消失或不再通过校验的行转为 `invalid`，问题保留在 diagnostics 中，而不是行悄悄消失。

这就是为什么启用状态是一个 durable 决定，不是进程内标志。

## 3. 依赖规则

- `requires` 指向**插件或服务**。当有活跃者 `provides` 该名字时依赖才算满足；无法满足的插件
  为 `disabled` 并给出具名原因（装配内则为 `PENDING`），绝不部分装载
  （`internal/harnessext/plugin.go:291`、`internal/harnessext/plugin_manager_ops.go:690`）。
- 工具是全局命名空间：两个启用插件不得提供同名工具。无论装配顺序如何，第二个 fail-closed
  并给诊断（`internal/harnessext/plugin.go:236`、`:454`）。
- Hook 事件必须属于闭集；未知事件在订阅时报错，绝不静默 no-op
  （`internal/harnessext/hooks.go:249`）。
- 有启用插件依赖目标时，停用与卸载被拒，因此一次停用绝不会让依赖方悬挂在 pending
  （`internal/harnessext/plugin_manager_ops.go:788`）。

## 4. 插件启动过的工作会怎样

| 工作项 | 停用时 |
| --- | --- |
| 插件提供的工具 | 下一次装配时从模型工具集中移除 |
| hook 订阅 | 立即释放（反序） |
| 触发器 | 注销；不再触发；既有审计行保留 |
| 声明的 effect（定时器、监听器、连接） | `Stop(ctx)` 在 2s 有界上下文中执行；失败只记日志，绝不致命 |
| sidecar 进程 | 协议层 `close` 通知，随后关 stdin → 宽限 → 进程组 SIGTERM → SIGKILL |
| 插件派生的子代理会话 | 继续运行——它们是账本中的会话，不是插件状态 |

（effects 见 `internal/harnessext/plugin.go:386`；sidecar 关闭路径见
`internal/gateway/stdio.go:228`。）

## 5. 不打扰在途工作的热更

插件是进程内注册，因此更新就是一次**重新装配**。两层保护确保它不打扰进行中的工作：

1. **产品层（今天）。** `Runtime.Apply` 先卸载上一代装配，再激活新装配并令 generation 恰好
   +1。引用已移除插件的 hook 回调与触发器触发会在各自计数器上可见地失败；已提交的 run 不受
   影响，因为 run 自身的注册活在转录里，而不是插件表里
   （`internal/harnessext/plugin.go:163`、`internal/harnessext/hooks.go:399`）。
2. **服务层（插件成为服务之处）。** 服务图为在途引用引入 generation：`RETIRING` 实例不接新
   run，待引用归零后按反序依赖卸载，或超过退休截止强制卸载——强制退休会把持有者的租约标记
   为 broken，使 run 以 `drain-expired` 诊断失败，而不是对着已死实例写入
   （`internal/servicegraph/lifecycle.go:21`、`internal/servicegraph/engine.go:270`、
   `:303`、`:650`）。

## 6. 审计

每一次生命周期动作——`install`、`enable`、`disable`、`uninstall`、`update`——与每一次权限
拒绝，都记录**时间、操作者、插件 id、版本、动作、结果**，并可通过
`GET /v1/plugins/audit` 查询（`internal/harnessapi/plugins.go:256`）。权限拒绝以 `deny`
动作落在同一张表（`internal/harnessext/plugin_permissions.go:19`）。审计行在 HTTP 响应之前
写入，`audit_id` 即 keyset 游标；审批被拒的行也会写入，即使管理器从未被调用
（`internal/harnessapi/plugin_approval.go:412`）。

表本身是 `plugin_audit`（`internal/store/schema.sql:603`）；清单表是 `plugins`
（`internal/store/schema.sql:582`）。

## 7. UI 中的生命周期——后端与客户端

后端面是完整的：`GET /v1/plugins` 返回清单（root、revision、generation、逐插件视图含
`state`、`bundled`、`source`、`path`、`contentHash`、槽位/贡献、诊断），五个写端点与审计端点
均已挂载（`internal/harnessapi/plugins.go:56`）。

客户端落后于该面，且这个缺口写在源码里：`web/src/screens/plugins.tsx` 从
`PluginStatusView` 渲染每张卡片，安装/更新卡的位置仍是一个 Empty——「the entry that owns the
install surface and the plugin UI route」（`web/src/screens/plugins.tsx:1`）。所以：
**API 已实现，客户端安装/更新卡尚未接线**。从清单打开插件 UI 已接线：web target 内嵌
`/plugins/<id>`，终端 target 调 `openUrl` 并保留可复制 URL
（[插件 UI](/zh/harness/plugins/ui)）。
