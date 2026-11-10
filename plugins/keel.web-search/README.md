# keel.web-search

官方联网搜索插件（`kind: sidecar`）。侧车是一个零依赖 Node 进程，按 Keel 外部 provider
协议（`internal/gateway`，stdio 行 JSON-RPC 2.0）提供**一个**模型工具：按查询词检索实时
网络并返回结构化结果。

## 装了这个插件，模型能用什么

| 侧车工具名（协议面） | 宿主发布名（模型/桥调用） | 作用 |
|---|---|---|
| `search` | `keel_web-search_search` | 联网检索，返回标题 / URL / 摘要 / 命中 provider |

> 命名规则（`internal/pluginpkg/manifest.go` 的 `PublishedTools`）：宿主把 sidecar 的本地
> 工具名发布为 `<插件 id>.<本地名>`，再把 `.` 归一成 `_`、保留 `-`。所以
> `keel.web-search` + `search` → **`keel_web-search_search`**。任务书里的规范名
> `keel_web_search_search` 就是这条规则下「插件 id 不带连字符」的写法；本插件按真实插件 id
> 发布，`provides.tools` 声明本地名 `search`，桥调用/模型调用用发布名。

参数（`inputSchema` 见 `sidecar.mjs` 的 `TOOL_SCHEMA`）：

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `query` | string | 是 | 查询词，最长 2000 字符 |
| `limit` | integer | 否 | 返回条数，1–10，默认 5 |
| `provider` | string | 否 | `auto`（默认，走回退链）或钉死一个：`bing` / `duckduckgo` / `hn` / `github` / `npm` |

返回（`structuredContent`）：

```json
{
  "query": "keel agent runtime",
  "provider": "bing",
  "resultCount": 3,
  "results": [{"rank": 1, "title": "...", "url": "https://...", "snippet": "...", "provider": "bing"}],
  "attempts": [{"provider": "bing", "ok": true, "resultCount": 3, "ms": 5936}],
  "degraded": false,
  "fetchedAt": "2026-11-06T..."
}
```

回退链默认 `bing,duckduckgo,hn,github,npm`：按顺序尝试，**第一个拿到结果的 provider 胜出**；
前面失败的原因原样保留在 `attempts[]`（`code` / `message` / `url` / `status`），
`degraded=true`。全部失败时工具返回 **`isError: true` 的结构化错误**，`content` 逐条列出
每个上游的真实错误，绝不返回“0 条结果的假成功”。

## 权限

- `permissions.net`：`www.bing.com`、`cn.bing.com`、`html.duckduckgo.com`、`hn.algolia.com`、
  `api.github.com`、`registry.npmjs.org`。
- 无 `secrets`、无 `fs`。
- 诚实边界：`permissions.net` 是声明式清单；sidecar 的 socket 由宿主进程组管理，运行时
  不代理它的出网请求（见 `internal/agentext/plugin_permissions.go` 的诚实边界注释）。

## 环境变量（`entry.env` 只声明名字，值由宿主环境提供）

| 变量 | 默认 | 说明 |
|---|---|---|
| `KEEL_WEB_SEARCH_PROVIDERS` | `bing,duckduckgo,hn,github,npm` | 回退链顺序 |
| `KEEL_WEB_SEARCH_TIMEOUT_MS` | `12000` | 单个 provider 超时 |
| `KEEL_WEB_SEARCH_BUDGET_MS` | `30000` | 整次检索总预算（受宿主请求默认 60s 约束） |
| `KEEL_WEB_SEARCH_LIMIT` | `5` | 默认返回条数（上限 10） |

## 本机可达性边界（2026-11-06 实测）

同一台机器上用 `curl -m 12` 探测（`000` = TLS/连接失败）：

| 上游 | 探测 URL | 结果 | 插件内行为 |
|---|---|---|---|
| Bing | `https://www.bing.com/search?q=test` | 200（302 → `cn.bing.com`，可达） | **默认首选，实测返回真实结果** |
| DuckDuckGo HTML | `https://html.duckduckgo.com/html/?q=test` | 000（`SSL_ERROR_SYSCALL`） | 记录 `tls_error`，继续下一个 |
| Hacker News | `https://hn.algolia.com/api/v1/search?query=test` | 200 | 可用 |
| GitHub | `https://api.github.com/search/repositories?q=keel` | 200 | 可用 |
| npm | `https://registry.npmjs.org/-/v1/search?text=test` | 200 | 可用 |
| Wikipedia | `https://en.wikipedia.org/w/api.php?...` | 000 | 未纳入默认链（本机不通；需要时自行加 provider） |
| Brave Search | `https://search.brave.com/search?q=test` | 000 | 未纳入（且需 API key） |

因此：**本机（有网但部分域名被遮挡）Bing 链可用**；完全断网或 DNS/代理故障时，工具会以
`isError: true` 返回每个上游的具体错误（`dns_error` / `tls_error` / `timeout` / `http_error` …）。

## 手工验证（不装宿主也能跑）

```bash
cd plugins/keel.web-search
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-11-07"}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"keel agent runtime","limit":3}}}' \
  | node sidecar.mjs
```

预期：三行 JSON。第 3 行 `result.structuredContent.provider` 非空、`results` 至少 1 条；
若网络全挂，第 3 行 `result.isError === true` 且 `attempts[]` 带真实错误码。

断网路径（不真的断网，钉一个不可达上游）：

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"test","provider":"duckduckgo"}}}' \
  | node sidecar.mjs
# 本机预期：result.isError=true，attempts[0].error.code ∈ {tls_error,dns_error,unreachable,timeout}
```

## 已知边界

- 结果质量取决于上游 HTML/API；Bing/DuckDuckGo 是 HTML 解析，页面结构变化会退化为
  `empty_result`（结构化错误，不会假装成功）。
- 没有分页、没有抓取正文（只返回搜索摘要）；正文抓取属于 W2 的 `keel.web` fetch 面。
- 未做速率限制/缓存；高频调用会被上游限流（届时 `http_error` 会如实返回）。
- 需要 Node ≥ 20（`fetch`、`AbortSignal.any`、`AbortSignal.timeout`）。
