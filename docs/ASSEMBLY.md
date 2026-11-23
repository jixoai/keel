# Assembly — 四层装配、row id overlay 与能力 seam

> 状态：W0.5 交付（2026-11-23）。实现：`internal/assembly`（纯模型）+
> `internal/loadout`（生产适配）。契约来源：
> `docs/PLUGIN-EXTENSIBILITY.md` §6、`openspec/changes/keel-harness-v2/specs/plugin-architecture/spec.md`
> （Requirement: 装配模型 / 能力 seam 可替换）、`design.md` §10、ADR
> `openspec/changes/keel-harness-v2/adr/0003-four-layer-row-overlay.md`。
>
> 本文件是**机读字段与人读规则的同源说明**：字段名、错误码、capability 名就是代码里的
> 标识符，不允许在文档里另起别名。

## 1. 范围

`internal/assembly` 是**纯模型**：没有 goroutine、锁、时钟、全局状态，也不 import
`servicehost` / `servicegraph` / `agent` / 产品层。唯一的仓内依赖是
`internal/pluginpkg`（`Layer` 词汇的冻结属主）。它负责：

- 校验并叠加 base → mode（serve/headless/tui/sdk）→ preset → config.toml；
- 按 row id 解析 effective rows（缺席语义、禁止重排）；
- 由 `requires` 推导激活顺序（服务可用性驱动，不写死顺序）；
- 解析 `config.toml`、计算内容版本哈希、维护不可变版本与账本锚点；
- 校验五个能力 seam 的 provider 能力矩阵，启动前以类型化
  `unsupported_capability` 拒绝不支持的组合。

**不在本切片、不得伪装的**（W8/W9）：

| 项 | 现状 | 归属 |
|---|---|---|
| 从磁盘读取生产 `config.toml` | 生产配置源仍是 `host.TenantConfig`（部署文档/管理端写入） | W8/W9 |
| `config.toml.new` → `fsync` → `rename()` 文件适配 | 轮转的**版本/锚点语义**已实现；原子文件写不在纯模型内 | W8/W9 |
| `headless` / `tui` / `sdk` mode bundle | 仅 `serve` 显式接线；其余请求直接报错 | W8/W9 |
| 真实 local/remote/SSH seam provider 注册与启动检查 | capability 词汇、矩阵、typed rejection 已实现；生产进程尚未注册 provider 集合 | W8/W9 |

## 2. 四层叠加

```text
base bundle     默认装配：内核 seam、记忆卫生、产品件（唯一可新增 row 的层）
   ↓ 按 row id 覆盖，last write wins
mode bundle     serve / headless / tui / sdk
   ↓
preset          工具/提示/技能的行为覆盖
   ↓
config.toml     用户配置；不可变版本 + 账本锚点
```

### 2.1 row id

```text
<plugin-id>#<row>        keel.shell#bash-local
```

- `plugin-id` 与 `docs/PLUGIN-EXTENSIBILITY.md` 一致：`^[a-z][a-z0-9._-]{1,63}$`。
- `row`：`^[a-z0-9][a-z0-9._-]{0,63}$`；恰好一个 `#`。
- row id 是**稳定身份**，不是位置。文档内行序只决定 base 的声明顺序与激活的平局裁决，
  不决定覆盖目标。

### 2.2 覆盖规则

| 规则 | 行为 |
|---|---|
| 命中方式 | 只按 row id 命中；last write wins（mode 覆盖 base，preset 覆盖前两层，config 覆盖前三层） |
| 覆盖粒度 | **整行**替换 `impl` + `params`；params 不按键合并 |
| 新增 | 只有 base 可以新增 row；mode/preset/config 命中不存在的 row → `row-unknown` |
| 顺序 | effective rows 保持 base 声明顺序；overlay 内 row 必须按 effective 顺序出现，否则 `row-order` |
| 结构 | overlay 不能改 `layer`（`row-layer`）、不能改 `requires`（`row-requires`） |
| 缺席 | `absent = true`：该行保留在 effective 集合中（`Absent=true`，无 impl/params），不参与激活；依赖它的行进入 `Blocked`，不会半接线运行 |
| 无 row id | base 允许匿名行：原样保留、**永不参与覆盖**；overlay 中的无 id 行直接 `row-id` 拒绝（fail-closed） |
| 层序 | 文档必须按 base → mode → preset → config 出现；每个 stage 至多一份；mode 的 `Name` ∈ serve/headless/tui/sdk |

示例：`headless` 只把 `keel.ui#shell` 覆盖为 absent，base 的其余 row 原样保留；
preset 关闭 `bash` 只改该会话看到的 `keel.shell#bash-local`，不改写 base/bundle 定义。

### 2.3 激活

`Row.Requires` 是 row id 列表（只在 base 声明）。`Assembly.Activation()` 用
Kahn 拓扑排序推导激活顺序：

- 可用性满足即激活；平局按 effective 位置（声明顺序）稳定裁决；
- 缺失或 absent 的依赖不会让依赖者“降级启动”，而是进入 `Assembly.Blocked()`，
  带 `Reason=absent|dependency` 与缺失 row id；
- 未知 `requires` row 与环在 `Build` 阶段以 `row-requires` / `row-cycle` 拒绝。

`Active()` 返回已激活行；`Effective()` 返回全部行（含 absent）；`Blocked()` 暴露阻塞行。
`servicehost` 的适配器把 `Blocked` 当作 loadout 构造失败，而不是忽略。

## 3. Row / Document 字段

```go
type Document struct {
    Stage Stage   // base | mode | preset | config
    Name  string  // mode 文档必须是 serve/headless/tui/sdk；其他 stage 是人读标签
    Rows  []Row
}

type Row struct {
    ID       RowID             // <plugin-id>#<row>；base 可空（匿名、不可覆盖）
    Layer    Layer             // kernel|harness|provider|ui|service；base 必填
    Impl     string            // 实现 id；present row 必填
    Params   map[string]string // 标量字符串参数；覆盖是整表替换
    Requires []RowID           // 仅 base；可用性依赖
    Absent   bool              // 覆盖为缺席
}
```

`EffectiveRow` 额外带 `Origin`（首次声明 stage）、`Source`（最后写入 stage）、
`Overridden`，用于审计“这一行是谁改的”。

## 4. 校验错误码

`Build` 一次性校验整栈，返回 `*assembly.Error`（`Issues []Issue`）。错误码稳定：

| code | 含义 |
|---|---|
| `missing-base` | 栈中没有 base 文档 |
| `stage-order` | 文档未按 base → mode → preset → config 排列 |
| `stage-duplicate` | 同一 stage 出现多份文档 |
| `stage-unknown` | stage 不在闭集 |
| `mode` | mode 文档的 Name 不是 serve/headless/tui/sdk |
| `row-id` | row id 缺失/格式错误；overlay 无 id 行 |
| `row-duplicate` | 同一文档内 row id 重复 |
| `row-unknown` | overlay 命中 base 未声明的 row |
| `row-order` | overlay 试图重排 |
| `row-layer` | layer 非法，或 overlay 试图改 layer |
| `row-impl` | present row 没有 impl |
| `row-params` | params key 非法 |
| `row-requires` | requires 非法 / 自依赖 / 重复 / overlay 声明 / 指向未声明 row |
| `row-cycle` | requires 成环 |
| `row-base-absent` | base 声明 absent |
| `row-absent-values` | absent 行同时带 impl/params/requires |
| `config-version` | 缺 `version`、类型错、或不是 1 |
| `config-table` | 不支持的 TOML 表/数组表（只允许 `[[rows]]`） |
| `config-key` | 未知顶层/行级 key，或 config 行试图设置 layer/requires |
| `config-value` | 值类型错（如 params 非字符串） |
| `config-duplicate` | key 或 row id 重复 |
| `config-absent-impl` | absent config 行带 impl |
| `config-params-on-absent` | absent config 行带 params |
| `config-empty-row` | 空 `[[rows]]` 或没有 id |
| `config-anchor` | 锚点为负，或轮转锚点未严格前进 |

## 5. config.toml

### 5.1 严格子集

```toml
version = 1

[[rows]]
id = "keel.shell#bash-local"
impl = "ssh"
params.host = "devbox"
params.port = '22'

[[rows]]
id = "keel.ui#shell"
absent = true
```

- 顶层只允许 `version = 1`；只允许 `[[rows]]` 表。
- 行级只允许 `id`、`impl`、`absent`、`params.<key>`。
- `params` 值是字符串（basic `"..."` 或 literal `'...'`）；数组、内联表、多行字符串
  不在子集内，遇到即 `config-value`，不半应用。
- 未知表/键、重复键/行、空行表、没有 id、absent 带值：字段级拒绝并带
  `config.toml:<line>` 路径。

### 5.2 版本哈希

`Config.Hash()` 是规范化内容摘要：跳过空白/注释，params 与 rows 排序后哈希，
输出 `sha256:<64 hex>`。因此：

- 同内容不同写法（键序、行序、注释）哈希相同；
- `impl`/`params`/`absent` 任一变化哈希不同；
- 行序本身不改变 effective 配置，所以不制造新 hash；是否合法由 `Build` 的
  `row-order` 判断。

### 5.3 轮转与 run 固定

```go
config, _ := assembly.ParseConfig(data)
rotation, _ := assembly.NewRotation(config, anchorEventSeq) // revision 1
pinned := rotation.Pin()                                   // 一次 run 固定一个版本
next, _ := rotation.Rotate(config2, nextAnchorEventSeq)    // revision+1
// pinned 仍是旧版本；rotation.Pin() 是新版本
```

- `ConfigVersion{Revision, Hash, AnchorEventSeq}` 是 run 快照要记录的配置身份；
  `Revision` 每次 `Rotate` 前进（即使内容相同——轮转事件本身是版本事实）。
- `AnchorEventSeq` 必须严格大于当前锚点且非负；否则 `config-anchor`。
- `Pinned` 是深拷贝：轮转不会改变进行中 run 的版本。
- 文件级原子替换（写 `config.toml.new` → fsync → `rename()`）由宿主在 W8/W9 实现；
  `Rotation` 只定义版本与锚点语义，不读写文件。

## 6. 能力 seam 与 capability matrix

五个 seam 是闭集：`fs`、`shell`、`subprocess`、`exec-surface`、`terminal`。
每个 seam 的能力也是闭集；provider 只能声明所属 seam 的能力。

| seam | capabilities |
|---|---|
| `fs` | `read`, `write`, `edit`, `delete`, `stat`, `list`, `rename`, `symlink`, `chmod`, `atomic-write` |
| `shell` | `exec`, `env`, `cwd`, `timeout`, `background`, `job-control` |
| `subprocess` | `spawn`, `wait`, `signal`, `kill`, `stdin`, `stdout`, `stderr`, `pty` |
| `exec-surface` | `materialize`, `capture`, `snapshot`, `delete-tracking`, `symlink`, `exec-bit`, `atomic-rename`, `case-sensitive` |
| `terminal` | `attach`, `resize`, `scrollback`, `detach`, `replay` |

```go
registry, _ := assembly.NewRegistry(
    assembly.Provider{Seam: assembly.SeamExecSurface, Impl: "remote",
        Capabilities: []assembly.Capability{assembly.CapExecMaterialize, assembly.CapExecCapture}},
)
_, err := assembly.Prepare(docs, registry, []assembly.Requirement{
    assembly.Need(assembly.SeamExecSurface, "remote", assembly.CapExecSymlink),
})
// err 是 *assembly.UnsupportedCapabilityError，Code() == "unsupported_capability"
```

- `Registry.Matrix()` 输出每个 provider 的完整行：seam 的**所有** capability 都出现，
  `Supported` 标记是否声明。
- `Prepare` 在返回 assembly 之前检查 requirements；不支持的组合不会进入启动流程。
- 类型化错误：
  - `*UnsupportedCapabilityError`（`unsupported_capability`）：provider 缺少能力，
    或 provider 未注册（`UnknownProvider=true`）；
  - `*InvalidCapabilityError`（`invalid_capability`）：未知 seam/capability、
    非法 provider id、重复 provider/capability——词汇错误，任何 provider 都满足不了。
- 不静默降级：没有 fallback 链，也没有“先启动再在第一次工具调用失败”。

## 7. 生产接线（W0.5 实际生效的部分）

`internal/loadout` 是唯一适配层，不新增 DI/运行时：

| 函数 | 作用 |
|---|---|
| `ServiceRowID(service)` | `keel.<service>#service` 稳定 row id |
| `AssemblyBase()` | 把 `DefaultServices()` 的 12 个默认服务映射为 base rows；`Requires` 由服务图的 capability 依赖推导 |
| `AssemblyMode(mode)` | 只接线 `serve`（空覆盖）；headless/tui/sdk 明确报错 W8/W9 |
| `AssemblyConfig(tenant, opts)` | 把 `host.TenantConfig` 映射为 config stage：未启用的服务 → absent row；models 文档派生的 llm 版本 → impl 覆盖 |
| `AssembledServices(tenant, opts)` | `base → serve → config` 叠加后从 active rows 取服务清单；`Blocked` 视为 loadout 失败 |
| `FromTenantConfig` | 用 `AssembledServices` 决定服务集合，再交给 `servicegraph` 装配 |

因此生产路径上的“哪些服务存在”已由四层 overlay 决定，而不是第二套 if/else；
`servicegraph` 仍是唯一的加载/激活器。`config.toml` 文件源、其余 mode bundle 和真实
seam provider 注册仍是 W8/W9，本切片不伪造。

## 8. 校验命令

```bash
go test ./internal/assembly
go test ./internal/loadout
go test ./internal/servicehost
go test ./internal/architecture
```
