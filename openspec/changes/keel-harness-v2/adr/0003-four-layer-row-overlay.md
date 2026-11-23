# ADR 0003 — 四层装配采用 row id overlay，而不是启动分支

- 状态：Accepted（W0.5，2026-11-23）
- 决策者：keel-harness-v2 契约冻结 + W0.5 实现
- 影响面：`internal/assembly`、`internal/loadout`、`internal/servicehost` 的服务选择、
  `docs/ASSEMBLY.md`、未来 W8/W9 的 config.toml 文件与 seam provider

## 背景

`docs/PLUGIN-EXTENSIBILITY.md` §6 冻结的装配模型是四层叠加：base bundle → mode bundle
（serve/headless/tui/sdk）→ preset → 用户 `config.toml`；按 row id 覆盖、last write wins，
激活由服务可用性驱动。冻结时仓库只有 `internal/servicegraph`（服务图）与
`internal/loadout`（单一默认蓝图），没有 bundle/preset/config.toml 概念，也没有 row id。
W0.5-3 的任务是把这套语义落成可实现、可测试的契约，而不是继续用启动代码里的 if/else
模拟。

同时，DSH 的参考实现提供的是 Cordis/typert 运行时。本项目的红线是**单一插件运行时**：
等价语义必须由 `agentext` + `servicegraph` 承担，不得引入第二套 DI/插件框架。

## 决策

### 1. 稳定 row id + 整行覆盖

- row id 为 `<plugin-id>#<row>`（例：`keel.shell#bash-local`），格式与插件 id 规则同源。
- 覆盖只按 row id 命中，last write wins；一次覆盖替换整行 `impl` + `params`，params
  **不按键合并**。
- 只有 base 能新增 row；mode/preset/config 命中不存在的 row 一律拒绝（`row-unknown`）。
- effective rows 保持 base 声明顺序；overlay 内 row 必须按 effective 顺序出现，禁止重排
  （`row-order`）。
- overlay 不能改 `layer` 或 `requires`：覆盖改变实现，不改变架构位置与依赖形状。
- `absent=true` 把行留在 effective 集合中（可查询、可审计），但不激活；依赖者进入
  `Blocked`，不半接线启动。
- base 允许无 row id 的匿名行：原样保留、永不参与覆盖；overlay 中的无 id 行 fail-closed。

### 2. 纯模型 + 有界适配层

- `internal/assembly` 只做内存模型：无 goroutine/锁/时钟/全局状态，只 import
  `internal/pluginpkg` 的 `Layer` 词汇；不 import harness/product/servicehost。
- `internal/loadout` 把默认服务蓝图映射为 base、把 `host.TenantConfig` 映射为 config
  stage，并从 active rows 推导服务集合。`servicegraph` 仍是唯一的加载/激活器；没有第二个
  DI/运行时。
- 若适配层看到 `Blocked`，loadout 构造失败，而不是让依赖缺失的服务降级激活。

### 3. 可用性驱动的激活顺序

- `requires` 只在 base 声明；`Build` 校验未知依赖、自依赖、重复依赖与环。
- 激活顺序由 Kahn 拓扑排序推导，平局按 effective 位置裁决；没有任何手写启动顺序表。
- 依赖 absent/被阻塞的行进入 `Blocked`（`absent` / `dependency`），可供 UI/诊断展示。

### 4. 配置是不可变版本 + 账本锚点

- `config.toml` 只承载覆盖行；严格子集 + 字段级稳定错误码。
- 内容身份是 `sha256:<hex>`（规范化 rows/params 后计算），与空白、注释、键序、行序无关。
- `ConfigVersion` = `{Revision, Hash, AnchorEventSeq}`；每次轮转 revision 前进，锚点必须
  严格大于当前锚点。`Pinned` 是深拷贝，一次 run 固定一个版本；轮转只影响下一次 `Pin()`。
- 文件级 `config.toml.new → fsync → rename()` 是宿主职责，留给 W8/W9；本 ADR 只冻结版本/
  锚点语义，不把文件 IO 塞进纯模型。

### 5. seam 能力矩阵 + 启动前类型化拒绝

- 五个 seam（fs/shell/subprocess/exec-surface/terminal）各有闭集 capability；provider
  只能声明所属 seam 的能力。
- `Registry.Matrix()` 对每个 provider 输出 seam 的完整 capability 行（含 false 位）。
- `Prepare(docs, registry, requirements)` 在返回 assembly 前检查 requirements：
  - 词汇/声明错误 → `*InvalidCapabilityError`，`Code() == "invalid_capability"`；
  - 能力缺口/未注册 provider → `*UnsupportedCapabilityError`，
    `Code() == "unsupported_capability"`；
  - 不静默降级，不等到第一次工具调用才失败。

## 被否决的方案

| 方案 | 否决理由 |
|---|---|
| 在 `cmd/keel` / `servicehost` 启动代码里按 mode 写 if/else | 覆盖规则散落，无法按 row 审计；新增 preset/config 会继续膨胀启动分支 |
| 手写插件启动顺序 | 与“激活由服务可用性驱动”冲突；顺序成为第二份依赖真值 |
| params 按键合并 | 覆盖结果依赖覆盖层数，无法从最终文档解释；last write wins 应作用于整行 |
| overlay 可以新增 row | mode/preset/config 变成第二份 base，`row-unknown` 失去意义；新增必须回 base |
| 引入 Cordis/typert 或第二套 DI | 违反单一插件运行时红线；现有 `servicegraph` 已表达依赖与激活 |
| capability 不足时先启动、运行期再失败 | 违反 spec「启动前拒绝」；会产生半接线会话与难以归因的工具错误 |
| 所有 run 共享一个可变配置指针 | 轮转会改变进行中 run；违反“一次 run 固定一个版本” |

## 不照搬清单（W0.5 冻结）

| DSH 做法 | Keel 的决定与理由 |
|---|---|
| Cordis DI + typert 构建期类型图 | 不引入。单一插件运行时红线；依赖/激活由 `servicegraph`，工具/扩展由 `agentext` 表达 |
| 一能力一工具（`tool-bash`/`tool-fs`/`tool-web`…） | 不照搬。工具面恒定，插件只挂聚合工具的既有 action（`provides.backends`），插件粒度 ≠ 工具粒度 |
| 70 个客户端 UI 包 | 不照搬。只有一套 NSH 客户端；槽位由插件声明，面板是贡献物 |
| sandbox 族 | 不做。强隔离走 Docker；`sandbox` 只作为可选服务位，未实现时不伪造工厂 |
| DSH ssh 执行面 | 不做。远端执行走自研 seam 契约（fs/shell/subprocess/exec-surface/terminal），不复制 DSH 方言 |

## 后果

- 生产服务选择已改由 `loadout.AssembledServices`（base → serve → config）决定；
  `FromTenantConfig` 不再直接读 `EnabledServices` 的布尔集合。现有服务图与工厂机制不变。
- `docs/ASSEMBLY.md` 成为字段/错误码/capability 名的单源；文档与代码不一致按契约缺陷处理。
- W8/W9 仍需完成：生产 `config.toml` 文件读取与原子 rename、headless/tui/sdk bundle、
  local/remote/SSH seam provider 的实际注册与启动检查。这些在文档与证据中明确标注，
  本 ADR 不把它们算作已交付。
- 新增装配行必须先在 base 声明并分配稳定 row id；新增 mode/preset/config 只能覆盖它。

## 证据

- `openspec/changes/keel-harness-v2/evidence/w05-assembly/`：针对性测试输出与接线说明。
- `go test ./internal/assembly ./internal/loadout ./internal/servicehost ./internal/architecture`
