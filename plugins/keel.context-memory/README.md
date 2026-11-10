# keel.context-memory

官方上下文记忆插件（`kind: sidecar`）。侧车是一个零依赖 Node 进程，按 Keel 外部 provider
协议（`internal/gateway`，stdio 行 JSON-RPC 2.0）提供**两个**模型工具：把事实/偏好/结论
持久化到插件自己的本地日志，并按关键词、标签、时间窗检索。

## 装了这个插件，模型能用什么

| 侧车工具名（协议面） | 宿主发布名（模型/桥调用） | 作用 |
|---|---|---|
| `remember` | `keel_context-memory_remember` | 持久化一条记忆（文本 + 标签 + 可选来源） |
| `recall` | `keel_context-memory_recall` | 按关键词/标签/时间窗检索 |

> 命名规则（`internal/pluginpkg/manifest.go` 的 `PublishedTools`）：本地名 `remember` 发布为
> `keel_context-memory_remember`（插件 id 的 `.` 变 `_`、`-` 保留）。任务书里的规范名
> `keel_context_memory_remember` 是同一规则下「插件 id 无连字符」的写法；README 的桥调用
> 一律用真实发布名。

### `remember`

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `text` | string | 是 | 记忆正文，1–20000 字符 |
| `tags` | string[] | 否 | ≤32 个，自动 trim + 小写去重 |
| `source` | string | 否 | 来源标签（会话 id / 文件 / URL），≤200 字符 |

返回 `{entry, total, store}`：`entry` 是写入的完整记录（含 `id`、`createdAt`）。

### `recall`

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `query` | string | 否 | 关键词；空表示不过滤（配 `mode=recent` 取最新） |
| `tags` | string[] | 否 | 必须**同时**含有的标签 |
| `since` / `until` | string \| number | 否 | 时间下/上界，ISO 8601 或 epoch 毫秒（闭区间） |
| `mode` | `keyword` \| `recent` | 否 | 默认 `keyword`：按命中 token 打分（标签命中 ×3，正文命中 ×1）；`recent`：按时间倒序 |
| `limit` | integer | 否 | 1–50，默认 10 |

返回 `{query, mode, tokens, filters, count, totalMatches, totalEntries, entries[], store}`；
每条结果带 `score`。**没有命中不是错误**（`count: 0`）。

## 存储与权限

- 日志文件：`<datadir>/memory.ndjson`（append-only，一行一条 JSON 记录）。
- 数据目录解析顺序：
  1. `KEEL_CONTEXT_MEMORY_DIR`（显式覆盖）；
  2. `<plugins root>/.data/keel.context-memory`（由宿主注入的 `KEEL_PLUGIN_DIR`
     推导：与受管包目录同级但不在包内）；
  3. `<cwd>/.data`（手工运行 sidecar 时）。
- 选择 `.data` 同级目录而不是包内目录的原因：升级/卸载插件包只动 `<root>/<id>`，
  记忆不会被 `keel` 的包存储一并删除；同时不会污染包内容哈希。
- 权限：`permissions.fs: ["plugin-data"]`（声明本地数据目录）。**不读**宿主配置、
  **不碰**宿主密钥、**不出网**、无 `secrets`。
- 诚实边界：运行时不代理 sidecar 的文件访问（见 `internal/agentext/plugin_permissions.go`
  的诚实边界注释）；本插件只写自己的数据目录，且不读取任何宿主路径。

## 并发安全

三层防护（都在 `sidecar.mjs` 里，可审计）：

1. **进程内**：请求按到达顺序串行执行（`requestChain`），写入再经 `writeChain` 串成一条链；
2. **跨进程**：写入用 `mkdir` 原子锁（`write.lock`，获取超时 5s，>10s 的陈旧锁会被回收）；
3. **落盘**：`O_APPEND` + 单次 `write` 写整行 + `fsync`；重放时半边行/坏行被忽略并计数
   （`store.corruptLines`），不会拖垮整个库。

## 手工验证（不装宿主也能跑）

```bash
cd plugins/keel.context-memory
rm -rf /tmp/cm-demo
KEEL_CONTEXT_MEMORY_DIR=/tmp/cm-demo node sidecar.mjs <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"remember","arguments":{"text":"Keel plugins run as stdio JSON-RPC sidecars.","tags":["keel","plugins"],"source":"manual-test"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"remember","arguments":{"text":"The web-search plugin falls back across five providers.","tags":["plugins","search"]}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"recall","arguments":{"query":"sidecar plugins","limit":5}}}
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"recall","arguments":{"tags":["search"],"mode":"recent"}}}
EOF
```

实测输出（2026-11-06，节选）：

- `remember` #3 → `{"entry":{"id":"mem_mwad7yf5_fth8y6",...,"createdAt":"2026-11-10T07:25:12.833Z"},"total":1,...}`
- `recall` #5 → `count: 2`，首条 `score: 5`（标签 `plugins` ×3 + 正文命中 ×2）
- `recall` #6 → `count: 1`（只有 `tags:["search"]` 那条）
- stderr 只有一行 `memory ready: 0 entries ...`（重启后是 `22 entries, 1 corrupt line(s)`）

并发写入验证（20 个写 + 1 个读，一次进程）：

```bash
{ echo '{"jsonrpc":"2.0","id":0,"method":"initialize"}'
  for i in $(seq 1 20); do printf '{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"remember","arguments":{"text":"concurrent write %d about plugin memory","tags":["concurrency"]}}}\n' "$i" "$i"; done
  echo '{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"recall","arguments":{"tags":["concurrency"],"limit":50}}}'
} | KEEL_CONTEXT_MEMORY_DIR=/tmp/cm-demo node sidecar.mjs
```

实测：20 个 `remember` 全部成功，`recall` 返回 `count: 20`，`memory.ndjson` 行数 = 20 + 之前的 2。
追加一行坏数据 `{"broken": true` 后重启：`totalEntries: 22, corruptLines: 1`，正常条目不受影响。

## 已知边界

- 单机、单文件、无加密：数据在宿主文件系统上明文（权限由数据目录的 umask 决定）。
- 检索是**词面子串匹配**，不是向量语义检索；中文按连续字串 token 处理（无分词）。
- 无删除/压缩工具（W1 范围只读+写）；日志只增不减，长期使用需外部轮转（停插件后手动处理）。
- 记忆跨插件不可见：只有持有同一数据目录的 `keel.context-memory` 侧车能读写。
