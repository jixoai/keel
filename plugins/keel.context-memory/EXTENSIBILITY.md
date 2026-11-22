# keel.context-memory — 扩展面

`keel.context-memory` 是一个 `kind: sidecar`、`layer: provider` 插件。它提供两个模型工具
`remember` / `recall`（宿主发布名 `keel_context-memory_remember` / `..._recall`），不声明槽位、
不投 UI 贡献、不订阅 hook。

## 我能被怎么扩展

| 扩展点 | 形式 | 说明 |
|---|---|---|
| 数据目录 | `KEEL_CONTEXT_MEMORY_DIR` | 显式覆盖；否则 `<plugins root>/.data/keel.context-memory`，再退到 `<cwd>/.data`。 |
| 存储后端 | `providers[].port: memory.store` | 同端口可换实现（例如未来的向量检索），工具名与动作不变。 |

槽位与贡献：

```json
"slots": [],
"contributes": []
```

## 调用示例

```json
{ "text": "The release train leaves on Thursday", "tags": ["release"] }
{ "query": "release", "limit": 5 }
```

## 边界与限制

- 权限：`permissions.fs: ["plugin-data"]`，只写自己的追加日志；无 `net`、无 `secrets`。
- 存储格式是本插件自己的 NDJSON；坏行隔离，不影响其余条目。
- 单条文本 20k 字符、32 个标签、召回上限 50；不做摘要、不做向量检索（登记为后续能力）。
- 不读宿主会话账本，不把记忆写进宿主 secrets。
