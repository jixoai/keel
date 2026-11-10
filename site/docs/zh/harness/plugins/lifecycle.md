# 插件生命周期

> 生命周期 API（`installed → enabled → disabled → uninstalled`、安装/启用/停用/卸载端点、
> 版本替换）为**规划中**，见
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`（「生命周期」）。
> 它所依赖的*装配机制*在 `internal/agentext` 与 `internal/servicegraph` 中**已实现**，
> 下文逐一给出出处。

## 1. 状态机

```
            安装                  启用                     停用
  （不存在）─────▶ installed ─────────▶ enabled ◀───────────┐
                    │   ▲               │                   │
                    │   │   升级        │  停用             │
                    │   └─（同 id、新版本）▼                  │
                    │              disabled ────────────────┘
                    │                   │
                    ▼                   ▼
               uninstalled ◀────────── 卸载
```

- `安装`校验清单、`engines.keel` 版本门与包 hash，把包存入受管目录并写审计行
  （[清单](/zh/harness/plugins/manifest)）。
- `uninstalled` 移除受管目录；插件处于启用且使用中时拒绝卸载。
- `升级`以同 id 替换版本——任一时刻每个 id 只有一个启用版本。

**启用与停用是装配操作**，不是翻标志位。启用让插件走依赖顺序激活；停用必须移除它注册的
*每一个*副作用。已实现的宿主对进程内插件正是如此，规划中的生命周期直接复用：

| 承诺 | 机制（已实现） | 出处 |
| --- | --- | --- |
| 依赖顺序激活 | 迭代式激活循环；依赖被 provide 后依赖方才激活 | `internal/agentext/plugin.go:257` |
| 依赖缺失 ⇒ 不装载且可见 | 状态 `PENDING` + `waiting for: <ids>` 诊断 | `internal/agentext/plugin.go:291` |
| 停用移除工具 | 每次装配后重算已发布工具集 | `internal/agentext/plugin.go:438` |
| 停用移除订阅/触发器 | `unsubs` 与 `triggerStops` 按反序释放 | `internal/agentext/plugin.go:387` |
| 停用时声明的清理恰好执行一次 | `effectsRun` 记账防止部分卸载重复执行 | `internal/agentext/plugin.go:396` |
| 激活失败回滚自身 effects | 失败路径在返回前调用 `cleanup` | `internal/agentext/plugin.go:317` |
| 每次变化可见 | 每次装配 generation 恰好 +1；快照报告状态与诊断 | `internal/agentext/plugin.go:209`、`:505` |

规格的验收场景把这同一件事说成产品语言：*启用一个提供工具 `x` 与 webhook 触发器的插件，
停用它——模型侧不再有 `x`，触发器不再触发，清单显示 `disabled`，且无任何报错*
（`spec: 停用即撤销`）。

## 2. 依赖规则

- `requires` 指向**插件或服务**。当有活跃者 `provides` 该名字时依赖才算满足；无法满足的插件
  保持 `disabled` 并给出具名原因，绝不部分装载（`spec: 依赖缺失不装载`；机制：
  `internal/agentext/plugin.go:291`）。
- 工具是全局命名空间：两个启用插件不得提供同名工具。无论装配顺序如何，第二个 fail-closed
  并给诊断（`internal/agentext/plugin.go:236`、`:454`）。
- Hook 事件必须属于闭集；未知事件在订阅时报错，绝不静默 no-op
  （`internal/agentext/hooks.go:249`）。

## 3. 不打扰在途工作的热更

插件是进程内注册，因此更新就是一次**重新装配**。两层保护确保它不打扰进行中的工作：

1. **产品层（今天）。** `Runtime.Apply` 先卸载上一代装配，再激活新装配并令 generation 恰好
   +1。引用已移除插件的 hook 回调与触发器触发会在各自计数器上可见地失败；已提交的 run 不受
   影响，因为 run 自身的注册活在转录里，而不是插件表里
   （`internal/agentext/plugin.go:186`、`internal/agentext/hooks.go:399`）。
2. **服务层（插件成为服务之处）。** 规划中的统一把官方插件路由到服务图，后者为在途引用引入
   generation：`RETIRING` 实例不接新 run，待引用归零后按反序依赖卸载，或超过退休截止强制
   卸载——强制退休会把持有者的租约标记为 broken，使 run 以 `drain-expired` 诊断失败，而不是
   对着已死实例写入（`internal/servicegraph/lifecycle.go:21`、
   `internal/servicegraph/engine.go:270`、`:303`、`:650`）。

## 4. 插件启动过的工作会怎样

| 工作项 | 停用时 |
| --- | --- |
| 插件提供的工具 | 下一次装配时从模型工具集中移除 |
| hook 订阅 | 立即释放（反序） |
| 触发器 | 注销；不再触发；既有审计行保留 |
| 声明的 effect（定时器、监听器、连接） | `Stop(ctx)` 在 2s 有界上下文中执行；失败只记日志，绝不致命 |
| sidecar 进程 | 协议层 `close`，随后关 stdin → 宽限 → 进程组 SIGTERM → SIGKILL |
| 插件派生的子代理会话 | 继续运行——它们是账本中的会话，不是插件状态 |

（effects 见 `internal/agentext/plugin.go:387`；sidecar 关闭路径见
`internal/gateway/stdio.go:228`。）

## 5. 审计

每一次生命周期动作——`install`、`enable`、`disable`、`uninstall`、`update`——与每一次权限
拒绝，都必须记录**时间、操作者、插件 id、版本、动作、结果**，并可通过
`GET /v1/plugins/audit` 查询（`spec: 审计`）。审计车道复用触发器审计今天的同一纪律：有界
记录、按时间序、含操作者与结果（既有审计条目形状见 `internal/agentext/triggers.go:86`）。

## 6. UI 中的生命周期

当前设置界面已渲染装配结果——generation、逐插件状态徽标（`active`/`pending`/`failed`/
`stopped`）、提供的工具、hooks、触发器与诊断（`web/src/screens/settings.tsx:1005`、`:1148`）。
规划中的生命周期在同一张表上新增 `installed`/`disabled` 行，以及
启用/停用/卸载/更新动作与审计视图——表还是那张表，只是状态更多。
