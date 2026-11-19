# 直接配置凭据 E2E 转录（2026-11-14，13760 真库）

目的：证明「不经 env、在密钥库直接配置」的凭据链路端到端成立（显式 `secret:` 引用形态）。

## 步骤与结果

```
# 1) PUT 密钥（值不回读）
PUT /v1/settings/secrets/DIRECT_FX_KEY {"value":"<随机 sk-direct-… >"}
→ 200 {"name":"DIRECT_FX_KEY","updatedAt":"2026-11-14T18:23:06+08:00"}

# 2) 添加 provider，凭据用 secret 引用（本地假上游 127.0.0.1:13769）
keel model provider add keel-direct-fx --type llm --name "Direct FX" \
  --api-type openai-chat-completions --base-url http://127.0.0.1:13769/v1 \
  --ref secret:DIRECT_FX_KEY --model direct-fx-model
→ added provider keel-direct-fx (revision 7; 9 providers executable, 11 models selectable)
   （executable=yes 说明 secret 引用已解析）

# 3) 真实会话（selection 指向该 provider/model）
POST /v1/sessions → session 68f027c1-…
POST /v1/sessions/68f027c1-…/input {"text":"直接配置测试",
  "selection":{"providerId":"keel-direct-fx","modelId":"direct-fx-model"}}
→ 201

# 4) 事件断言
assistant.message: [{'text': 'DIRECT-CONFIG-OK', 'type': 'text'}]
run.finished: {"status": "done", "model": "direct-fx-model", "provider": "keel-direct-fx"}

# 5) 假上游鉴权日志（本地假上游只在 Bearer == 密钥库存值时返回 200）
{"path": "/v1/chat/completions", "match": true, "hasAuth": true}   # 自检
{"path": "/v1/chat/completions", "match": true, "hasAuth": true}   # keel 的请求
```

## 清理（起过必回收）

```
DELETE /v1/sessions/68f027c1-…            → 200（软删除）
keel model provider remove keel-direct-fx → removed (revision 10)
DELETE /v1/settings/secrets/DIRECT_FX_KEY → 200
kill 假上游（13769）→ lsof 无监听；临时目录删除
终态：providers=8（与初始一致）；secrets available=true、names=[]
```

## 说明

- 该实证走的是**显式** `secret:NAME` 引用；内置目录 provider 的 `env:NAME` 声明在 env 未设置时
  自动使用同名密钥属于本 change 的待实现项（见 tasks.md）。
- 主密钥 `KEEL_SECRET_KEY` 只存在于 gitignored 的 `.env.local`，全程未打印。
