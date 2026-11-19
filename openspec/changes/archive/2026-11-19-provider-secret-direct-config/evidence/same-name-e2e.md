# 同名密钥回退真库 E2E（2026-11-19）

目的：证明内置形状 provider 的 `access.env: NAME` 在**进程环境变量未设置**时，按同名从租户密钥库
解析凭据（provider-secret-direct-config 的最后一项）。

## 环境

- 临时库 `keel_prov_e2e`（跑完 DROP）；真 `keel serve`（127.0.0.1:13773，password 认证）；
  本地假 OpenAI 流式上游（127.0.0.1:13774）。
- serve 进程环境**不含** `SAME_NAME_E2E_KEY`；密钥库保存同名 secret（值为合成随机串，全程未打印）。
- provider 文档（PUT `/v1/settings/models`）：
  `{"id":"same-name-e2e","type":"llm","api":{"type":"openai-chat-completions","baseUrl":"http://127.0.0.1:13774/v1"},"access":{"type":"api-key","env":"SAME_NAME_E2E_KEY"},"modelIds":["same-name-model"]}`，
  `selection` 指向该 provider/model。**文档内不出现凭据值**。

## 转录（真实输出节选）

```
POST /v1/auth/login                          → 200
PUT  /v1/settings/secrets/SAME_NAME_E2E_KEY  → 200 {"name":"SAME_NAME_E2E_KEY"}
PUT  /v1/settings/models                     → 200 revision=1 source=stored
     providersExecutable=1  modelsSelectable=1        # env 未设置也能解析 → 同名 secret 生效
keel model provider test same-name-e2e --json → rc=0
     executable=true selectable=true ok=true issues=[]
POST /v1/sessions                            → 201
POST /v1/sessions/{id}/input                 → 201
GET  /v1/sessions/{id}/run                   → done attempts=1 lastError=""
run.finished                                 → {"status":"done","provider":"same-name-e2e","model":"same-name-model"}
assistant.message                            → "SAME-NAME-OK"
假上游鉴权日志                                → {"auth_match":true,"has_auth":true}
```

`auth_match=true` 的判定：请求头 `Authorization` 精确等于 `Bearer <密钥库中的同名值>`。

## 清理（起过必回收）

- `keel serve` 收 SIGTERM 退出（日志 `keel serve stopped`）；假上游随脚本进程退出；
  `pgrep` 无残留；端口 13773/13774 无监听。
- 临时库已 `DROP DATABASE keel_prov_e2e`（`pg_database` 查询为空）。
- 密钥值、DSN 全程未打印、未写入仓库。

## 备注

原生内置目录 provider（`env: S2API_KEY`）同样走这条判定链（`credentialDecision()` 单一判定，
单测已覆盖 env 胜/同名回退/双缺 fail-closed）；本 E2E 用等价文档形状验证同一生产路径。
