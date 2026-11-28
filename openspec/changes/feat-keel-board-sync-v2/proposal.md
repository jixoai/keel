# Proposal: feat-keel-board-sync-v2

> 立项：2026-11-28。基线：keel `origin/main` `2c1245e9`（v0.5.0 后）。
> 承接：已归档 `keel-board-v1` 的 14.2 / 15.5 未完成项、`DISPOSITION.md` 的 P2 #8/#9/#10
> 与 dead-code 清理。不重新打开 `keel-board-v1`。
> 依赖：`github.com/jixoai/keel-board` v0.1.2（本 change 同批实现并打 tag）。

## Why

`keel-board-v1` Phase 2 已把 board 做成唯一任务面，但 sync 健康页仍有三个字段只能显示
“后端未上报”：每个 sync entry 的外部 targets、最老 pending inbox/outbox 的 lag/age、
24h 失败数。board v0.1.1 只返回计数，没有 target 列表、时间戳或 24h 聚合；keel 的
mount 层只能把 `lag` 明确写成 `null`，不能把未知伪装成 0。watcher 的 `dead`/`lastError`
同样没有 DTO，导致死信重试入口在自动化页隐藏。

同时 Phase 2 复核留下几项 P2：390 状态 tabs 不会自动滚入、短屏筛选 sheet footer 未换行、
provider 错误只有压缩后的类别、移动端隐藏了复制链接、schedule 的“下次运行”只有时分秒、
takeover/forceRelease 的客户端死代码仍占据动作表。

本 change 用 board v0.1.2 的只读健康 API 把上述数据变成真实值；未知仍然是未知，绝不
用 fixture 形状或 0 冒充。

## What Changes

### 1. board v0.1.2（独立仓库，只读）

- 新增 `GET /v1/board/sync/entries/{key}/targets`：按 sync entry 枚举
  `board_sync_threads` 的外部 target，带 board root、pending/leased/dead、attempts、
  lastError、undelivered；现有 `GET /sync/threads?thread=` 继续按 board thread 枚举。
- 状态 DTO 增加：
  - `targets[]`（同一 entry 的目标快照，bounded）；
  - `lag`：`{inbox: {pending, oldestAt, ageSeconds} | null, outbox: ... | null}`；
    `null` 表示该队列当前没有 outstanding 行，**不是**测量失败；
  - `failures24h`：该 entry 最近 24h 死信出站投递数。
- `/sync/status` 的每个 key、`/sync/entries` 与 `/sync/entries/{key}/status` 使用同一组
  health 事实；root status 给出跨 entry 聚合。
- watcher DTO 增加 `dead`、`lastError`、`lastDeliveryAt`（来自
  `board_watch_deliveries`），使自动化页的 dead/lastError 不再只能隐藏。
- 协议仍是只读管理面，继续要求 `board.manage`；target 只含外部 ID，不返回正文、凭据或
  平台原始错误 body（`lastError` 仍是稳定错误码）。

### 2. keel 挂载与 UI

- `go.mod` 固定 `github.com/jixoai/keel-board v0.1.2`。
- `internal/boardintegration` 的 augment 不再覆盖 board 已提供的 `lag`/`targets`/
  `failures24h`；只有字段缺失时保留 `null`（真实未知）。
- `web/src/data` 解析并缓存 targets/lag/24h；同步页显示真实值：lag 秒/分钟、24h 失败、
  目标列表与 undelivered；无积压显示“无积压”，未知显示“未知/未上报”，不写 0。
- watcher 行显示真实 dead/lastError/lastDeliveryAt；重试死信入口由真实 dead 驱动。

### 3. P2 同批收口（keel UI）

- 390 状态 tabs 选中项自动滚入视口；短屏筛选 Sheet footer 可换行，不溢出。
- provider 错误按产品口径给出“类别 + 下一步”，原始 provider sentence 不进入 UI；
  服务端日志保留原文。
- 移动端恢复任务复制链接（44px 命中区）。
- schedule 的“下次运行”显示日期（`MM-DD HH:MM`，今天可只显示时间）；取消定时任务使用
  危险动作权重并经 AlertDialog 二次确认。
- 删除 `takeover` / `release.force` 的客户端死代码（API 方法、fixture、move 表、标签），
  不改变服务端契约；`answer` 保留。

## Impact

- 子仓：`keel-board` 独立分支 + `v0.1.2` tag；keel 依赖升级。
- keel 代码：`internal/boardintegration`（augment/DTO 测试）、`web/src/data`、
  `web/src/screens/board/{sync,automation,queue,task}.tsx`、kit `Tab`/`Sheet` 可访问性。
- 数据面：board `syncstore` 新增只读查询（memory + PostgreSQL）；无 schema 迁移。
- 权限：不变，仍为 `board.manage`；无新增写端点。
- 兼容：Alpha 零兼容；客户端不再保留 v0.1.1 的 target/lag 伪字段兼容垫片，只用
  `undefined`/`null` 表达缺失。

## Non-goals

- 不新增 sync 写动作、不实现 takeover/forceRelease 服务端端点。
- 不返回平台消息正文、凭据、原始 provider body 或 connector secret。
- 不在本 change 跑全量 TUI（由最终集成批次执行）；本 change 只跑定向 board/TUI 用例。
- 不重构 board 日志/投影契约，不改 claim/needs 语义。
