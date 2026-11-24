---
title: 装配
description: loadout 背后的四层装配——base → mode → preset → config.toml、row id、覆盖规则、严格子集的 config.toml 与能力矩阵。
---

# 装配

loadout 是按 run pin 的不可变快照（见[架构 §4](/zh/core/architecture)）。本页讲输入侧：effective 行集
如何由四层叠加而成，以及生产里哪些接线了、哪些没有。

> **状态。** 纯模型（`internal/assembly`）、磁盘加载器（`internal/configfile`）与 `serve` 接线已交付。
> 三块明确未接线：`headless`/`tui`/`sdk` mode bundle、真实的 local/remote/SSH seam provider、以及
> `config.toml` 的 row 进入生产服务图（`docs/ASSEMBLY.md:3`、`docs/ASSEMBLY.md:35`、
> `docs/ASSEMBLY.md:36`）。

## 1. 四层

```text
base bundle      默认：内核 seam、记忆卫生、产品服务
   ↓ row id      （唯一允许新增 row 的层）
mode bundle      serve / headless / tui / sdk
   ↓
preset           工具/提示/技能的行为覆盖
   ↓
config.toml      用户文件：不可变版本 + 账本锚点
```

`base` 是唯一可以新增 row 的层，其余三层只是覆盖（`docs/ASSEMBLY.md:44`、`docs/ASSEMBLY.md:70`）。
一个栈必须恰好有一份 `base` 文档；`mode`、`preset`、`config` 可选，出现时必须按该顺序、每个 stage 至多
一份，且 `mode` 的 `Name` ∈ `serve`/`headless`/`tui`/`sdk`（`docs/ASSEMBLY.md:75`）。

## 2. row id 是身份，不是位置

```text
<plugin-id>#<row>        keel.shell#bash-local
```

id 是稳定身份：`plugin-id` 遵循插件命名空间，row 部分恰好一个 `#`（`docs/ASSEMBLY.md:53`）。文档内的行序
只决定 base 的声明顺序与激活平局裁决，从不决定覆盖命中谁（`docs/ASSEMBLY.md:61`）。

| 规则 | 行为 |
|---|---|
| 命中 | 只按 row id；last write wins（mode 覆盖 base，preset 覆盖前两层，config 覆盖前三层）（`docs/ASSEMBLY.md:68`） |
| 粒度 | 整行替换——`impl` + `params`；params 绝不按键合并 |
| 新增 | 只有 `base` 可以新增 row；覆盖基里没有的 row → `row-unknown`（`docs/ASSEMBLY.md:70`） |
| 顺序 | effective rows 保持 base 顺序；覆盖行必须按该顺序出现，否则 `row-order` |
| 结构 | 覆盖不能改 `layer`（`row-layer`）或 `requires`（`row-requires`）（`docs/ASSEMBLY.md:72`） |
| 缺席 | `absent = true` 保留该行但移出激活，依赖它的行进入 `Blocked` |
| 匿名 | base 允许无 id 行（原样保留、永不参与覆盖）；覆盖里无 id 的行直接以 `row-id` 拒绝（`docs/ASSEMBLY.md:74`） |

## 3. 激活

`requires` 只在 base 声明。`Assembly.Activation()` 用 Kahn 拓扑排序推导顺序：能力满足即激活，平局按
effective 位置裁决；依赖缺失或 absent 不会让依赖者「降级启动」，而是进入 `Assembly.Blocked()`，带
`Reason=absent|dependency` 与缺失 row id（`docs/ASSEMBLY.md:80`）。服务适配层把 `Blocked` 当作 loadout
构造失败，而不是忽略（`docs/ASSEMBLY.md:91`）。

## 4. config.toml 是严格子集

```toml
version = 1

[filesystem.capture]
mode = "checkpoint"        # per-tool-call | checkpoint | manual

[[rows]]
id = "keel.shell#bash-local"
impl = "ssh"
params.host = "devbox"
params.port = '22'

[[rows]]
id = "keel.ui#shell"
absent = true
```

顶层只允许 `version = 1`、`[[rows]]` 与 `[filesystem.capture]`；行内只允许 `id`、`impl`、`absent`、
`params.<key>`；值必须是字符串（`"..."` 或 `'...'`），数组、内联表、多行字符串一律以
`config.toml:<line>` 路径字段级拒绝，不做半应用（`docs/ASSEMBLY.md:151`）。校验对整栈一次完成，错误码
稳定：`missing-base`、`stage-order`、`stage-duplicate`、`row-unknown`、`row-order`、`row-cycle`、
`config-value`、`config-anchor` 等（`docs/ASSEMBLY.md:117`）。

## 5. 版本与账本锚点

`Config.Hash()` 是规范化内容摘要（`sha256:<64 hex>`）：注释、空白、键序与行序都不改变它；`impl`、
`params`、`absent`、`filesystem.capture.mode` 任一变化都会改变它（`docs/ASSEMBLY.md:178`）。轮转即使
内容相同也推进 `revision`，锚点必须严格前进（`config-anchor`）；`Pin()` 是深拷贝，因此轮转永远不改动
进行中的 run（`docs/ASSEMBLY.md:186`）。

唯一的磁盘适配器是 `internal/configfile.Loader`。`keel serve --config-file` / `KEEL_CONFIG_FILE`
（默认 `$KEEL_HOME/config.toml`）只在启动时加载：文件缺失 = 空配置，不可读或非法 = 启动失败；轮转顺序是
解析 → 写 `.new` → `fsync(file)` → `rename` → `fsync(dir)`（`docs/ASSEMBLY.md:203`）。`Rotate` 与
`Reload` 目前是库 API——没有生产调用者，也没有 HTTP 入口（`docs/ASSEMBLY.md:228`）。当前生效版本在
`GET /v1/capabilities` 的 `configFile` 块可见（hash、revision、anchor、capture mode；从不含本机路径）
（`internal/harnessapi/api.go:525`）。

## 6. 能力 seam

五个 seam 是闭集——`fs`、`shell`、`subprocess`、`exec-surface`、`terminal`——每个 seam 的能力也是闭集，
provider 只能声明所属 seam 的能力（`docs/ASSEMBLY.md:261`）。`Prepare` 在返回 assembly 之前按注册表检查
requirements；不支持的组合以 `unsupported_capability` / `invalid_capability` 类型化拒绝，没有 fallback
链（`docs/ASSEMBLY.md:291`）。

## 7. 生产今天接线了什么

| 函数 | 效果 | 出处 |
|---|---|---|
| `ServiceRowID` | `keel.<service>#service`——服务的稳定 row id | `internal/loadout/assembly.go:38` |
| `AssemblyBase` | 把 `DefaultServices()` 映射为 base rows；`Requires` 由服务图推导 | `docs/ASSEMBLY.md:300` |
| `AssemblyMode` | 只接线 `serve`；`headless`/`tui`/`sdk` 直接报错 | `docs/ASSEMBLY.md:302` |
| `AssemblyConfig` | 把租户文档映射到 `config` stage（未启用的服务 → absent row） | `docs/ASSEMBLY.md:303` |
| `AssembledServices` | `base → serve → config` 后从 active rows 取服务清单；`Blocked` 视为 loadout 失败 | `docs/ASSEMBLY.md:304` |
| `configfile.Open` / `OverlayTenant` | 启动加载；把文件 setting 叠加到启动期租户文档 | `docs/ASSEMBLY.md:306` |

也就是说「哪些服务存在」由覆盖决定，而不是第二套 if/else——`servicegraph` 仍是唯一的加载/激活器。
尚未接线的是：非 service 的 `config.toml` row 不进入生产服务图、没有生产轮转调用者，其余 mode bundle
与真实 seam provider 注册仍是未完成工作（`docs/ASSEMBLY.md:315`）。

## 8. 校验

```bash
go test ./internal/assembly
go test ./internal/configfile
go test ./internal/loadout
go test ./internal/servicehost
go test ./internal/architecture
go test ./cmd/keel
```

（`docs/ASSEMBLY.md:320`）
