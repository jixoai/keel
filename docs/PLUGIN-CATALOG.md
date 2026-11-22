# Plugin Catalog — 唯一机读真源（keel-harness-v2 / W0.5）

> 状态：**frozen 2026-11-22**。`plugins/keel.plugin-market/catalog.json` 是官方/社区插件
> 覆盖矩阵的**唯一机读真源**；`plugins/COVERAGE.md` 是它的可读投影 + 逐行对账对象。
> Schema：`plugins/keel.plugin-market/catalog.schema.json`；校验器：`scripts/plugin-catalog.mjs`。
> 规格：`openspec/changes/keel-harness-v2/specs/plugin-system/spec.md`
> （Requirement: 官方插件套件覆盖矩阵）。

## 1. 为什么只有一份

历史上有三份近似清单：`catalog.json`、`COVERAGE.md`、`plugin-inventory.md`。三者 id 集合
不相等，且 `COVERAGE.md` 手工维护会漂移。W0.5 后的口径：

| 文件 | 角色 | 门禁 |
|---|---|---|
| `catalog.json` | 唯一机读真源（id/状态/波次/dshGroups/覆盖目标） | `--check` 必过 |
| `COVERAGE.md` §A/§B/§C | 人读能力矩阵；每行映射到 `keelPluginId` 或 absent/merged | `--check` 逐行对账 |
| `COVERAGE.md` §H | 由 catalog **生成**的机读视图 | `--write` 生成，`--check` 比对 |
| `plugin-inventory.md` | 设计期附录，**不作门**、不作归档后规范引用 | 不参与门禁 |

## 2. 顶层形状

```json
{
  "schema": "keel.plugin-market.catalog/1",
  "generatedAt": "2026-11-22",
  "sources": [ { "kind": "dsh-official", "repo": "...", "ref": "..." } ],
  "entries": [ /* 见 §3 */ ],
  "retiredIds": [ /* 见 §4 */ ]
}
```

- `schema` 固定 `keel.plugin-market.catalog/1`（sidecar 启动时也校验它）。
- `generatedAt` 是 `YYYY-MM-DD`；它记录本次数据冻结日期，不参与排序。
- `sources` 记录 DSH 基线（commit/日期/统计口径）与抓取方法；条目本身不重复这些事实。

## 3. 条目字段

| 字段 | 类型 | 规则 |
|---|---|---|
| `id` | string | 全局唯一；官方组 `dsh.<group>`、社区 `community.<owner>.<repo>`、keel 插件 `keel.<name>` |
| `name` / `summary` | string | 非空；`summary` 是一句话能力面 |
| `capabilities` | string[] | 非空；检索用的能力词 |
| `source` | object | `kind ∈ dsh-official｜dsh-community｜keel-official`；社区条目带 `repo/url/stars/starsObservedAt/discovery` |
| `dshPackages` | string[] | DSH 组的真实包名（已核对）；未知时 `[]` |
| `wave` | enum | `W0.5｜W1｜W2｜W3｜never`（闭集） |
| `dshGroups` | string[] | 该条目覆盖/吸收的 DSH 组 id；`dsh.*` 条目必须包含自身；社区条目为 `[]`；keel 插件列出它吸收的组 |
| `coverage.covered` | bool | 是否有 keel 覆盖 |
| `coverage.status` | enum | `delivered｜partial｜planned｜registered｜merged｜absent`（闭集） |
| `coverage.keelPluginId` | string\|null | 覆盖它的 keel 插件 id；必须能在 entries / 其他 keelPluginId / retiredIds 中解析 |
| `coverage.mergedInto` | string? | 并入非插件实体时使用（目前只有 `group`） |
| `coverage.tools` | string[] | 已发布工具名（delivered 必填非空） |
| `coverage.note` | string? | merged/absent 必填，写明目的地或理由 |

状态语义：

- `delivered`：已交付可安装（工具非空）。
- `partial`：部分能力已交付，剩余部分在 note 里写明。
- `planned`：已登记，按 `wave` 推进。
- `registered`：已登记但长期/受限，不承诺当前 change。
- `merged`：能力并入另一个插件/实体；必须给 `keelPluginId` 或 `mergedInto`。
- `absent`：明确不做；必须给理由（note ≥ 8 字符）。

波次语义：`W0.5` 契约切片；`W1`–`W3` 见 `tasks.md` 的 W 分段；`never` 明确不排期。

## 4. `retiredIds`

已合并/改名的 keel 插件 id 不删行，进 `retiredIds`：

```json
{ "id": "keel.guard", "status": "merged", "into": "keel.supervisor", "reason": "重复工具提醒/超时策略并入护栏" }
```

规则：`id` 唯一、`status ∈ merged｜absent`、`merged` 必须有 `into`、`reason` 必填。
`COVERAGE.md` 的旧 id 行通过 `retiredIds` 解析，因此不会因为改名而出现悬空映射。

## 5. COVERAGE.md 的对账规则

`--check` 解析 §A/§B/§C 的表格行：

1. 状态列必须出现闭集状态；`absent` 必须在单元格内给出理由。
2. 波次列必须出现 `W0.5｜W1｜W2｜W3｜never`（absent 行可写 `—`）。
3. 映射列至少有一个反引号 id 能解析到 catalog 的 entry id / `keelPluginId` / `retiredIds`；
   §B 的 bundle 行按 spec 统一映射到 `keel.bundles`。
4. 加粗且声明 `delivered` 的 id，其 catalog 状态必须是 `delivered｜partial`。
5. catalog 的每个 `dsh.*` 组必须在 §A 有对应行（允许 `identity/telemetry` 这种合写）。
6. 已安装的官方包（`plugins/*/plugin.yaml`）必须有 catalog 条目且状态为 `delivered｜partial`；
   必须随包提供 `extensibility.json` + `EXTENSIBILITY.md`。

`--write` 只重写 §H（生成视图），不猜改人读表格；人读表格的漂移由 `--check` 报错。

## 6. 命令与 CI

```bash
node scripts/plugin-catalog.mjs --check   # CI 模式：schema/enum/引用/映射/§H 全查
node scripts/plugin-catalog.mjs --write   # 改完 catalog 后重新生成 COVERAGE §H
```

CI 的 `test` job 在 `go build` 后执行 `--check`；本地改动 catalog 后忘记 `--write` 会直接失败。
`keel.plugin-market` 的 `search`/`info` 工具与 catalog 同源：加条目只改 catalog（+§H 生成），
不改 sidecar 代码。

## 7. 新增条目的检查单

1. 先判断是官方组、社区项目还是 keel 插件；`id` 用对应前缀。
2. 填 `wave` 与 `dshGroups`（keel 插件写它吸收的 DSH 组；社区条目写 `[]`）。
3. 状态与工具名必须和实际交付一致；`delivered` 工具非空。
4. 合并/改名走 `retiredIds`，不直接删 id。
5. 跑 `--write` 再跑 `--check`；CI 绿之前不提交。
