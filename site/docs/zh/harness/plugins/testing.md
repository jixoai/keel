# 插件测试

> **已实现。** 下文的规则、四层测试与负控清单由已交付的套件强制：管理器、权限、sidecar、
> 扩展、创造模式与客户端槽位测试分别存在于 `internal/harnessext/*_test.go`、
> `internal/harnessapi/*_test.go` 与 `web/src/plugins/`。覆盖矩阵是 `plugins/COVERAGE.md`，
> 机读真源是 `plugins/keel.plugin-market/catalog.json`，门禁是
> `node scripts/plugin-catalog.mjs --check`（已接进 `.github/workflows/ci.yml` 与
> `scripts/acceptance-all.sh`）。

## 1. 规则

> 插件规格中的每个 Scenario 都必须有测试；**负控是强制的**，不是可选项。

这是服务图 change 给自己定下的同一规则：每个 spec scenario 有测试，且只有快乐路径、没有断言
失败模式的验收不算通过（`docs/SERVICE-GRAPH.md` §6）。对插件而言，「装载成功了」是最无趣的
事实；「停用后零残留、被拒时 fail-closed」才是有趣的那个。

## 2. 四层测试

| 层 | 回答的问题 | 运行方式 | 落在哪 |
| --- | --- | --- | --- |
| **单测** | 纯逻辑是否正确？清单解析、版本区间门、权限策略 | `go test ./internal/…` | `internal/pluginpkg/*_test.go`、`internal/harnessext/plugin_manager_test.go`、`plugin_permissions_test.go` |
| **集成** | 插件在真实宿主中是否如愿？安装 → 启用 → 工具调用 → hook → 触发器 → 停用 | 对真实 PG 跑 `go test -race` | `internal/harnessext/plugin_manager_test.go`、`e2e_test.go`、`approve_plugin_test.go`、`plugin_sidecar_test.go` |
| **conformance** | 跨实现/版本是否守住契约？ | conformance 套件 | `internal/agentloop/conformance_suite_test.go`、`docs/PI-CONFORMANCE.md` |
| **负控** | 每个失败模式是否以*正确方式*失败？ | 对错误/状态显式断言 | 上述插件套件 + `scripts/external-gateway-smoke.sh` |

### 单测

- 清单：缺 `id`/`version`/`kind`、id 不合文法、semver 非法、未知 `kind`、`engines.keel`
  超区间——每种都以**字段级原因**失败，且插件目录不被采用
  （`internal/pluginpkg/manifest_test.go`，校验器在 `internal/pluginpkg/manifest.go:653`）。
- `provides.hooks` 中不在闭集里的条目在注册时被拒，绝不静默忽略
  （`internal/harnessext/hooks.go:249`）。
- 工具重名检测与顺序无关：谁第二个被装配，谁 fail-closed
  （`internal/harnessext/plugin.go:236`、`:454`）。
- 权限策略：未声明的工具/host/密钥是 `*PermissionDeniedError` 并写一条审计行
  （`internal/harnessext/plugin_permissions_test.go`）。
- 扩展声明：非法的槽位/贡献是字段级错误，合法的会被发布到清单
  （`internal/harnessext/plugin_extension_test.go`）。

### 集成

已实现的扩展 E2E 就是模板：*插件注册 → 触发器 fire → hook 投递 → 子代理派生 → 取消在线
attempt*（`internal/harnessext/e2e_test.go`，摘要见 `docs/HARNESS.md` §5）。插件管理器套件
还证明停用路径：停用一个提供工具 `x` 与 webhook 触发器的插件后，工具从模型工具集消失、
触发器不再触发、清单显示 `disabled` 且无错误残留
（`internal/harnessext/plugin_manager_test.go`）。

安装路径按真实门禁测试：`202` → 作答 → 带 `decisionId` 重试 → `201`；拒绝 → `409`
（`internal/harnessapi/plugin_approval_test.go`）。

### Conformance

- **loop conformance 保持全绿。** 插件不能改变 loop 契约；PI conformance 套件是门禁
  （`internal/agentloop/conformance_suite_test.go`）。
- **官方套件就是矩阵。** `plugins/COVERAGE.md` 以闭集状态词（`delivered`、`partial`、
  `planned`、`registered`、`merged`、`absent`）与闭集波次（`W0.5`、`W1`、`W2`、`W3`、
  `never`）逐条跟踪 DSH 能力组与 bundle，使「W1 已实现」对每一行含义一致
  （`node scripts/plugin-catalog.mjs --check`）。
- **差分门禁保持全绿。** R1 差分（`cmd/r1diff`）钉住客户端可见事件流；改变默认装配的插件须
  证明差异在白名单内（`docs/LOADOUT.md` §8）。

### 负控（必需清单）

| 控制项 | 注入方式 | 必需结果 | 断言位置 |
| --- | --- | --- | --- |
| 清单非法 | 删字段 / 破坏 id 文法 | 安装被拒，字段级原因，目录不被采用 | `internal/pluginpkg/manifest_test.go` |
| 版本门 | `engines.keel` 排除宿主 | 安装**与**启用被拒，明示区间 | `internal/harnessext/plugin_manager_test.go` |
| 依赖缺失 | `requires: [A]` 缺席时启用 B | 状态 `disabled`（装配内为 `PENDING`），具名原因，无半装载 | `internal/harnessext/plugin_manager_test.go` |
| 工具重名 | 两个启用插件声明同名工具 | 第二个插件 `FAILED`，第一个不受影响 | `internal/harnessext/plugin_manager_test.go` |
| sidecar 崩溃 | 运行中杀掉 sidecar 进程 | 其工具调用明确失败、状态 `failed`，其他插件与 Harness 不受影响，重启用可恢复 | `internal/harnessext/plugin_sidecar_test.go` |
| 权限拒绝 | 调用未声明工具 / 读取未声明密钥 | 调用明确失败**且**存在审计行 | `internal/harnessext/plugin_permissions_test.go` |
| 审批被拒 / 超时 | 答 `deny`（或永不作答） | 插件不生效；审计记录拒绝；超时 fail-closed | `internal/harnessapi/plugin_approval_test.go` |
| 创造模式失败 | 令生成/校验失败 | 无半装状态，错误原样返回，临时产物被清理 | `internal/harnessext/creation_test.go` |
| 宿主 `SIGKILL` 且 sidecar 存活 | kill -9 宿主 | 子进程按协议义务因 stdin EOF 退出；sweep 缺口是**已登记**的已知限制，须断言其存在而不是隐藏 | `docs/DSH-REFERENCE.md` §4 |

sidecar 各行的机制已被 `scripts/external-gateway-smoke.sh`（15 步，含孙进程存活/回收断言）与
`cmd/keel-dsh-ref` 的故障开关（`-crash-on-send`、`-ignore-cancel`、`-spawn-child`，
`docs/DSH-REFERENCE.md` §2）演练——插件 sidecar 沿用同一注入风格。

## 3. UI 测试

对 `webview` 插件：

1. 包被托管且页面从 `/plugins/<id>/` 加载，且只对已启用插件开放
   （`cmd/keel/plugins_ui.go:17`）；
2. 页面经桥读取自身状态并调用自身工具
   （`internal/harnessapi/plugin_bridge_test.go`）；
3. 负例成立：非桥调用、其他插件的工具、贡献未声明的动词都以带类型的错误被拒
   （`internal/harnessapi/plugin_bridge_test.go`、`plugin_bridge_capability_test.go`）；
4. TUI 断言：插件行打开 URL，且 TUI 中**不**渲染任何 HTML/WebView
   （`web/src/screens/plugins.tsx:13`）。

## 4. 运行套件

```bash
# 单测 + 集成（-race 集成包需要真实 PostgreSQL）
go test ./internal/pluginpkg/… ./internal/harnessext/… ./internal/harnessapi/…

# 插件 catalog 门禁（schema、引用、COVERAGE §H 新鲜度）
node scripts/plugin-catalog.mjs --check

# 扩展 E2E 与全量门禁
scripts/acceptance-all.sh

# 外部网关冒烟（真实 runtime + 参考 sidecar，15 步）
scripts/external-gateway-smoke.sh

# 客户端投影套件（插件 UI 消费同一投影）
cd packages/chat-surface && bun test

# 站点侧检查：双语对等、引用、死链
cd site && pnpm build
```

插件包应自带 `make test` 入口，运行第 1、2 层及其自身负控；conformance 与 UI 检查由宿主的
验收门禁执行，而非仅由包作者自行运行。
