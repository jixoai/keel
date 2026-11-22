# keel.plugin-market — 扩展面

`keel.plugin-market` 是一个 `kind: sidecar`、`layer: provider` 插件。它提供两个模型工具
`search` / `info`（宿主发布名 `keel_plugin-market_search` / `..._info`），在随包分发的静态
`catalog.json` 上检索。它不声明槽位、不投 UI 贡献、不订阅 hook、没有环境变量配置。

## 我能被怎么扩展

| 扩展点 | 形式 | 说明 |
|---|---|---|
| 索引数据 | `catalog.json`（随包） | 唯一机读真源；schema 与对账规则见 `docs/PLUGIN-CATALOG.md`。 |
| 索引端口 | `providers[].port: plugin.market.index` | 未来可换托管市场实现，工具名与动作不变。 |
| 配置 | 无 | 不读环境变量，不联网。 |

槽位与贡献：

```json
"slots": [],
"contributes": []
```

## 调用示例

```json
{ "query": "memory", "limit": 4 }
{ "id": "dsh.web" }
```

## 边界与限制

- 权限：`permissions.fs: ["read:plugin-package"]`，只读本包 `catalog.json`；无 `net`、无 `secrets`。
- 结果上限 100；未命中返回 `isError: true`，不是空成功。
- catalog 是静态快照（`generatedAt` 记录日期）；市场托管、在线刷新、一键安装不属于本插件。
- 数据变更只经 `scripts/plugin-catalog.mjs --write` 与对账门，禁止手工维护第二份真源。
