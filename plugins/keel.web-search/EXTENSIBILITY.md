# keel.web-search — 扩展面

`keel.web-search` 是一个 `kind: sidecar`、`layer: provider` 插件。它只提供一个模型工具
`search`（宿主发布名 `keel_web-search_search`），不声明槽位、不投 UI 贡献，也不订阅 hook。

## 我能被怎么扩展

| 扩展点 | 形式 | 说明 |
|---|---|---|
| 上游顺序 | `KEEL_WEB_SEARCH_PROVIDERS` | 逗号分隔的有序列表；默认 `bing,duckduckgo,hn,github,npm`。 |
| 超时预算 | `KEEL_WEB_SEARCH_TIMEOUT_MS` / `KEEL_WEB_SEARCH_BUDGET_MS` | 单上游超时与整条回退链预算。 |
| 默认结果数 | `KEEL_WEB_SEARCH_LIMIT` | 1–10；工具参数可覆盖。 |
| provider 端口 | `web.search` | `providers[].port` 声明；替换实现不改变模型可见工具名。 |

槽位与贡献：

```json
"slots": [],
"contributes": []
```

## 调用示例

```json
{ "query": "keel harness v2", "limit": 5 }
```

返回结构化结果；上游失败时 `isError: true` 并逐个报告 `dns_error` / `tls_error` / `timeout`，
不伪造成功、不拼接猜测内容。

## 边界与限制

- 权限：只声明 `permissions.net` 的六个上游 host；无 `secrets`、无 `fs`。
- 出网请求由 sidecar 自己发出，运行时当前不代理；声明是诚实边界。
- 整条链受 30s 预算约束；单个上游 12s；结果上限 10 条。
- 不提供抓取正文、不提供浏览器操作、不做结果去重之外的排序干预。
