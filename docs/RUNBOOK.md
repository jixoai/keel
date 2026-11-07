# 运行手册

## 本地起 PostgreSQL

```bash
docker run -d --name keel-runtime-pg \
  -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 --memory=2g --cpus=2 postgres:18
```

## 起服务

```bash
export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
go build -o /tmp/keel-runtime ./cmd/runtime
GOMEMLIMIT=1610612736 /tmp/keel-runtime \
  -addr 127.0.0.1:13740 -cas /tmp/keel-cas \
  -streams 2200 -chunk-batch 1 -admit-rate 1200
```

- `-streams` 是总并发流上限；1000 会话需要 ≥1000（SSE）+ 图片流余量，建议 1600–2200。
- `-chunk-batch 1` ≈150KB/流；`2` 是 1Gbps 模式（PG 查询减半）。
- `-bandwidth` 是全局出站字节/秒（0=不限）；生产按链路 85–90% 设。
- `-cas` 必须与上传目录一致，否则 >1MiB 的图读不出来。

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
  （另 214 张按尺寸分池 429 后退避重试）、0 硬错误、23.2MB/s、2GB cgroup 峰值 **590MB
  （27.5%）**；双进程 handoff（`scripts/acceptance-handoff-rss.sh`）峰值 584MB（27.2%）。
  单进程峰值各轮 485–590MB（随负载完成度浮动，均 ≤2GiB 的 29%），上限门 1.5GB。
- 结果 JSON 含 p50/p95/p99、吞吐、429 计数、RSS 采样。

## 验收脚本（一条命令复跑）

**推荐入口**：`scripts/acceptance-all.sh` —— 串行跑完全部验收门（build/vet、全仓
`-race`、conformance、R1（含同 dump 负控）、空库 E2E、排水、in-flight、网关冒烟、
chaos kill/handoff、2GB/1000 与双进程内存门），每个门的工件先截断再写、失败即停，
全部证据落在 `$KEEL_EVIDENCE_DIR`：

```bash
KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence \
  scripts/acceptance-all.sh
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
| `keel_streams_active` | gauge | 当前 SSE/文件流数 |
| `keel_draining` | gauge | SIGTERM 排水期为 1 |
| `go_goroutines` / `go_memstats_heap_alloc_bytes` / `go_memstats_heap_sys_bytes` | gauge | Go 运行时 |
| `pgxpool_*` | gauge/counter | 连接池占用与累计统计（可选） |

僵尸 attempt（租约被接管或终态已提交）会被 FinishRun 的
`status='running'` + `attempts` 守卫拒绝（`ErrStaleRun`），worker 记
`abandoned` 且**不写终态**。

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
  `postgres:18` service，注入 `KEEL_TEST_DSN`）、`go build ./...`；
- `image` job：`docker build -t keel:ci .`；
- `chaos-nightly`：schedule 触发，当前为 TODO 占位（随机 kill 10 分钟场景待接）。

本地等价门：

```bash
gofmt -l .
go vet ./...
KEEL_TEST_DSN="$KEEL_TEST_DSN" go test -race ./...
go build ./...
```

