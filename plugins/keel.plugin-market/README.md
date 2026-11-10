# keel.plugin-market

官方插件市场索引客户端（`kind: sidecar`）。侧车是一个零依赖 Node 进程，按 Keel 外部
provider 协议（`internal/gateway`，stdio 行 JSON-RPC 2.0）提供**两个**模型工具：在
**随包分发的静态索引** `catalog.json` 上做检索与详情查询。它是「对照 DSH 提供官方插件」
的可查询面：每条索引都带 DSH 侧真实包名、来源链接、keel 覆盖状态与覆盖它的 keel 插件 id。

## 装了这个插件，模型能用什么

| 侧车工具名（协议面） | 宿主发布名（模型/桥调用） | 作用 |
|---|---|---|
| `search` | `keel_plugin-market_search` | 按关键词/能力面/覆盖状态/来源检索索引 |
| `info` | `keel_plugin-market_info` | 按条目 id / keel 插件 id / 工具名 / DSH 包名查一条 |

### `search`

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `query` | string | 否 | 关键词，覆盖 id/名称/一句话/能力面/DSH 包名/来源仓 |
| `capability` | string[] | 否 | 只要能力面**包含全部**给定词的条目 |
| `status` | string[] | 否 | `delivered` / `partial` / `planned` / `registered` / `absent` |
| `source` | string[] | 否 | `dsh-official` / `dsh-community` / `keel-official` |
| `limit` | integer | 否 | 1–100，默认 10 |

返回 `{query, filters, tokens, totalMatches, count, entries[], catalog}`；`entries` 按分数、
覆盖状态（delivered 优先）、id 排序，每条带 `score`。`catalog` 是索引自身的元信息
（`generatedAt`、来源列表、条目总数、按状态/来源的分布）。

### `info`

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是 | 条目 id（`dsh.context`）、keel 插件 id（`keel.context-memory`）、工具名或 DSH 包名（`web-search-deepseek`） |

精确匹配（大小写不敏感），返回 `{query, found, count, matches:[{entry, matchedBy}]}`；
没找到时 `found: false` 且工具结果 `isError: true`（结构化错误，不是空成功）。

## 索引 `catalog.json`（随包分发，离线可审计）

- 86 条：53 条 DSH 官方能力组（一条对应 `packages/<group>/`）+ 30 条社区项目 +
  3 条本轮交付的 keel 官方插件。
- 每条字段：`id`、`name`、`summary`、`capabilities[]`、`source{kind,repo,url,...}`、
  `dshPackages[]`（**真实包名**，从 DSH 仓库树核对）、`coverage{covered,status,keelPluginId,tools,note}`。
- 顶层 `sources` 记录基线：仓库 `deepseek-ai/deepseek-harness`、commit `fcc0ad462172`
  （2026-11-04）、53 组 / 370 个 workspace 包（`packages/**/package.json` 共 379 个，余下
  是 fixtures/templates）、15 个 bundle；web profile 主层 196 行（171 行启用）。
- 社区条目来自 GitHub `topic:dsh-plugin`（29,208 仓）与 `dsh-plugin in:name`（5,854 仓）
  按 star 排序的前 30，逐个核对仓库描述后收入；`stars` 与 `starsObservedAt` 都写进条目，
  不随时间漂移。

## 权限

- `permissions.fs: ["read:plugin-package"]`：只读**本包**内的 `catalog.json`。
- 无 `net`、无 `secrets`、不读宿主任何路径。诚实边界同其他 sidecar：运行时暂不代理文件访问，
  本插件也只用 `new URL("./catalog.json", import.meta.url)` 读自己的包。

## 手工验证（不装宿主也能跑）

```bash
cd plugins/keel.plugin-market
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"memory","limit":4}}}' \
  '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"info","arguments":{"id":"keel.context-memory"}}}' \
  '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"info","arguments":{"id":"dsh.web"}}}' \
  '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"search","arguments":{"status":["delivered"]}}}' \
  | node sidecar.mjs
```

实测输出（2026-11-06，节选）：

- `search "memory"` → `count: 4`，id 依次 `keel.context-memory`、`community.EverMind-AI.EverOS`、
  `community.MemTensor.MemOS`、`dsh.context`
- `info "keel.context-memory"` → `found: true`，4 条匹配（`matchedBy: ["id","keelPluginId"]` 等）
- `info "dsh.web"` → `coverage: {covered:true, status:"partial", keelPluginId:"keel.web-search",
  tools:["keel_web-search_search"], note:"联网搜索本轮交付；web 服务与抓取仍 planned"}`
- `search status=["delivered"]` → 4 条：`community.EverMind-AI.EverOS`、`keel.context-memory`、
  `keel.plugin-market`、`keel.web-search`

未命中路径：

```bash
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"info","arguments":{"id":"dsh.does-not-exist"}}}' | node sidecar.mjs
# result.isError=true, structuredContent.found=false
```

## 已知边界

- 索引是**快照**（`generatedAt` 2026-11-06），不联网刷新；DSH 上游变化后需重新生成并升版本。
- 只覆盖「官方 53 组 + 社区前 30 条」；社区生态有上万仓，未逐条收录（`source.discovery` 记了
  抓取口径）。
- star 数是抓取时刻的事实，不是实时数据。
- 市场**托管/一键安装**不在本插件（W3 方向）；本插件只回答「有什么、覆盖到哪」。
