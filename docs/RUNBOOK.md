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

四个场景：requestId 幂等、SSE 游标、blob wire-offset 续传、100 会话 2000 事件无缺口。

## 压测

```bash
go build -o /tmp/keel-loadtest ./cmd/loadtest
/tmp/keel-loadtest -base http://127.0.0.1:13740 \
  -sessions 1000 -duration 120s \
  -text-every 10s -image-every 30s -image-size 1048576 -images 20 \
  -client-rate 102400 -out /tmp/bench.json
```

- `-client-rate` 是每流客户端读取上限；它决定延迟下界（1.4MB ÷ 100KB/s ≈ 14.3s）。
- 429 是预期反压：loadtest 会按 Retry-After 重试。
- 结果 JSON 含 p50/p95/p99、吞吐、429 计数、RSS 采样。

## 2GB 硬限复跑

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/runtime-linux ./cmd/runtime
docker run -d --name runtime-2g --memory=2g --cpus=2 -p 13748:13740 \
  -v /tmp/runtime-linux:/runtime:ro -v /tmp/cas:/cas \
  -e KEEL_DSN='postgres://postgres:runtime@host.docker.internal:13741/runtime?sslmode=disable' \
  -e GOMEMLIMIT=1610612736 \
  alpine:3.20 /runtime -addr :13740 -cas /cas -streams 2200

# 同 netns 的旁车跑压测，避开 Docker Desktop 端口转发的千连接限制
docker run --rm --network container:runtime-2g \
  -v /tmp/loadtest-linux:/loadtest:ro -v /tmp/out:/out alpine:3.20 \
  /loadtest -base http://127.0.0.1:13740 -sessions 1000 -duration 60s ...
```

`docker exec runtime-2g cat /sys/fs/cgroup/memory.current` 采样 cgroup 内存。

## 清理

```bash
docker rm -f runtime-2g keel-runtime-pg
pkill -f /tmp/keel-runtime
```
