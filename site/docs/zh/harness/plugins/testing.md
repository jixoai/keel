# 插件测试

> 本页给出插件作者与插件系统自身的**规划**测试要求，源自
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`。下文的测试*纪律*与每个
> 运行器都是本仓库**已实现**的约定，插件套件必须遵循。

## 1. 规则

> 插件规格中的每个 Scenario 都必须有测试；**负控是强制的**，不是可选项。

这是服务图 change 给自己定下的同一规则：每个 spec scenario 有测试，且只有快乐路径、没有断言
失败模式的验收不算通过（`docs/SERVICE-GRAPH.md` §6）。对插件而言，「装载成功了」是最无趣的
事实；「停用后零残留、被拒时 fail-closed」才是有趣的那个。

## 2. 四层测试

| 层 | 回答的问题 | 运行方式 | 既有先例 |
| --- | --- | --- | --- |
| **单测** | 纯逻辑是否正确？清单解析、版本区间门、切点 | `go test ./internal/…` | `internal/agentext/agentext_test.go` |
| **集成** | 插件在真实宿主中是否如愿？启用 → 工具调用 → hook → 触发器 → 停用 | 对真实 PG 跑 `go test -race` | `internal/agentext/e2e_test.go` |
| **conformance** | 跨实现/版本是否守住契约？ | conformance 套件 | `internal/loop/conformance_suite_test.go`、`docs/PI-CONFORMANCE.md` |
| **负控** | 每个失败模式是否以*正确方式*失败？ | 对错误/状态显式断言 | 网关冒烟 + fail-closed 测试 |

### 单测

- 清单：缺 `id`/`version`/`kind`、id 不合文法、semver 非法、未知 `kind`、`engines.keel`
  超区间——每种都必须以**字段级原因**失败，且插件目录不被采用（`spec: 清单非法即拒绝`）。
- `provides.hooks` 中不在闭集里的条目必须在注册时被拒，绝不静默忽略
  （`internal/agentext/hooks.go:249`）。
- 工具重名检测与顺序无关：谁第二个被装配，谁 fail-closed
  （`internal/agentext/plugin.go:236`、`:454`）。

### 集成

已实现的扩展 E2E 就是模板：*插件注册 → 触发器 fire → hook 投递 → 子代理派生 → 取消在线
attempt*（`internal/agentext/e2e_test.go`，摘要见 `docs/HARNESS.md` §5）。插件套件的集成测试
还必须证明停用路径：停用一个提供工具 `x` 与 webhook 触发器的插件后，工具从模型工具集消失、
触发器不再触发、清单显示 `disabled` 且无错误残留（`spec: 停用即撤销`）。

### Conformance

- **loop conformance 保持全绿。** 插件不能改变 loop 契约；PI conformance 套件是门禁
  （`internal/agentloop/conformance_suite_test.go`）。
- **同一套件向插件作者开放。** 官方插件套件（规划，`plugins/COVERAGE.md`）对每个插件跑同一批
  场景，使「W1 已实现」对矩阵每一行含义一致。
- **差分门禁保持全绿。** R1 差分（`cmd/r1diff`）钉住客户端可见事件流；改变默认装配的插件须
  证明差异在白名单内（`docs/LOADOUT.md` §8）。

### 负控（必需清单）

| 控制项 | 注入方式 | 必需结果 |
| --- | --- | --- |
| 清单非法 | 删字段 / 破坏 id 文法 | 安装被拒，字段级原因，目录不被采用 |
| 版本门 | `engines.keel` 排除宿主 | 安装**与**启用被拒，明示区间 |
| 依赖缺失 | `requires: [A]` 缺席时启用 B | 状态 `disabled`，具名原因，无半装载 |
| 工具重名 | 两个启用插件声明同名工具 | 第二个插件 `FAILED`，第一个不受影响 |
| sidecar 崩溃 | 运行中杀掉 sidecar 进程 | 其工具调用明确失败、状态 `failed`，其他插件与 Harness 不受影响，重启用可恢复（`spec: sidecar 崩溃隔离`） |
| 权限拒绝 | 调用未声明工具 / 读取未声明密钥 | 调用明确失败**且**存在审计行（`spec: 越权被拒`） |
| 审批被拒 / 超时 | 答 `deny`（或永不作答） | 插件不生效；审计记录拒绝；超时 fail-closed |
| 创造模式失败 | 令生成/校验失败 | 无半装状态，错误原样返回，临时产物被清理（`spec: 生成失败可回滚`） |
| 宿主 `SIGKILL` 且 sidecar 存活 | kill -9 宿主 | 子进程按协议义务因 stdin EOF 退出；sweep 缺口是**已登记**的已知限制，须断言其存在而不是隐藏（`docs/DSH-REFERENCE.md` §4） |

sidecar 各行的机制已被 `scripts/external-gateway-smoke.sh`（15 步，含孙进程存活/回收断言）与
`cmd/keel-dsh-ref` 的故障开关（`-crash-on-send`、`-ignore-cancel`、`-spawn-child`，
`docs/DSH-REFERENCE.md` §2）演练——插件 sidecar 预期沿用同一注入风格。

## 3. UI 测试

对 `webview` 插件（规划）：

1. 托管该包并断言页面确实从 `/plugins/<id>/` 加载；
2. 断言页面能经桥读取自身状态并调用自身工具（`spec: 插件 UI 可用`）；
3. 断言负例：非桥调用（访问宿主密钥、访问其他插件工具）失败；
4. TUI 断言：插件行打开 URL，且 TUI 中**不**渲染任何 HTML/WebView（`spec: TUI 行为`）。

## 4. 运行套件

```bash
# 单测 + 集成（-race 集成包需要真实 PostgreSQL）
go test ./internal/agentext/… ./internal/servicegraph/… ./internal/loadout/…

# 扩展 E2E 与全量门禁
scripts/acceptance-all.sh

# 外部网关冒烟（真实 runtime + 参考 sidecar，15 步）
scripts/external-gateway-smoke.sh

# 客户端投影套件（插件 UI 消费同一投影）
cd packages/chat-surface && bun test

# 站点侧检查：双语对等、引用、死链
cd site && pnpm build   # 构建期链路与页面检查
```

插件包应自带 `make test` 入口，运行第 1、2 层及其自身负控；conformance 与 UI 检查由宿主的
验收门禁执行，而非仅由包作者自行运行。
