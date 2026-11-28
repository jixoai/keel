# 运行手册

## 本地起 PostgreSQL

无 Docker（推荐）：`keel db` 管理一个本机一次性 PostgreSQL 18 集群——默认目录
`$KEEL_HOME/dev-pg`（未设置时 `~/.keel/dev-pg`，`--dir` 可改）、仅监听
`127.0.0.1:13741`、随机密码存 0600 文件、不触碰已有实例。`keel serve`/`keel admin`
在 `KEEL_DSN` 未设置时会自动复用/初始化该集群，通常不需要先跑下面的命令；
`KEEL_NO_AUTO_DB=1`（或 `keel serve --no-auto-db`）关闭自动并报错，生产部署请显式设置
`KEEL_DSN`。

```bash
go build -o keel ./cmd/keel
./keel db start         # 首次 initdb + 启动 + 建库 keel；打印 DSN
./keel db status        # 运行状态 + DSN
./keel db stop          # 停止，数据保留
./keel db reset --yes   # 停止并删除本工具创建的集群（数据丢失）
eval "$(./keel db env)" # 在当前 shell 导出 KEEL_DSN
```

Docker 备选（端口同为 13741，二者不要同时开）：

```bash
docker run -d --name keel-runtime-pg \
  -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 --memory=2g --cpus=2 postgres:18
export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
```

## 起服务

```bash
export KEEL_DSN='postgres://…'   # 或：eval "$(./keel db env)"
go build -o /tmp/keel-runtime ./cmd/runtime
GOMEMLIMIT=1610612736 /tmp/keel-runtime \
  -addr 127.0.0.1:13740 -cas /tmp/keel-cas \
  -streams 2200 -chunk-batch 1 -admit-rate 1200
```

- `-streams` 是总并发流上限；1000 会话需要 ≥1000（SSE）+ 图片流余量，建议 1600–2200。
- `-chunk-batch 1` ≈150KB/流；`2` 是 1Gbps 模式（PG 查询减半）。
- `-bandwidth` 是全局出站字节/秒（0=不限）；生产按链路 85–90% 设。
- `-cas` 必须与上传目录一致，否则 >1MiB 的图读不出来。

## Harness 产品进程（`keel serve`）

`cmd/runtime` 是多租户运行时（每请求 `X-Tenant-ID` + `KEEL_ADMIN_KEY`）；`keel serve` 是产品壳，
认证模型不同：**账号 + 密码**登录，机器客户端用绑定到具体 Bot 的 key。

```bash
export KEEL_DSN='postgres://…'
export KEEL_SERVE_TENANT='<tenant-uuid>'
export KEEL_SECRET_KEY='<32-byte base64/hex>'       # cookie/CSRF 签名
keel admin bootstrap --tenant "$KEEL_SERVE_TENANT"  # 空库首次；一次性 setup token
keel serve --addr 127.0.0.1:13750
```

- 浏览器用 setup token 完成首个超管设置，设置即完成并直接进入；之后用户名/密码登录。CLI/管理员重置出的临时密码仍需先改密。
- CLI/TUI：`KEEL_ACCOUNT_USERNAME` + `KEEL_ACCOUNT_PASSWORD`（或 `--password-file`）；
  写请求由客户端带 `X-Keel-CSRF`。
- 机器客户端：`KEEL_AGENT_TOKEN=keel_bot_...`（绑定 `account_id + bot_id`，不能登录浏览器）。
- `--auth required|local-only|off`：`required` 默认；`local-only` 仅回环并映射内置 local-admin；
  `off` 必须显式 `--insecure`，公网拒绝启动。
- **零兼容**：`KEEL_SERVE_PASSWORD`、旧 cookie、旧 `api_keys`、旧 `agents`/`agent_teams` schema
  全部失效；启动会拒绝旧 schema 并给出重建指令，不提供迁移。升级即重新初始化账号与 Bot。

### Board（keel-board v0.1.4）

`keel serve` 启动时在同一个进程内挂载 board 表面（`github.com/jixoai/keel-board@v0.1.4`，
commit `3d6115b68a1cc0ef1de73364e03787ceb610785f`，tag `v0.1.4`）。board 是独立授权面：
`board.read/write/manage` + 频道 `resource_relations`，不经过 keel 的 `accessgate` 矩阵。

**开关与存储**

```bash
export KEEL_BOARD_ENABLED=1          # 默认 0（关闭）：/v1/board/* 与 /webhooks/* 404 board_disabled；1 开启
export KEEL_BOARD_DSN='postgres://…' # 可选；默认复用 KEEL_DSN
export KEEL_BOARD_SCHEMA=board       # 可选；board 表独立 schema（不存在时自动 CREATE SCHEMA）
export KEEL_BOARD_CHANNEL_SCAN=5m    # 可选；频道 owner 关系修复间隔，负值只跑启动扫描
export KEEL_SECRET_KEY='…'           # sync 凭据必需：32 字节；缺失时 credential=missing，sync fail closed
export KEEL_BOARD_FILE_HOSTS=files.slack.com,api.github.com # 可选；入站文件/链接允许的 host，空=全部拒绝
export KEEL_BOARD_PROVIDER_HOSTS=gateway.internal # 可选；自定义平台 API endpoint 的 HTTPS host 白名单；loopback 恒允许（测试）
export KEEL_BOARD_MAX_FILE_BYTES=26214400  # 可选；单文件上限（默认 50 MiB）
export KEEL_BOARD_MAX_FILES=10       # 可选；单条消息文件数
export KEEL_BOARD_MAX_LINKS=10       # 可选；单条消息链接数
export KEEL_BOARD_MAX_WEBHOOK_BYTES=1048576 # 可选；webhook body 上限
export KEEL_BOARD_FILE_TIMEOUT=10s   # 可选；单个入站文件下载超时
```

- 启动幂等执行 board 的 `board_*` 迁移（core / sync / automation / audit RLS）。
- **RLS 双设置边界**：keel 既有表用 `app.tenant_id`；board 表用 `keel.tenant_id`，且
  **不**注册进 keel 的 `rlsTenantTables`。两套 policy 不互相覆盖；board 查询由 board 自己的
  连接设置 tenant，未设置时匹配零行（fail closed）。keel 侧的 sync entry 配置表
  `board_tenant_sync_entries` 与附件映射 `board_tenant_blob_keys` 是普通 tenant 表，已注册
  进 keel 的 RLS。
- board 的 DSN 建议使用**非超级用户**角色（拥有 board 表即可；`FORCE RLS` 对 owner 生效，
  超级用户/`BYPASSRLS` 总是绕过 RLS）。`keel serve` 启动时会检查该角色并 warn（不阻断）。
  专用库/角色用 `KEEL_BOARD_DSN`；单库单租户可复用 `KEEL_DSN`。
- 镜像/Dockerfile 只构建 `cmd/runtime`，**不含 board**。生产 board 入口是 `keel serve`
  （`KEEL_BOARD_ENABLED=1`）；若要让 runtime 也提供 board，需要另一条接入 lane。keel 的
  二进制预算为 **4096 KiB**（board + sync/secrets 约 3.0 MiB；`cmd/runtime` 未新增依赖，
  仍受同一脚本的 4096 KiB 上限约束）。
- 跨租户：HTTP 侧 tenant 只来自登录 principal（不读 header/path），A 租户读不到 B 的行；
  数据库侧见 `internal/boardintegration` 的 RLS 测试（restricted role + unset tenant）。

**Phase 2 契约（keel 挂载层，board 模块未改）**

- `GET /v1/board/me`：当前 actor / account / bot、`tenant`、`can.board.{read,write,manage}`、
  可读频道的 `channels{path:{read,write,manage}}`。未认证 401；跨租户只看到本租户（tenant
  来自登录 principal，不读 header/path）；board 关闭时和其他路由一样 404 `board_disabled`。
- `TaskView.moves[]` 是 UI 的唯一动作来源。挂载层按 board 状态、owner/for/claim、当前 actor
  和频道关系推导，不写 `permission.denied`：
  - `ready`：`claim`（`for` 为空或匹配当前 actor，需 `board.write`+频道 editor）；owner/manage
    另有 `block`/`assign`/`needs`/`cancel`。
  - `claimed`（租约有效）：holder 有 `renew`/`release`/`complete`/`fail`/`block`；owner/manage
    有 `assign`/`cancel`（board 的 claim 围栏拒绝 owner 在活 claim 上 `block`）。
  - `blocked`：owner/manage 有 `unblock`/`assign`/`needs`/`cancel`；`failed`：owner/manage 有
    `reopen`/`cancel`；`waiting`：owner/manage 有 `assign`/`needs`/`cancel`。
  - 任何非归档、可写频道都有 `reply`（需频道 editor）。租约过期的 claim 按 `ready` 处理，
    不再提供 token 动作。
  - `archived=true`：`moves=[]`。命令桥已在 change keel-plugin-split 删除，board 不再把
    动作回呼 supervisor；superagent 的 ticket 是唯一真值，board 任务由 `keel.superagent`
    插件的只写投影维护。
  - `takeover`、`release.force`、`answer` 在当前 board v0.1.1/keel 没有后端端点，**不**谎报。
- `archived` 来自 `board.archived=true` 标签（客户端不能写 `board.*`）；归档 Ticket
  导入为只读历史任务。`copies` 暂不提供：board v0.1.1 的 message view 只有
  `undelivered`，没有 task-level 成功副本列表，mount 层不伪造空数组。
- `/v1/board/sync/entries|status` 保留 board 形状，合并 keel 的凭据状态
  （`configured`→`valid`，`missing`/`invalid`→`invalid`，keel 无该 entry→`unknown`）；
  board v0.1.1 没有可测量的 lag 时间戳，因此 `lag: null`（明确未知），不伪造 `targets`
  或 0 值。
- 改写只作用于上述路由的 2xx JSON 响应；SSE、错误状态/错误体、内容类型保持不变。
  SSE 帧按 board 事件契约**不带** `moves`；客户端合并任务事件时需保留已有 `moves` 或重新读取
  任务，否则事件后的卡片会退回只读。

**tenant 开关与 sync entries**

```json
// tenant 配置文档（keel admin 的 config，或 st.SetTenantConfig）
{"board":{"enabled":true,"sync":{"enabled":true}}}
```

- **route 前置条件（fail closed）**：`routes` 里每个 board channel 必须已经存在，并且
  在自身或任一祖先路径上有至少一条 `resource_type=board_channel` 的关系（与 board 的
  最近祖先继承一致）；否则
  `POST/PATCH /v1/board/sync/entries` 返回 400，提示先 `POST /v1/board/channels`
  （创建者自动成为 owner）或走孤儿频道认领。boot 时历史 entry 也做同一校验：不合格的
  entry 在 board sync store 停用、日志给出原因、不启动 worker，进程继续启动。
- `board.enabled=false`：`/v1/board/*` 全部 404 `board_disabled`；`/webhooks/*` 同样 404 且
  不读 body、不写 inbox。`board.sync.enabled=false`：核心 board 可用，`/v1/board/sync/*`、
  `/v1/board/sources/*` 与 webhook/bridge 返回 404 `sync_disabled`。
- 开关按请求读取（5s 缓存），关闭即时生效；打开 sync 需要重启装载 entry（`keel serve`
  启动时读取 `board_tenant_sync_entries`）。
- sync entry 固定属于部署租户（`KEEL_SERVE_TENANT`）：`/webhooks/{key}` 没有 tenant 段，
  其他租户的 manage 调用在管理路由上返回 404，避免存下永远收不到平台事件的 entry。
- entry 只存 `secret_ref`（role → tenant secret 名称）；secret 值用
  `PUT /v1/settings/secrets/{name}`（需 `secret.write`）或 `tenant_secrets` 存储，绝不写入
  entry 行、日志、响应、指标或审计。

```json
// POST /v1/board/sync/entries（board.manage + secret.write）
{
  "key": "slack", "platform": "slack", "enabled": true,
  "secretRef": {"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT_TOKEN"},
  "settings": {"botUserId": "U123", "botId": "B123", "apiBaseUrl": "https://slack.com/api"},
  "routes": {"C0123": "support"},
  "startChannels": ["C0123"]
}
```

- PATCH `/v1/board/sync/entries/{key}` 接受 `enabled` / `secretRef` / `settings` / `routes` /
  `startChannels`。`enabled:false` 永远允许（即使 route 频道已被删除/撤权）；
  `enabled:true` 或 `routes` 变化会重新校验 route。凭据/settings/platform 变更可先保存，
  但 entry 保持 pending/disabled 直到重启，boot 会再次校验 route。启停即时生效；创建、凭据
  轮换、settings/routes/startChannels 变更需要重启，变更期间 entry 在 board sync store 中
  停用（安全失败，旧凭据不会继续服务），响应 `restartRequired:true`。
- `enabled:false` 的 entry 可作为安全 staging，允许 `routes` 指向尚未创建的频道；启用时
  （`enabled:true`）再返回 400 要求先创建/授权频道。
- `GET /v1/board/sync/credentials`（board.manage）返回每个 entry 的
  `configured|missing|invalid`；`GET /metrics` 的 `board_sync_credential_status{key,status}`
  同口径。凭据只报状态，不含值或密文。entry 同时返回 `restartRequired`：凭据本身可能已是
  configured，但本进程仍持有旧 provider 时它明确标为待重启，而不是假装已生效。
- `platform` 支持 `slack` / `github` / `linear` / `chatsdk`；GitHub 需 `repos`（入站）与
  `writeRepos`（出站，空=不写），Linear 需 `botUserId` + `teamKeys`，chatsdk 需
  `settings.platform`、`bridgeUrl`、`selfIds` 与 `secretRef.bridgeSecret`。

**权限**

- `board.read` → 频道 viewer；`board.write` → 频道 editor；`board.manage` → tenant 级管理
  （sync entries/audit/projection；`channel="*"`）。频道 owner 关系允许任务 owner 操作。
- 最近祖先继承：先查精确路径，再逐级向上；某一层对调用者有任意关系即生效（显式 viewer
  可收窄父级 editor）。
- 创建频道（`POST /v1/board/channels`）成功 201 后，adapter 立即写创建者的 `owner` 关系；
  隐式子频道（父频道 editor 下 `POST /messages`/`/tasks` 首次出现）也在 201 后写。
- **孤儿频道认领**：对已存在但**自身、整条祖先路径和所有后代路径都零 relation** 的频道，
  `board.manage`（tenant/all scope）调用 `POST /v1/board/channels` 返回 200，并补写调用者
  owner；自身/祖先/后代已有 relation 时保持现状，不会借该路径夺取继承 ACL，也不会让父频道
  owner 通过祖先继承覆盖已有精确 relation 的子频道。board.write 调用者对无关系频道仍 404
  （不能探测存在性）。
  **正式偏离 design §11.2 的“同一事务”**：board 模块与 keel 关系表不能共享事务，因此
  owner 写入是非原子的；启动与周期扫描按 `channel.created` 和每个频道最早的
  `message.posted` root 补偿（崩溃窗口可用测试复现）。
- 已知风险：未来 Phase 2 的频道 ACL 撤销后，上述 reconciler 会按历史创建事件重新补授
  owner；Phase 2 必须增加 revocation marker（或改为显式 owner 管理），否则撤销不持久。
- 权限拒绝写 `auth_events`（`permission.denied`），detail 只有 key/relation/scope，不含正文；
  私有频道的未授权读/写同样先记审计，再向调用者返回 404。

**API / SSE**

- Base `/v1/board`；沿用 keel 登录与 Bot key，写请求仍要 `X-Keel-CSRF`（人类会话）。
- `GET /v1/board/events`：SSE；`Last-Event-ID` 优先于 `after`；adapter 每 15s 发 `: ping`，
  55s 主动断开由客户端带游标重连；无权事件不下发内容。
- 同步：`GET /v1/board/sync/entries|entries/{key}/status|status|inbox|outbox/failures|threads`
  与 `GET /v1/board/sources/{key}/read|join` 需要 `board.manage`；entry 创建/启停/轮换见上。
  平台 webhook `POST /webhooks/{key}` 不走会话认证，由 provider 验签；未配置 entry 时
  不读取 body、不落库（pre-auth unknown key）。Chat SDK bridge
  `POST /v1/board/sync/bridge/{key}/events` 同样绕过会话，用 HMAC v1 + 5 分钟 nonce 窗口。
- 时间线：`keel serve` 启动时固定每个 owned entry 的 board cursor（`PrepareSync`），然后启动
  board 事件订阅、每 entry 的 inbound/outbound worker、watcher/schedule worker；worker 全在
  serve context 下，退出时停止并等待。

**superagent 镜像 / 命令桥（Phase 1 11.x）**

```jsonc
// tenant 文档（PUT /v1/tenants/{id}/config 的 config 字段）
{
  "board": {
    "superagent": {
      "mirror": true,          // 默认 false：关闭时完全 no-op
      "bridge": true,          // 默认 false：board 动作先调 supervisor
      "channel": "superagent"  // 默认 superagent；非法路径导致该 adapter fail closed
    }
  }
}
```

```bash
export KEEL_SECRET_KEY='…'     # 必填；缺失时 board 投影 fail closed（不写、不 claim）
```

- 插件边界：`keel.board` 拥有 task/channel/thread 与 board 存储，暴露
  `boardplugin.Service`；`keel.superagent` 声明 `requires: [board]`，自有 `superagent_*`
  表与 `superagent_board_links`，board 缺失/禁用时 `PENDING`。旧镜像/命令桥已删除
  （破坏性变更），互不 import 由 `internal/architecture` 门禁守住。
- 方向：superagent → board 只写投影 + board task → ticket 导入。Ticket→任务线程、
  Assignment→claim（lease 900s，按 assignment 心跳续期）、Attention→blocked
  （`needs_input`/`awaiting_approval`）、report→回复；可执行的 board 任务经 board 服务导入为
  ticket，再由常规 dispatch 创建带 worker session 的 assignment（不写无 session 的 open
  assignment）。superagent 的 `superagent_*` 始终是真值，适配器只追加 board 事件。
- 幂等：`superagent_board_links` 的 `(tenant, source, source_id, kind)` 唯一；重复同步/重启
  复用同一 task/回复；report_id / attention_id 是回复幂等键；claim token 由
  `HMAC-SHA256(KEEL_SECRET_KEY, "board-claim:v1:<tenant>:<assignment>:<session>")` 派生，服务端只存 hash。
- 只读：投影线程/消息写 `noSync`，统一 GitHub sync 不会二次回写；旧 `/superagent` 看板与
  `/v1/superagent` 只读/运营 API 已删除（破坏性变更，旧脚本归档在
  `scripts/archive/retired-superagent/`），board 队列/线程是唯一任务面。
- 命令桥已删除：board 上的 claim/complete/fail/block/unblock/assign/cancel 只改 board，
  不再回呼 supervisor；superagent 运营 API（resume/stop/answer）仍走 `task.write` 授权。
- 运营组：投影对 `tenant-admin` 与 `bot-operator` 写 channel 的 `owner` 关系；
  `bot-operator` 预设同时带 `board.read`/`board.write`（team scope），因此默认运营组成员
  可以 claim/complete/block；没有该频道关系的 board.write 用户仍 404。`board.manage`
  （sync entries 等）不在 `bot-operator` 预设内。
- 验收：`KEEL_TEST_DSN='postgres://…' go test ./internal/superagentplugin/ ./internal/superagentstore/ ./internal/boardplugin/ -v` 与
  `go test ./internal/pluginruntime/ ./internal/pluginstore/...`。

**验收 / 排障**

```bash
# board 适配 + sync/secret 管理（真实 PG；KEEL_TEST_DSN 指向专用库）
KEEL_TEST_DSN='postgres://…' go test ./internal/boardintegration/ -v
KEEL_TEST_DSN='postgres://…' go test -race ./internal/boardintegration/
KEEL_TEST_DSN='postgres://…' go test ./internal/boardintegration/ -run TestBoardStoreRLSUsesTenantSetting -v
# Phase 2 契约：/me、moves 矩阵、归档导入、sync 凭据合并
KEEL_TEST_DSN='postgres://…' go test ./internal/boardintegration/ -run 'TestBoardMe|TestTaskMoves|TestBoardJSON|TestSyncEntriesMerge' -v
KEEL_TEST_DSN='postgres://…' go test ./internal/superagentplugin/ -run TestBoardTaskToTicketToBoardWriteback -v
go test ./internal/access/ ./internal/accessgate/ ./cmd/keel/
scripts/binary-size.sh --budget-kb 4096 --base 6ee7fe3

# 全量 + E2E（fake Slack/GitHub/Linear/Chat SDK bridge + superagent 命令桥；
# 生成 HTML 报告；含 fresh-channel 400/201/导入、board_disabled、supervisor
# timeout/5xx/retry、频道级授权拒绝等步骤）
# 插件 E2E 是 internal/superagentplugin 的 PG 测试（两轮 Writer SyncOnce + Import）。
KEEL_TEST_DSN='postgres://…' go test ./internal/superagentplugin/ -run TestBoardTaskToTicketToBoardWriteback -v

# Phase 2 最终门禁：独占全量 TUI、全量 Go、UI 截图/五态 HTML、文件覆盖表；
# 需要干净 tracked worktree，输出 evidence/phase-2-gates.md + phase-2-pin.txt。
KEEL_TEST_DSN='postgres://…' scripts/board-phase2-gates.sh
```

- **Phase 2 任务面 UI**：nsh `/board`（web/TUI 同源）是唯一任务面；旧 `/superagent` 页面、
  只读数据源与 `/v1/superagent` 只读/运营 API 已删除，旧脚本在
  `scripts/archive/retired-superagent/`。`targets`/可测量 lag/24h 失败计数在 board v0.1.1
  没有后端字段，UI 明确显示“未知”（见 change 的 `DISPOSITION.md`），不伪造成功。

- `GET /metrics` 同时输出 `board_*`（事件数、append 延迟、live claims、SSE 连接/滞后、
  sync 深度/失败、`board_sync_credential_status`）；标签只有 kind/key/platform/status，
  不含 tenant/消息 ID/secret。
- `GET /v1/board/admin/audit?limit=`（`board.manage`）读最近运维审计；领域审计在
  `board_events`，两者都不含 secret/正文。entry 生命周期另写
  `sync.entry.created|updated`（仅 key/platform/status/revision）。
- v0.1.1 SSE 对不可见事件写 `id:` 空帧（游标前进、不泄露内容），`Last-Event-ID` 优先于
  `after`；`last` 只重放可见事件。
- 已知接口缺口（v0.1.1）：board 无 `GET /me`（用 `/actors` 或消息 author 投影）；
  board 模块没有 entry POST/PATCH（keel 在挂载层补齐，未改 board 仓库）。
- 轮换/创建/路由变更的运行期语义是“持久化 + 停用 + 重启装载”，不是热替换：本进程的
  provider 仍持有旧凭据，keel 主动停用 board sync row 使 webhook 无副作用；重启后按新
  `secret_ref` 重建 provider。启停是即时的；禁用时已排队的 outbox 在重启/重新启用后继续。
- 自定义 `apiBaseUrl`/`bridgeUrl` 只接受 loopback、平台官方 host 或
  `KEEL_BOARD_PROVIDER_HOSTS` 中的 HTTPS host；请求体不能把平台 token 指向任意公网地址。
- 每个进程每 5s（`SyncMonitorInterval`）读取 tenant flags、durable entry 和凭据指纹：
  `board.enabled`/`board.sync.enabled` 关闭、entry 被停用、配置 revision 文档变化、或同名
  secret 被改写/删除时，inbound/outbound worker、board root-start 和 automation watcher
  都会暂停；重新打开 flags 后恢复。多副本共享同一 durable row，因此任一副本做出的停用/
  轮换会在一个监控周期内被其他副本观察，不依赖请求落在哪台。
- webhook、Chat SDK bridge、`POST /sync/threads`（attach）与 `GET /sources/{key}/read|join`
  在进入 board 处理前都做同一 durable 检查：disabled/缺失返回 404 `sync_disabled`，配置或
  凭据与启动快照不一致返回 503 `sync_pending_restart`；旧 provider 不可能通过这些路径
  继续工作。

## 遥测导出插件（`keel.telemetry`）

遥测是**默认关闭**的可选插件；配置 OTLP endpoint 不会自动出网。

```yaml
# tenant 文档（stored admin document；config.toml 不覆盖 telemetry）
telemetry:
  adapter: otlp                 # memory | none | otlp
  endpoint: https://collector.example:4318
  headers: { Authorization: "secret:OTLP_TOKEN" }  # 只允许 secret: 引用
  serviceName: keel
  signals: [traces, metrics]    # 省略=traces+metrics；logs 需显式开启
  sampleRatio: 1.0              # 0 只停 traces
  logRatio: 1.0                 # error 日志不受比例影响
  logMinLevel: info
```

启用（必须走 durable 审批；问题文本会展示 exporter/signals/endpoint host/采样/header 名）：

```bash
# 1) 打开审批问题
curl -sS -X POST "$KEEL_URL/v1/plugins/keel.telemetry/enable" \
  -H "X-Tenant-ID: $TENANT" -H "X-Keel-Actor: $USER" | tee /tmp/ask.json
# 2) 由持 plugin.lifecycle.enable 的人回答 allow（走 /v1/plugins/approvals/{decisionId}）
# 3) 带 decisionId 重放 enable
curl -sS -X POST "$KEEL_URL/v1/plugins/keel.telemetry/enable" \
  -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' \
  -d "{\"decisionId\":\"$(jq -r .decision.decisionId /tmp/ask.json)\"}"
# 状态（plugin.read）
curl -sS "$KEEL_URL/v1/telemetry/status" -H "X-Tenant-ID: $TENANT" | jq .
```

排障口径：

- `state=plugin_disabled`：插件未启用，配置是惰性的，零出网。
- `state=inactive`：插件已启用但 adapter 为 none/memory，无 exporter（合法）。
- `state=failed`：endpoint 非法、header 不是 `secret:` 引用或 secret 缺失；修复配置后重新
  enable，无需重启进程。`reason` 是字段级原因（不含 header 值/secret 名称）。明文 `http`
  endpoint 只接受 `localhost`、回环或私网 IP 字面量（内网 DNS 名请用 https）；redirect 不跟随。
- `state=degraded` + `lastErrorClass=http_5xx|http_429|timeout|network`：collector 不可用，
  队列满会丢最旧并在 `dropped` 计数；run/tool 不受影响。恢复后熔断半开自动探测。
- `metricSeriesDropped`：指标名未注册或 series 超过每族 1,000 / 全局 5,000；不是进程故障。
- 停用：`POST /v1/plugins/keel.telemetry/disable`，先注销 exporter 再 5s 有界 flush；
  bundled 插件不可卸载（409 `ErrPluginBundled`）。
- 关机：5s flush 期限内尽力导出；超时记录 `dropped` 后退出。没有持久队列：进程被 `kill -9`
  时未确认/未发出的批次直接丢失，重启不会重放；同一存活 exporter 的“已发送但响应丢失”重试
  可能让 collector 收到同一批次两次，collector 侧应按 (traceId, spanId) 幂等。
  这不是跨重启的 at-least-once（重启不补发），文档按 at-most-once across restart 执行。
- 配置变更审计：telemetry 配置从 tenant 文档读取，在下一次插件 Start（enable/重启）应用并写
  `configure` 审计；`PUT /v1/tenants/{id}/config` 本身是管理员文档写入，不逐字段写 telemetry
  审计。`configured=false` 表示租户完全没有 telemetry 段。
- 旧 `KEEL_TELEMETRY=memory` 仍只启用内存 recorder；OTLP 是否出网只取决于插件是否启用。

## 租户 provisioning

`POST /v1/tenants` 是管理员操作，必须带 `Authorization: Bearer <KEEL_ADMIN_KEY>`。
未配置 `KEEL_ADMIN_KEY` 时管理员 API 整体关闭（503）；缺少/错误 key 返回 401。
`X-Tenant-ID` 只指定新租户的 id，不是身份凭证。

```bash
export KEEL_ADMIN_KEY='<admin key>'
TENANT=$(uuidgen | tr '[:upper:]' '[:lower:]')
curl -sf -X POST http://127.0.0.1:13740/v1/tenants \
  -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
  -H "X-Tenant-ID: $TENANT" -H 'Content-Type: application/json' \
  -d '{"name":"tenant-a"}'
```

## 准入额度归还的已知取舍（陈旧释放可能瞬时超额）

`internal/kernel/lease.go` 的 `Finish` 与 `Release` 都用 `defer k.releaseAdmission(...)`
**无条件**归还租户 run 并发槽位（`internal/quota/limits.go` 的 `ReleaseRun`，计数钳在 0）。
这是「宁可瞬时超额，不永久饿死」的既定取舍，不是 bug：

- **现象**：旧 owner 的租约已被接管（过期/换班），新 owner 已持租并占用槽位；旧 owner
  此时才 Finish/Release，会给同一租户的 `active` 再减一。若紧接着有新 claim，`active`
  会低于真实在跑 run 数，`runConcurrency` 可被瞬时突破——上限＝在途陈旧 attempt 数
  （每个 attempt 只归还一次，`ReleaseRun` 在 0 处钳住，不会负溢出）。
- **取舍理由**：归还不能依赖 store 结果。陈旧、被 fencing 拒绝、store 报错的结算也必须
  释放本进程占用的槽位；否则崩溃重试/接管/放弃路径会把租户额度永久占死，后续 run 饿死
  （`internal/agent/worker.go` 的 abandon 路径也依赖这一点）。
- **影响面**：配额是**进程内**计数（重启归零），只影响本 runtime 的准入瞬时值；不影响
  租约/fencing/事件账本的持久正确性。要严格不超额得把租约计数放到数据库侧（跨进程
  权威），属后续加固，不在当前实现内。

运维口径：同租户短时在跑 run 数高于 `runConcurrency` 属预期窗口；真正的泄漏特征是
`active` 长期不回落且没有对应的在跑 run。

## MCP OAuth

MCP `oauth` 配置项的交互式授权、刷新与凭据存储。**fail-closed**：缺少必需配置时
相关功能直接关闭/返回 503，不会落明文，也不会回退到隐式共享租户。

```bash
export KEEL_SECRET_KEY='<32 字节 key 的 base64 或 hex>'   # 必填；不接受裸 32 字符口令
export KEEL_MCP_OAUTH_TENANT='<tenant uuid>'              # 必填；缺失则整个 OAuth 关闭
export KEEL_MCP_OAUTH_CALLBACK_URL='https://<host>/v1/mcp/callback'  # 必填；sign-in 端点开关
export KEEL_ADMIN_KEY='<admin key>'                       # 必填；sign-in 起点鉴权
# 可选
export KEEL_MCP_OAUTH_METADATA_BASE_URL='https://<host>/oauth'  # CIMD 文档基址；不设则 auto→DCR
export KEEL_MCP_OAUTH_ALLOWED_HOSTS='app.example,app2.example' # 仅在未设 CALLBACK_URL 时用于派生 Host
export KEEL_MCP_OAUTH_ALLOW_LOOPBACK=1                    # 仅本地 CLI helper；生产勿开
```

接口：

- `POST /v1/mcp/servers/{name}/signin`：`X-Tenant-ID` + `X-Admin-Key`（或
  `Authorization: Bearer`）；返回 `authorizationUrl` / `state` / `expiresAt`；
  已有可刷新 grant 时返回 `kind=authorized`。
- `GET /v1/mcp/callback`：浏览器回调（tenant/server/state 编码在 redirect URI 查询中，
  state 为 capability）；成功后自动重连目标 server。
- `POST /v1/mcp/callback`：宿主用 `{server, code, state, iss}` 或
  `{server, redirectUrl}`（粘贴地址）完成；重复回调幂等，进行中返回 409。

安全约束：凭据 AES-256-GCM（AAD=tenant+server，信封含版本与 key id），日志/错误对
token/verifier/client_secret 脱敏；OAuth 模式要求 MCP URL 为 https（仅 loopback 允许
http）；发现/注册/令牌请求走拒绝私网/回环的 SSRF 客户端；刷新锁使用独立连接池上的
PostgreSQL advisory lock（主池再小也不会死锁）。

运维口径：

- **pending 清理**：runtime 启动时立即清理一次 `mcp_oauth_pending`，之后每 10 分钟
  清理「已消费（终态）」与「已过期」且超过 1 小时保留窗口的行；未过期且未消费的浏览器
  步骤保留（回调仍需 `state` capability）。
- **锁池预算**：刷新锁池与主池物理隔离、按需建连（`MinConns=0`）、上限
  `store.Config.MCPOAuthLockConns`（默认 4）；锁连接 `statement_timeout=0`，等待由
  `lock_timeout`（默认 30s）约束。主池默认 24，连接总预算 = 主池 + 锁池。
- **challenge 可见性**：MCP server 返回 401 challenge 时 runtime 记录
  `mcp oauth challenge` 日志（server/error/scope/resource metadata URL），不记录 token。

## 测试

```bash
export KEEL_TEST_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
go test ./...
```

集成测试（blob/HTTP）在 `KEEL_TEST_DSN` 未设置时自动 skip。

## 无状态 E2E（真 SIGKILL）

```bash
go build -o /tmp/keel-e2e ./cmd/e2e
KEEL_DSN="$KEEL_TEST_DSN" /tmp/keel-e2e -bin /tmp/keel-runtime -port 13745
```

七个场景：requestId 幂等、SSE 游标、blob wire-offset 续传、agent 回复、模型调用中
SIGKILL、工具执行中 SIGKILL、100 会话 2000 事件无缺口。E2E 默认先 `TRUNCATE` 运行时表
（`-reset=false` 关闭），所以只能指向专用测试库。

## 压测

```bash
go build -o /tmp/keel-loadtest ./cmd/loadtest
/tmp/keel-loadtest -base http://127.0.0.1:13740 \
  -sessions 1000 -duration 60s \
  -text-every 10s -image-every 60s -image-size 1048576 -images 20 \
  -client-rate 51200 -out /tmp/bench.json
```

- `-client-rate` 是每流客户端读取上限；它决定延迟下界（1.4MB ÷ 50KB/s ≈ 27.3s）。
- 服务端必须用 `-streams 2000` 才能同时容纳 1000 条 SSE 与 800 条 Medium 图片流。
- 429 是预期反压：loadtest 会按 Retry-After 重试；60000ms 单轮只有 800 条 Medium
  容量，剩余 200 条会在 6 次退避后记为失败（这是容量上限，不是流错误）。
- 2026-11-06 历史基线：800/1000 图片完成、0 SSE 错误、18.6MB/s、2GB cgroup 峰值 544MB。
- 2026-11-08 终轮（`scripts/acceptance-perf.sh`，fail-closed 口径）：998/1000 图片完成
  （该轮 1853 张按尺寸分池 429 后退避重试）、0 硬错误、23.2MB/s、2GB cgroup 峰值 **640MB
  （29.8%）**；双进程 handoff（`scripts/acceptance-handoff-rss.sh`）峰值 656MB（32.0%）。
  单进程峰值各轮 485–640MB（随负载完成度浮动，均 ≤2GiB 的 32%），上限门 1.5GB。
- 结果 JSON 含 p50/p95/p99、吞吐、429 计数、RSS 采样。

## VmRSS 进程树内存门（`scripts/acceptance-rss.sh`）

口径（`openspec/changes/keel-harness-v2/acceptance.md` §0）：**后端进程树的
`VmRSS` 求和**，采样间隔 ≤10s，不含 page cache。判据：**>500MB 失败；300–500MB
通过但记「未达目标」（`withinTarget:false`）；≤300MB 达标**。cgroup
`memory.peak`（含 page cache）单列，只用于与 v1 基线对比，**不与 RSS 直接比较**。

```bash
# attach：给已在跑的后端采样（端口经 /v1/health 取 pid，并用 lsof 校验监听者）
scripts/acceptance-rss.sh --port 13740
scripts/acceptance-rss.sh --pid 12345 --duration 10s --out /tmp/keel-rss/attach.json

# profile：起一个原生 runtime 并跑 1000 会话 / 64 并发 run / 1MiB 图片通道，
# 采样窗口覆盖负载；RSS 与 workload 都真实才 PASS（约 2 分钟 + 独立库）
KEEL_TEST_DSN='postgres://…' scripts/acceptance-rss.sh --profile
```

- 退出码：`0` PASS（含 300–500MB 的 above-target 档）、`1` 越限或负载无效、
  `2` 不可测（无 procfs 且无 ps、目标消失、身份不匹配）。**没有「静默通过」路径**：
  取不到样本就是 2，不是 0。
- 平台：Linux 读 `/proc/<pid>/status:VmRSS`（进程表用 `ps` 建树，`ps` 缺失时仅在
  所有 `/proc` 行可读时降级为 proc-only，否则 fail-closed）；**macOS 无 procfs/
  `VmRSS`，用 `ps` 常驻集（KiB）作为平台等价读数**，summary 的 `source` 字段写明
  （`ps-rss` vs `proc-status`），不会把 ps 读数冒充 Linux VmRSS。
- 目标识别：`--port` 走后端自己的 `/v1/health` 取 pid，再用 `lsof` 交叉校验端口
  监听者；容器端口代理/外来进程不一致时直接 exit 2（容器内请用 `--pid`）。只对
  该 pid 及其后代求和，绝不把无关进程算进总量。
- `profile` 模式遇到不可用的 PostgreSQL（psql 缺失且 docker daemon 无响应）或端口
  占用会立即失败（docker/psql 探针带超时，不会挂住）；退出时回收 runtime、采样器
  与数据库，并核对端口已释放。`--cgroup <container>` 可把 `memory.peak` 作为**独立
  指标**附到 summary。
- 采样器自身子树（`processTree.excludedOverhead`，如 `python3` 采样器与 `ps`）不计入
  总量；采样节奏用绝对截止时间，慢 `ps` 不会累积漂移，实际间距报在
  `sampling.observedIntervalSec`。
- 机读 summary（`--out`，默认 `/tmp/keel-rss/summary.json`）：`verdict`、
  `exitCode`、`source`、`sampling`（间隔/实际间距/样本数/窗口/`stopReason`）、
  `processes`（每个成员的 pid/comm/RSS）、`processTree`（zombie/中途退出/被排除的
  采样器自身进程）、`totals`（peak/steady/p50）、`thresholds`
  （`ceilingMB`/`targetMB`/`withinTarget`）、`workloadCheck`（profile 模式）。
  W11 G4 报告直接引用其中的 `totals.peakMB`、`source` 与 `sampling`。
- 自测（无进程、无网络，喂合成 `/proc` 与 `ps` 表）：`scripts/acceptance-rss.sh --self-test`；
  CI 直接跑这条命令。
- `scripts/acceptance-all.sh` 里是**opt-in** 门：`KEEL_ACCEPTANCE_RSS=1` 跑 profile，
  `KEEL_ACCEPTANCE_RSS=attach`（配 `KEEL_RSS_PORT`/`KEEL_RSS_PID`）只采样已有进程；
  默认 SKIP，避免每次本地全量验收都多背一个 1000 会话负载。

## 验收脚本（一条命令复跑）

**推荐入口**：`scripts/acceptance-all.sh` —— 串行跑完全部验收门（build/vet、全仓
`-race`、conformance、R1（含同 dump 负控）、空库 E2E、排水、in-flight、网关冒烟、
chaos kill/handoff、2GB/1000 与双进程内存门），每个门的工件先截断再写、失败即停，
全部证据落在 `$KEEL_EVIDENCE_DIR`：

```bash
KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence \
  scripts/acceptance-all.sh

# 加上 VmRSS 进程树门（默认 SKIP；=1 跑 1000 会话 profile，=attach 只采已有进程）
KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence \
  KEEL_ACCEPTANCE_RSS=1 scripts/acceptance-all.sh
```

改内核/装配/handoff 之后，也可以按下面顺序手工复跑；这些脚本都自己建/删**隔离库**
（库名必须含 e2e/test/smoke/perf/accept，含防呆），跑完不留进程：

```bash
# 1) 重构平价：基线二进制 vs 候选二进制，同剧本规范化逐字比对
go build -o /tmp/keel-r1-cand ./cmd/runtime
go run ./cmd/r1diff -old-bin /tmp/keel-r1-base -new-bin /tmp/keel-r1-cand \
  -dsn 'postgres://...@127.0.0.1:13741/keel_r1diff?sslmode=disable' \
  -out /tmp/r1 -port-old 13782 -port-new 13783 \
  -allow openspec/changes/archive/keel-service-graph/r1-allow.json
# 2) SIGTERM 排水：健康探针可达 → 新写 503 → SSE drain 帧 → 进程退出 → 重启恰好一条回复
KEEL_DRAIN_BIN=/tmp/keel-runtime scripts/acceptance-drain.sh
# 3) claim 已发生后落账的输入必须被真正回答（工具调用恰好一次）
KEEL_INFLIGHT_BIN=/tmp/keel-runtime scripts/acceptance-inflight.sh
# 4) 2GB/1000 会话（隔离库 + --memory=2g + 峰值断言；429 是准入反压，hard error 必须 0）
bash scripts/acceptance-perf.sh 1000 60s
# 5) handoff 双进程并存（两 runtime + 1000 SSE + 中途换班；峰值上限断言）
bash scripts/acceptance-handoff-rss.sh
# 6) 10 分钟混沌（随机 SIGKILL / 接班即崩；每条输入必须被真正回答一次）
go build -o /tmp/keel-chaos ./cmd/chaos && go build -o /tmp/keel-runtime ./cmd/runtime
/tmp/keel-chaos -bin /tmp/keel-runtime -dsn 'postgres://…/keel_chaos_e2e?sslmode=disable' \
  -port 13795 -sessions 8 -duration 10m -kill-every 20s -mode kill
# R1 基线二进制由固定 commit 构建（不是 main：合并后 main 会移动）
bash scripts/build-r1-baseline.sh e7361f8 /tmp/keel-r1-base
```

**共享测试库纪律**：并发开发/测试期间**禁止**把会 TRUNCATE/DROP 的工具指向共享库
（`cmd/e2e -reset` 已加专用库防呆；`r1diff` 要求库名含 r1diff）。一条旧版 schema 的
迁移打到共享库上就足以删掉别的线程正在使用的行（实证：submissions 反向迁移事故）。

## 升级与迁移

- 迁移在启动时自动执行，事务内 `SET LOCAL statement_timeout=0`（DDL 成本由数据量决定），
  锁等待由 `lock_timeout=30s` 兜底并在 55P03 上自动重试。
- **首次带 kernel 索引的升级**会在大 `events` 表上 `CREATE INDEX`（持 SHARE 锁，期间
  append 阻塞；200k 行实测约 1s，量级线性）——请按维护窗口执行，先停旧进程再启新二进制
  （跨版本并存写者不被 fence，见 `docs/HANDOFF.md` 的割接纪律）。
- 迁移是增量的、幂等的；列的历史状态由迁移自身归一（DEFAULT + NOT NULL + 回填）。

## 2GB 硬限复跑

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/runtime-linux ./cmd/runtime
# 指南：用 scripts/acceptance-perf.sh（隔离库 + 峰值断言 + 库名防呆）。
# 若手工复跑，请指向专用库（名含 e2e/test），不要用共享 runtime 库：
docker run -d --name runtime-2g --memory=2g --memory-swap=2g --cpus=4 \
  -v /tmp/runtime-linux:/runtime:ro -v /tmp/cas:/cas \
  -e KEEL_DSN='postgres://postgres:runtime@host.docker.internal:13741/keel_perf_e2e?sslmode=disable' \
  -e GOMEMLIMIT=1610612736 \
  alpine:3.20 /runtime -addr 127.0.0.1:13750 -cas /cas -streams 2000

# 同 netns 的旁车跑压测，避开 Docker Desktop 端口转发的千连接限制
docker run --rm --network container:runtime-2g \
  -v /tmp/loadtest-linux:/loadtest:ro -v /tmp/out:/out alpine:3.20 \
  /loadtest -base http://127.0.0.1:13750 -sessions 1000 -duration 60s \
  -client-rate 51200 -image-size 1048576
```

`docker exec runtime-2g cat /sys/fs/cgroup/memory.peak` 读 cgroup 峰值（`memory.current` 是末次读数，会低估）。

## 清理

```bash
docker rm -f runtime-2g keel-runtime-pg
pkill -f /tmp/keel-runtime
```

## Prometheus 指标（GET /metrics）

`/metrics` 是抓取面（Prometheus text 0.0.4），`/v1/metrics` 的 JSON 保持给人和
既有客户端使用，两者并存。

```bash
curl -s http://127.0.0.1:13740/metrics | head
# 文本头：Content-Type: text/plain; version=0.0.4; charset=utf-8
```

固定基数指标（标签只有 `status`/`outcome` 等有限取值，**禁止** tenant/session/run 标签）：

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `keel_runs_started_total` | counter | 本进程认领的 run attempt 数（每次执行 +1） |
| `keel_runs_finished_total{status}` | counter | **终态** run：仅 FinishRun 成功提交 `done`/`failed` 后计数；失败但回到 pending 的重试不计入 |
| `keel_runs_failed_total` | counter | **终态** failed run（不含仍会重试的失败 attempt） |
| `keel_run_attempts_total{outcome}` | counter | 每次尝试的结局：`done`/`failed`/`pending`/`finish_error`/`abandoned`/`other` |
| `keel_run_attempt_duration_seconds` | histogram | 单次 attempt 墙钟耗时（含 pending/abandoned） |
| `keel_run_finish_errors_total` | counter | FinishRun 写库失败的次数（该 attempt 未计入终态） |
| `keel_runs_active` | gauge | 当前执行中的 run attempt |
| `keel_runs_pending` | gauge | `runs` 表 pending 队列深度（抓取时查库） |
| `keel_leases_expired_total` | counter | 过期租约被回收次数 |
| `keel_compactions_total` | counter | 已提交的 compaction 事件数 |
| `keel_checkpoint_created_total` | counter | 成功写入的 checkpoint 投影数（自动+手动） |
| `keel_checkpoint_recovered_total` | counter | 从有效 checkpoint + tail 重建模型上下文的次数 |
| `keel_checkpoint_failed_total{reason}` | counter | checkpoint 失败/降级；reason 固定为 `save`/`summary`/`load`/`corrupt`/`anchor`/`prune`/`conflict`/`policy`/`other` |
| `keel_checkpoint_pruned_total` | counter | 保留清理删除的 checkpoint 投影数（事件永不删除） |
| `keel_streams_active` | gauge | 当前 SSE/文件流数 |
| `keel_draining` | gauge | SIGTERM 排水期为 1 |
| `go_goroutines` / `go_memstats_heap_alloc_bytes` / `go_memstats_heap_sys_bytes` | gauge | Go 运行时 |
| `pgxpool_*` | gauge/counter | 连接池占用与累计统计（可选） |

僵尸 attempt（租约被接管或终态已提交）会被 FinishRun 的
`status='running'` + `attempts` 守卫拒绝（`ErrStaleRun`），worker 记
`abandoned` 且**不写终态**。

## Checkpoint（压缩存档点）

事件日志是唯一真值；checkpoint 只是压缩后的投影行，删除/清理它**永远不会删除
事件**。恢复路径是「最近有效 checkpoint 的 summary + `kept_from_seq` 之后的事件」，
重启、handoff 和 worker 恢复走同一条路径。

### 策略（tenant 文档 `compaction` 段）

| 字段 | 含义 | 0 值 |
| --- | --- | --- |
| `enabled` | 自动压缩开关（指针，显式 false 覆盖默认） | 继承 |
| `threshold` | 窗口占比阈值 (0,1] | 继承 |
| `keepRecentTokens` | 压缩后保留的近似 tail token | 默认 20000 |
| `thresholdTokens` | 绝对 token 阈值（优先于窗口占比） | 用窗口−reserve |
| `maxCheckpoints` | 单会话最多保留的 checkpoint 投影数 | 不限制 |
| `ttlHours` | 旧 checkpoint 投影过期小时数 | 不过期 |
| `manualCompact` | 是否允许手动压缩 | 允许 |

session 级只能**收窄** tenant 上限：不能开启 tenant 关闭的能力，不能超过
`maxCheckpoints`/`ttlHours`/`keepRecentTokens` 上限，不能比 tenant 更早触发压缩。
收窄由 `compaction.NarrowCheckpointPolicy` 计算并返回 `PolicyNarrowing`（仅字段名与
原因，不含正文）；设置页面接线后可直接呈现。

### 切点与失败

- 切点永远不落在 toolResult 上；最后一条尚未被模型看到的 user（其后再无可见
  assistant 回答）不会被压缩，必要时切点回退到该 user 或本次不压缩。
- 摘要请求失败或取消：当前上下文（事件日志）不变，不写 compaction 事件、
  不写 checkpoint，可直接重试。
- 成功时先提交 `compaction` 事件（owner-scoped，需要 run lease），再写
  `checkpoints` 投影。投影写失败只记 WARN，事件保留，回放退化为全量日志。

### 恢复与损坏

回放前会校验投影：`upto_seq` 必须指向一条已提交的 `compaction` 事件、
`kept_from_seq` 必须是已提交事件的 seq、summary 非空、锚点小于 `upto_seq`。
任一不满足即 fail closed：忽略该投影并**回退全量事件重建**，日志记录
`checkpoint unusable; rebuilding from the full event log`（只有 ids/原因，不含正文）。

### 保留清理

`maxCheckpoints`/`ttlHours` 清理只删 `checkpoints` 行，**最近一个 checkpoint
永远保留**（它是当前会话唯一恢复锚点），直到被更新的 checkpoint 取代；事件与
历史全文始终可按 seq 读取。清理失败只记 WARN，不影响 run。

### 手动压缩 API

```
POST /v1/sessions/{id}/compact
X-Tenant-ID: <tenant>
{"requestId": "compact-2026-11-25-1"}
```

- 需要 session 写权限（`session.write`），否则 403 且不读取上下文；
- `requestId` 幂等：重复请求返回同一个 `checkpointId`（`replayed=true`），不再调用模型；
- 返回 `checkpointId`（compaction 事件 seq）、`tokensBefore/tokensAfter`、
  `keptFromSeq`、`policyVersion`、`performed/replayed`；
- 与自动压缩共用切点/失败语义：未读输入按自动路径重排到最后，切点若会覆盖任何
  未读输入、落在 fork 前缀内或无法锚定到子会话事件，返回 409 `conflict` 且不写事件；
  悬空 assistant tool call 同样 409 `conflict`（交给正常 attempt 收尾）。
- 摘要请求使用本次 attempt 解析出的模型（selection/binding、provider stream、动态
  key、thinking、context window、request patch）；解析失败 fail closed，不落假摘要。
- 摘要失败或取消时不写任何东西。
- 生产 `keel serve` 已接线：适配器先取得 session 的 run lease（无 run 行时入队一次），
  摘要期间续租，成功后 finish（未读输入照常重新入队），失败则 release；
  活动 run 持有租约时返回 409 `busy`，租户并发配额拒绝时返回 429。
- 策略 `manualCompact=false` 时返回 409 `disabled`；`enabled` 只控制自动压缩。

### session 级策略 API

```
GET /v1/sessions/{id}/checkpoint-policy
PUT /v1/sessions/{id}/checkpoint-policy
X-Tenant-ID: <tenant>
{"maxCheckpoints": 1, "ttlHours": 12, "retainTokens": 5000,
 "thresholdTokens": 80000, "enabled": true, "manualCompact": false}
```

- GET 需要 `session.read`，PUT 需要 `session.write`；
- 返回 `tenant`（tenant 上限）、`effective`（收窄后的生效值）、`override`（已存
  的 session 文档，可省略）、`narrowed`（被 tenant 收窄的字段与原因）、`revision`
  和 `source`（tenant|session）；
- session 只能收窄：超过 tenant 的 `maxCheckpoints`/`ttlHours`/`retainTokens` 会被
  夹到 tenant 值并在 `narrowed` 中说明；tenant 关闭的能力不能被 session 打开；
  tenant 设了绝对 `thresholdTokens` 时 session 不能回退到更早的窗口触发；
- PUT `{}` 清除 override；负数返回 400；响应与日志都不含对话正文。
- 当前只提供 API 与只读投影，设置页 UI 尚未接入（后续 change）。

MCP 工具指标当前**不暴露**：`internal/mcp` 尚无调用计数，注册空 family 只会产生
没有样本的 `# HELP`/`# TYPE`。接入计数后按 `tool`/`status` 重新加入。

抓取示例（Prometheus）：

```yaml
scrape_configs:
  - job_name: keel
    scrape_interval: 15s
    static_configs:
      - targets: ["127.0.0.1:13740"]
```

队列/连接池指标在抓取时求值（分别带 500ms 超时）；单条采集失败只会跳过该
样本并打一条 `metrics collect` 警告，不影响其余指标。series 数与会话数无关。

**安全**：`/metrics`（以及 `/v1/health`、`/v1/ready`）**没有鉴权**，会泄露运行
规模与队列深度；只应绑定内网/回环，公网入口需在反向代理层限制来源。

## 就绪探针（GET /v1/ready）

- `GET /v1/health`：进程存活 + goroutine/heap（永远 200，用于 liveness）。
- `GET /v1/ready`：PG `Ping` + 运行 schema 检查（表与迁移列），未就绪返回
  **503 + Retry-After**；排水期间也返回 503（`"draining": true`），让 LB 摘实例。
  检查覆盖 store 基础表、kernel 0001/0003/0004/0005 列（lease/fencing、loadout pin、
  pending_input/read_watermark、loadout_generation/loadout_owner_epoch）、sessions 的
  external 标记列，以及 handoff 的 `runtime_instances`/`handoffs` 表；旧库缺任一列/表即
  503，不会误报 ready。
  响应体不包含内部主机/表名，细节只进日志。

```bash
curl -i http://127.0.0.1:13740/v1/ready
```

Kubernetes 探针建议：liveness → `/v1/health`（initialDelay 10s），readiness →
`/v1/ready`（period 5s，timeout 3s，failureThreshold 3）。

## SIGTERM 排水

- **SIGINT**：保持旧行为，立即取消并关闭。
- **SIGTERM**：先进入 draining（新 API 请求一律 `503 + Retry-After`，`/v1/health`、
  `/v1/ready`、`/metrics` 例外），worker 停止认领新 run；每条已建立的 SSE 连接先
  补发未读事件，再收到：

  ```
  event: drain
  data: {"retry_after_ms":3000}
  ```

  客户端应按该提示重连（配合 `Last-Event-ID` 续传，事件不丢）。连接随后按速率
  上限关闭；排水总时长约为
  `max(-drain-window, active_streams / -drain-close-rate) + Shutdown(≤10s)`。
  被拦截的新请求会以结构化日志记录（`route="draining"`、`draining=true`、status 503）。

  排水只覆盖 SSE：**长文件下载不参与 drain**，会继续传输并在 window 用尽后随
  Shutdown 断开；客户端用 Range / base64 offset 续传。在途 run 在排水开始时停止
  认领并被取消，租约到期后由其他实例（或重启后的本进程）回收，事件日志不丢。

参数（flag / 环境变量）：

| flag | env | 默认 | 含义 |
| --- | --- | --- | --- |
| `-drain-retry-ms` | `KEEL_DRAIN_RETRY_MS` | 3000 | drain 事件里的重连提示 |
| `-drain-close-rate` | `KEEL_DRAIN_CLOSE_RATE` | 200 | 每秒关闭的流数 |
| `-drain-window` | `KEEL_DRAIN_WINDOW` | 5s | 排水等待上限 |

1000 条 SSE 在默认 200/s 下约 5s 关闭；编排平台应给 `stop_grace_period` ≥ 20s
（compose 文件已设为 20s）。

## 容器镜像与 compose

```bash
# 构建（distroless static、非 root、内置 HEALTHCHECK）
docker build -t keel:local .

# PG18 + Keel（默认只绑回环：Keel 127.0.0.1:13740，PG 127.0.0.1:13741）
docker compose up -d --build
docker compose ps
curl -sf http://127.0.0.1:13740/v1/ready && echo ready

# 查看健康检查与排水
docker inspect --format '{{.State.Health.Status}}' keel-keel-1
docker stop keel-keel-1   # SIGTERM -> drain -> 关闭
```

- 镜像内以 `nonroot` 运行，CAS 使用 `/data/cas` 命名卷（镜像预创建该目录并带
  ownership，新卷继承后可写）；容器根文件系统只读，`/tmp` 为 64MB tmpfs。
- `HEALTHCHECK` 调用二进制自带的 `-healthcheck`（探测 `-healthcheck-addr`，否则
  `KEEL_HEALTH_ADDR`，否则由 `-addr` 推导），不依赖 shell/curl。它是**liveness**：
  只证明进程 HTTP 还活着，**不反映 PG/迁移状态**；依赖就绪请用 `/v1/ready`。
- compose 默认未设 `KEEL_MODEL`，即使用确定性 **fake provider**（无需凭据）；
  接真实模型时注入 `KEEL_MODEL`/`KEEL_BASE_URL`/`KEEL_API_KEY`。
- DSN 由 compose 通过 `KEEL_DSN` 注入；生产请替换密码并放到 secret 管理。

## CI

`.github/workflows/ci.yml`：

- `gofmt -l .` 零输出、`go vet ./...`、`go test -race ./...`（GitHub Actions
  `postgres:18` service，注入 `KEEL_TEST_DSN`）、`go build ./...`、
  `scripts/acceptance-rss.sh --self-test`（VmRSS 解析/求和/阈值夹具，不启动进程）；
- `image` job：`docker build -t keel:ci .`；
- `chaos-nightly`：schedule 触发，当前为 TODO 占位（随机 kill 10 分钟场景待接）。

本地等价门：

```bash
gofmt -l .
go vet ./...
KEEL_TEST_DSN="$KEEL_TEST_DSN" go test -race ./...
go build ./...
```

