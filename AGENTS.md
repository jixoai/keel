# Keel（keel）项目指令

> 本文件是项目级指令，补充（不覆盖）`~/.agents/AGENTS.md` 与 `~/.zcode/AGENTS.md` 的全局规则。
> 本仓＝**Keel（龙骨）· 通用 Agent 无状态运行时底座**，不绑定任何具体产品。

## 原则：一切任务用 OpenSpec 履行

任何任务/需求，都必须以 openspec change 履行：先建 change（proposal + tasks +
specs 增量），再实现，再按 change 验收。change 全部任务完成且验证通过后立即归档。

## 原则：无状态是宪法（Owner 2026-11-05）

- 应用进程**不持有会话状态**：事件日志是唯一真值，checkpoint 是投影，进程只是执行壳。
- 每个 await 点可重入；每个外部效果有幂等键；每个未答问句有 qid；断线用 `Last-Event-ID`/offset 续传。
- 验收必须包含 `kill -9` → 重启 → 会话继续、不丢不重；不允许只测 happy path。

## 原则：流式与内存纪律

- **绝不允许整图进内存**：`base64.NewEncoder` + `io.Copy` + 32KB `bufio`；禁止 `EncodeToString`。
- 图片流每流缓冲 ≤64KB；1000 流预算 ≤100MB。
- `GOMEMLIMIT=1.6GiB`、`GOGC=50–100`；RSS 曲线必须进验收报告。

## 原则：网络反压优先于磁盘/内存

- 全局带宽预算（1GbE 的 70–80%）；按请求尺寸分池（小请求快车道、大请求慢车道）；
- 有界 worker：网络饱和时**停止从存储拉取**，不得继续开新读；
- 慢客户端：写超时/进度超时 → cancel → 保留 offset 以便续传；
- 重试预算 + 指数退避 + 抖动，禁止 1000 个请求同时重试。

## 原则：多租户是逻辑隔离

- 单库 + `tenant_id` + 复合索引；RLS 可选（FORCE + 非 owner 角色）；
- 不做 schema-per-tenant、database-per-tenant；不做每用户进程（2GB 不可行）；
- 每租户配额（并发、带宽、存储）在应用层与 PG 层双保险。

## 原则：上游代码搬运纪律

- pigo 代码按模块搬运，保留 MIT 版权头与来源注释；记录 pin commit（`docs/UPSTREAM.md`）。
- 搬运后按本仓风格改造；不跟随上游升级，不引入上游 CLI/TUI/session/trust 依赖。

## 原则：Go 版本与安全

- 固定 **Go 1.27.3**（最新稳定）；`go.mod` 写 `go 1.27.3`；CI 使用同一版本。
- 依赖最小化；新增依赖须说明内存/安全影响。
