---
title: 多租户、RLS 与配额
description: 租户如何隔离与限额——单库 tenant_id、默认开启的行级安全、按维度各有一套词汇的两类配额，以及租户管理 API。
---

# 多租户、RLS 与配额

Keel 的多租户落在**一个 PostgreSQL 18 库**里：每行带 `tenant_id`、复合键，没有每租户一个库或进程
（`README.md:70`）。三层机制把租户分开——schema 的谓词纪律、作为第二道防线的行级安全，以及两套配额词汇在
三个执行点上生效。

## 1. 租户行

`tenants` 保存身份与边缘限额（`internal/store/schema.sql:5`），以及配置文档和每次写入都会变动的
revision（`internal/store/schema.sql:16`）。`quota_storage_bytes` 通过 `ALTER` 追加，让既有库收敛而不是
重建（`internal/store/schema.sql:703`）。

## 2. 行级安全默认开启

- 应用不以表属主身份连接：它 `SET ROLE` 到专用非属主角色（`keel_app`，创建时
  `NOLOGIN NOSUPERUSER NOBYPASSRLS`），因为属主与超级用户会绕过 RLS（`internal/store/rls.go:45`、
  `internal/store/rls.go:259`）。
- 每张租户表都 `ENABLE` + `FORCE ROW LEVEL SECURITY`，并有一条 `tenant_isolation` 策略，其
  `USING`/`WITH CHECK` 把 `tenant_id` 与 `current_setting('app.tenant_id', true)` 比较。missing-ok
  形式让未设置的变量取 NULL，因此忘记谓词的查询返回**零行**而不是别的租户的数据；运维路径显式用
  `app.bypass_rls = 'on'` 放行（`internal/store/rls.go:292`）。
- 受保护表清单只在一处维护（`internal/store/rls.go:57`）。`plugins` 被刻意排除：插件启用是进程事实而
  不是租户事实，租户角色在它上面没有任何授权（`internal/store/rls.go:54`）。
- 迁移幂等、用 advisory lock 串行化、对并发目录写错误重试；无法创建角色的集群会**明确失败**，而不是
  留着 `FORCE` RLS 却没有可用的角色（`internal/store/rls.go:120`）。
- `KEEL_RLS=off` 是托管集群禁止 `CREATE ROLE` 时的显式逃生门（`cmd/keel/serve.go:313`），不会被推断
  出来。

## 3. 配额：两套词汇，三个执行点

run 与事件预算在租户配置文档里；流、带宽与存储预算在 `tenants` 行里。这个分工是刻意的，代码写明了规则：
「每个维度只有一套配额词汇」（`internal/store/byte_usage.go:181`）。

| 词汇 | 维度 | 执行点 | 拒绝方式 |
|---|---|---|---|
| 租户文档（`host.QuotaConfig`） | `runConcurrency`、`eventsPerMinute` | run 并发：持久放置闸门；事件速率：内核进程内 token bucket | `429`（`internal/httpapi/server.go:838`） |
| `tenants` 配额列 | `quota_streams`、`quota_bytes_per_sec`、`quota_storage_bytes` | 进程边缘，在任何账本行存在之前 | `429` / `413`（`internal/quota/edge.go:9`） |

run 并发由放置闸门做持久准入，内核只保留 event-rate 桶；这个分工是刻意的，一个 run 不会被计两次
（`cmd/keel/serve.go:412`、`internal/quota/limits.go:227`）。存储的租户文档覆盖部署默认值，并以 2 秒
缓存重读（`internal/quota/limits.go:58`、`internal/quota/limits.go:244`）；读取或解析失败时拒绝准入而
不是猜一个配额（`internal/quota/limits.go:20`）。存储配额是对 blobs 表的实时测量，所以删掉 blob 就真的
释放配额（`internal/quota/edge.go:24`）。放置的细节见[放置与组](/zh/core/placement)。

## 4. 租户管理 API

租户级操作都是运维操作，且不存在「租户级」API 凭据：人类账号用密码登录，Bot 用绑定到自己的
`keel_bot_…` key（`internal/harnessapi/tenancy.go:7`）。provisioning 需要部署的运维密钥作为 Bearer 头
（`internal/harnessapi/tenancy.go:158`、`docs/HOST.md:172`）。

| 方法 / 路径 | 作用 | 说明 |
|---|---|---|
| `POST /v1/admin/tenants` | 创建租户并播种内置权限组 | `201`；按 id 幂等（`internal/harnessapi/tenancy.go:58`） |
| `GET /v1/quota` | 租户自己的限额与当前用量 | `streams`、`bytesPerSec`、`storageBytes`；只有计数，无价格（`internal/harnessapi/tenancy.go:141`） |
| `GET /v1/auth/whoami` | 解析出的租户、账号、Bot 与权限集 | 说明当前凭据到底是什么（`internal/harnessapi/tenancy.go:56`） |
| `PUT /v1/tenants/{id}/config` | 替换租户配置文档 | body 即文档；非法 `400`、超限 `413`、租户不存在 `404`（`docs/HOST.md:178`） |
| `GET /v1/tenants/{id}/config` | `{tenantId, revision, config}` | `config_revision` 是变更身份 |
| `GET /v1/tenants/{id}/usage` | 按 session/provider/model 分组的 token 计数 | 只有计数，含 `unaccountedEntries` |
| `POST /v1/tenants` | 用显式 id 创建租户 | `X-Tenant-ID` 只是行 id，不是凭证（`docs/HOST.md:177`） |

```bash
# 创建租户；运维凭据是部署的 KEEL_ADMIN_KEY
curl -s -X POST http://127.0.0.1:13750/v1/admin/tenants \
  -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"acme"}'

# 该租户此刻能用多少；机器客户端出示 bot key
curl -s http://127.0.0.1:13750/v1/quota \
  -H "Authorization: Bearer $KEEL_BOT_KEY"
```

`/v1/quota` 需要带 app-read 权限的 key；运维路由在访问矩阵里标记为 `ModeOperator`，普通租户凭据无法做
provisioning（`internal/accessgate/matrix.go:154`、`internal/accessgate/matrix.go:383`）。

## 5. 单租户 `keel serve` 与多租户 `cmd/runtime`

`keel serve` 是**单个租户**的产品进程（`KEEL_SERVE_TENANT`）：它的扩展读接口（plugins/triggers）返回
该进程的全局装配状态，要做多租户复用需要另立 change（`docs/HARNESS.md:39`）。进程级资源无法在一个
进程里假装按租户隔离——MCP 目录就是文档举的例子——因此多租户部署是**每租户一个进程**
（`docs/HOST.md:262`）。

`cmd/runtime` 是裸运行时而不是产品外壳：它仍以 `X-Tenant-ID` 作为运行时租户接口，其它地方鉴权中间件
注入该头并删除客户端自带的值（`docs/HOST.md:270`）。所以多租户是一种部署形态（一个库、多个租户、
每个租户一个产品进程），而不是单个 `keel serve` 里的按请求模式切换。
