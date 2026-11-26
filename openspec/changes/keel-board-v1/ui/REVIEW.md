# keel-board-v1 · Phase 2 任务面设计自检（复审修复轮）

> 状态：**设计稿 / 可交互原型，P0 决策已落实，P1 已修，待实现与独立视觉评审**。
> 本切片不改生产代码。设计正文：[`DESIGN.md`](DESIGN.md)；
> 原型：[`prototype/index.html`](prototype/index.html)；截图：[`prototype/shots/`](prototype/shots/)。
> 基线：keel `main@8b9c1e2`；change `keel-board-v1`。

## 1. 结论

- 上一轮 4 个 P0 已按 Owner 决定写进 DESIGN 与原型：claim 丢失恢复、`board.read` 迁移、
  归档只读、服务端 `moves` 权威动作。
- 上一轮 P1 清单已全部修复（§4；loading 与 390 骨架拆成两条），并补了浅色五态、1024/1180、无 board.read 404、
  TUI 线框/键位/焦点/smoke 清单。
- 证据重生成：**92 张**真实 Chrome/Playwright 截图，92/92 无横向溢出、无 console/page error，
  92 个 SHA1 全部不同；`shots/manifest.json` 与 `interact.log` 可复核。
- 仍未验证/未交付：真实 nsh TSX/TUI 渲染、真实后端、独立视觉评审、真机安全区、生产 token 改动。
  本文件不把这些写成“已通过”。

## 2. 交付物

| 文件 | 说明 |
| --- | --- |
| `ui/DESIGN.md` | IA、路由、组件树、状态模型、moves 动作矩阵、权限/403/404、五态、键盘、TUI 线框、响应式、sync/自动化故障、归档/迁移、退役、契约依赖 |
| `ui/prototype/index.html` | 自包含原型（内嵌 CSS/JS，零构建、零运行时依赖） |
| `ui/prototype/shoot.mjs` | 截图矩阵 + `shots/manifest.json` 生成；逐 URL 记录 bytes/sha1/overflow/errors |
| `ui/prototype/interact.mjs` | 20 组原型交互断言；生成 `interact.log` |
| `ui/prototype/shots/*.png` | 92 张：1440×1000（63）、768×1000（6）、390×844（19）、1024×1000（2）、1180×1000（2） |
| `ui/prototype/shots/manifest.json` | 每张图的 name/query/width/height/bytes/sha1/overflow/errors |
| `ui/prototype/interact.log` | 20 条 PASS、生成时间、Playwright 路径、errors=0 |
| `ui/REVIEW.md` | 本文件 |

## 3. P0 决策落实

| P0 | 决定 | 设计落点 | 原型/证据 |
| --- | --- | --- | --- |
| claim 丢失 | owner/manage **强制 release**（无 token，写审计）+ 同 Actor **takeover** | `DESIGN.md` §6.1/§6.2/§7.4/§9.2/§15；TaskView 依赖表 | `task-1440-claim-lost.png`、`task-1440-dialog-force-release.png`；交互断言 takeover + force-release |
| `task.read`→`board.read` | 启用时默认角色 bootstrap `board.read`；发布公告；legacy 显示迁移页，不静默隐藏 | §2.1/§8.1/§13.3/§15 | `queue-1440-migrate.png`、`queue-390-migrate.png`；交互断言迁移页 |
| 归档 Ticket | board 提供只读归档任务（`archived=true` + `source=superagent` 筛选），无动作；适配器 MUST NOT 跳过 | §4.1.10/§13.2/§15 G11 | `queue-1440-archive.png`、`task-1440-archived.png`、`task-390-archived.png`；交互断言归档无动作 |
| 权威动作 | TaskView 返回 `moves[]` 与 `mirror {source,readOnly,bridge}`；UI 只渲染 moves，禁止按 source/标签推断 | §6.1/§7.1/§15 G1 | 原型 `fixtureMoves` 只作服务端替身；`taskActions` 只读 `t.moves`/`t.mirror`；无 source 分支 |

**新增的 API/契约依赖（P0 已决，必须在 keel-board/keel 实现）**：

- `POST /v1/board/tasks/{id}/release {force:true}`（owner/manage，无 epoch/token，写审计）。
- `POST /v1/board/tasks/{id}/takeover`（同 Actor，epoch+1，响应含新 token）。
- TaskView：`moves[]`、`mirror{source,readOnly,bridge}`、`archived`、`origin{platform,ref}`。
- `GET /tasks?archived=1&label=source=superagent&state=all`（归档只读筛选）。
- 租户启用 board 时默认角色 bootstrap `board.read`；legacy 页面。

## 4. P1 修复清单

| P1 | 修复 | 证据 |
| --- | --- | --- |
| loading 保留 tabs/filter | 队列 loading 显示 tabs + filter（计数隐藏）；390 3 行 × 3 段骨架 | `queue-1440-loading.png`、`queue-390-loading.png`；交互断言 |
| 390 三行骨架 | 移动端 skeleton 每行 3 段（ID+状态 / 标题 / 元信息），只显示 3 行 | `queue-390-loading.png` |
| sync/automation 空态 badge 归零 | empty 时 `syncIssueCount`/`autoIssueCount` 返回 0 | `sync-1440-empty.png`、`automation-1440-empty.png`；交互断言 |
| 非队列错误保留 view tabs | sync/automation loading/error 渲染 view tabs | `sync-1440-error.png`、`automation-1440-error.png` |
| readonly 不显示“本标签页持有” | lease label 仅当 `moves` 含 `complete`/`renew` 且 token 在本页；read-only 显示持有者 | `task-1440-readonly.png`；交互断言 |
| assigned ready 文案 | 行 `已指派 / 仅限 X`；任务页 `已指派 · 仅限 X` | `queue-1440-success.png`、`task-1440-ready`/`#824`；交互断言 |
| stale 不显示“实时更新” | stale 队列右列 `最后更新 14:02:11`；header `重连中` | `queue-1440-stale.png`；交互断言 |
| dark destructive token | 原型 dark `destructive-foreground:#171717`（5.92:1）；生产 `web/theme.json` 需同步改 | 对比度表 §5.2；`task-1440-dialog-force-release.png` |
| 手机 tabs/chips/switch ≥44 | view tabs/state tabs/chips/归档/switch 全部 44px；switch 44×44 命中区 | 交互断言实测 44；`queue-390-success.png`、`sync-390.png` |
| 镜像只读入口按事实 | `taskActions` 只看 `moves`/`mirror.readOnly`；不看 `source=` | `task-1440-mirror-readonly.png` |
| NewTask 6 字段 | 频道/说明/标签/依赖/指派/立即认领；手机全屏 100vh | `task-1440-dialog-new.png`、`task-390-dialog-new.png`；交互断言 |
| TUI 线框/键位/焦点/smoke | DESIGN §12.4 80×24/120×40 线框、焦点模型、8 条 smoke；**未执行** | DESIGN §12.4；§8 未验证 |
| 访问 Sheet 无假编辑 | 无 API 时只显示关系与“不能在这里授权”，无输入/添加行 | `task-1440-dialog-access.png`；交互断言 |
| 动作菜单由矩阵生成 | `MOVE_META` + `taskActions` 由 `moves` 生成 primary/secondary/overflow | 原型代码；所有任务帧 |
| 768 折叠次要信息 | ≤1179 只留 频道/创建者；`详情` 展开事实/标签/外部副本；线程进首屏 | `task-768-claimed.png`、`task-1024.png`、`task-1180.png` |

## 5. 视觉与可测量自检

### 5.1 排版/间距/材质

| 项 | 结果 |
| --- | --- |
| 字号阶梯 | 30/20 标题 > 14 正文 > 12 辅助 > 11 mono 数据；无倒挂 |
| 正文行宽 | ≤72ch；任务根/回复不铺满 1440 |
| 间距 | 4/8/12/16/20/24/32/40；6/2 仅密集行 |
| 控件 | 桌面 34，手机 44；行高 68–104 |
| 材质 | Solid-glass rim→inner→edge→shade 复现；无手写 hex 组件色 |
| 图标 | lucide 24×24/1.8pt 单色；状态均有文字 |
| 焦点 | 单一 `outline 2px foreground 65%`；实测 5.82:1 |

### 5.2 对比度（实测）

| 对 | 实测 | 门限 | 结果 |
| --- | --- | --- | --- |
| dark foreground / background | 12.21 | 4.5 | PASS |
| dark muted / background | 8.83 | 4.5 | PASS |
| dark foreground / card | 9.29 | 4.5 | PASS |
| dark muted / card | 6.72 | 4.5 | PASS |
| dark foreground / popover | 9.42 | 4.5 | PASS |
| dark primary-fg / primary | 16.01 | 4.5 | PASS |
| dark fg / success/warning/destructive/info tint 16% | 9.31/9.33/9.94/9.70 | 4.5 | PASS |
| dark link / background | 9.24 | 4.5 | PASS |
| dark focus ring（fg 65%）/ background | 5.82 | 3 | PASS |
| **dark destructive-fg / destructive** | **5.92** | 4.5 | **PASS（本轮修复）** |
| light foreground / background | 8.86 | 4.5 | PASS |
| **light muted / background** | **4.90** | 4.5 | **PASS（本轮修复）** |
| light muted / card | 4.68 | 4.5 | PASS |
| light foreground / popover | 9.93 | 4.5 | PASS |
| light primary-fg / primary | 15.93 | 4.5 | PASS |
| light destructive-fg / destructive | 4.83 | 4.5 | PASS |

- 原型 tokens 已改为 dark destructive `#171717`、light muted `#666666`；**生产 `web/theme.json` 仍是
  旧值**，实现时必须同步改 token，否则上线仍是 3.03/4.36。这是本轮唯一未落到生产文件的 token 修复。

## 6. 五态与新增页面

| 界面 | 帧 |
| --- | --- |
| 队列 success/loading/empty/filtered/error/noread-404/disabled/migrate/stale/readonly/archive | `queue-1440-*.png`、`queue-390-*.png` |
| 浅色五态 | `queue-1440-light-success/loading/empty/error/noread-404.png`、`task-1440-light-claimed.png` |
| 任务七态 + claim lost + mirror + archived + read-only | `task-1440-*.png` |
| 动作对话框（complete/fail/block/assign/needs/cancel/release/reopen/answer/unblock/force-release/new/access/shortcuts） | `task-1440-dialog-*.png` |
| 同步/自动化五态 + 死信/凭据失效/失败 | `sync-1440-*.png`、`automation-1440-*.png` |
| 1024/1180 | `queue-1024/task-1024/queue-1180/task-1180` |
| 旧地址 | `gone-1440.png`、`gone-390.png` |
| 390 手机 | `queue-390-*`、`task-390-*`、`sync-390`、`automation-390`、`gone-390` |

- 无 board.read：`queue-1440-noread-404.png`（404 文案，不泄露原因）；legacy：`queue-1440-migrate.png`。
- 归档：`queue-1440-archive.png`、`task-1440-archived.png`；无动作、无 Composer。
- 旧 `/superagent` 仍是 404 下线页，不重定向。

## 7. 证据与可复算

### 7.1 截图

- 构建身份：`prototype/index.html` SHA-256 `46f6b0261846e63cf0e9cc934cb3b32ecf0e9b36fe64c9f02e8146ca974ea3f7`。
- 92 张：1440×1000（63）、768×1000（6）、390×844（19）、1024×1000（2）、1180×1000（2）。
  **上一轮“50/53”计数已纠正为 manifest 实测的 63 张 1440。**
- 生成：
  ```bash
  cd openspec/changes/keel-board-v1/ui/prototype
  PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs
  ```
- `shots/manifest.json` 记录每张图的 name/query/width/height/bytes/sha1/overflow/errors；
  本次结果：`overflow:false` 92/92、`errors:[]` 92/92、SHA1 92/92 不同。
- 非空白门限（本设计板定档）：数据页非主色像素 ≥5%，空/权限/迁移页 ≥4%；unique colors ≥100；
  文件 ≥20 KB。实测最小：非主色 4.26%（`queue-390-noread-404.png`）、unique 203
  （`queue-390-migrate.png`）、28 KB（`queue-390-noread-404.png`），全部通过。空白/黑图为 0% / 1 色，
  会被拦下。
- 可复算边界：字体/Chrome 版本不同会改变字节；承诺结论级复算（同一矩阵、同一状态、同一
  overflow/error 结论），不是跨机器逐字节相同。

### 7.2 交互

- `interact.mjs` 20 组断言全 PASS，生成 `interact.log`（含生成时间、Playwright 路径、errors=0）。
- 覆盖：分页、claim、complete+依赖就绪、answer、reply、tabs/filter、claim-lost takeover/force-release、
  read-only、watcher/schedule 开关、手机 44px Sheet、loading tabs/filter、stale 无实时、空态 badge 0、
  assigned 仅限、readonly 持有者、归档无动作、迁移/404、6 字段 NewTask、访问 Sheet 只读。

### 7.3 独立评审

- 本文件仍是**实现者自检**，不是独立视觉评审；独立评审应在实现后用 fresh-context 子代理执行
  （禁 Codex）。本轮不把它表述为已通过。

## 8. 未验证 / 未交付（诚实边界）

| 项 | 状态 |
| --- | --- |
| 真实 nsh TSX / TUI 渲染 | **未实现**；DESIGN §12.4 只有线框/键位/焦点/smoke 计划，不是运行证据 |
| 真实 keel-board API / SSE | 未接入；原型 fixture，工具条标注“设计样例” |
| `moves`/`force release`/`takeover`/归档筛选 | 只在 DESIGN 契约与原型替身；服务端未实现 |
| 生产 token 改动 | 原型已改 `#171717`/`#666666`；`web/theme.json` 未改（本切片不改生产文件） |
| 独立视觉评审 | 未做 |
| 真机安全区/软键盘 | 只用 CSS `env(safe-area-inset-*)`；未真机验证 |
| 浅色五态 | 已截图（5 帧）；未做浅色全动作对话框 |
| 1024/1180 | 已截图；1024 折叠、1180 双栏；未跑真实 TUI |
| OpenMotion 公网可达 | 本机 200 校验；地址 `127.0.0.1`，不是 Owner 交付链接 |

## 9. 剩余待确认/依赖

### P1 — 实现前仍需服务端/产品补齐

1. **G2 状态计数聚合**：tabs 目前无真实 summary 时只显示文字；需要 `GET /tasks/summary`。
2. **G3 更早回复游标**：`before`/`headCursor`；当前原型用 fixture 模拟。
3. **G5 origin/copies**：线程来源与外部副本失败需要 message/task 视图字段；当前的
   `mirror/origin` 是设计契约，尚未接真实 sync。
4. **G6/G7 watcher/schedule 启停与同步状态实时性**：API/状态词表 + 是否有管理事件流。
5. **G8 频道访问 API**：没有 API 时 UI 只读；要真正授权需 `GET/POST/DELETE /channels/{path}/relations`。
6. **G9 `for=pool` / actors 形状**。
7. **G12 `task.links[]`**：来源/镜像/归档的正式链接；`ticket` 标签只能作为内容，不用于动作。
8. **生产 token**：`web/theme.json` dark destructive → `#171717`，light muted → `#666666`（或等价
   AA 方案）；不改就会回退到 3.03/4.36。
9. **TUI 实现与 smoke 执行**：DESIGN §12.4 的 T1–T8 尚未跑。

### P2 — 可迭代

10. 终态排序/分页：completed/failed 是否改“最新优先”或双向分页；当前仍最早优先。
11. `answer` 的审批选项：Attention options 是否投影；当前自由文本。
12. 归档筛选的计数/入口命名（`归档 N`）是否要进 view tabs。
13. 404/迁移页对 `task.read` 的过渡窗口结束日期；本 change 不自动移除。
14. 旧 `/superagent` 下线页对无 `board.read` 用户的主操作文案（当前“打开任务队列”需按权限替换）。

## 10. 反模式清单

- [x] 无假数据进入生产路径；fixture 明确标注。
- [x] 无硬编码 hex 到组件；只用 token。
- [x] 五态、迁移、404、归档、镜像只读均有专属帧。
- [x] 无 hover-only 信息；焦点环单一规则。
- [x] 92 张截图全部真实生成、互不相同、无 overflow/console error。
- [x] 无横向溢出（92/92 measured）。
- [x] Composer 不被底部 tab 遮挡；手机 44px。
- [x] 镜像/归档动作由 moves/mirror 决定，不按 source 标签推断。
- [x] 访问 Sheet 无 API 时不显示可编辑假象。
- [x] 高阶动作有确认；claim lost 有 takeover/force release。
- [ ] 独立视觉评审未做——不得表述为已通过。
- [ ] TUI 实机/渲染未做——不得表述为已通过。
- [ ] 生产 token 未改——原型通过，生产待改。

## 11. 复现

```bash
cd openspec/changes/keel-board-v1/ui/prototype

# 92 张截图 + manifest.json
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs

# 20 组交互 + interact.log
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node interact.mjs

# 单张
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs --only queue-1440-success
```

原型 URL 参数见 `DESIGN.md` §14.1。

## 12. OpenMotion

- 项目 ID：`5e8f42d0-3237-4d8e-b37b-2676f5ea7866`
- 本机预览：`http://127.0.0.1:63535/motion-api/design/raw/5e8f42d0-3237-4d8e-b37b-2676f5ea7866/index.html`
- 本机编辑器：`http://127.0.0.1:63535/?page=design&scene=projects&designProject=5e8f42d0-3237-4d8e-b37b-2676f5ea7866`
- 这是设计工作站本机地址，不是公网交付链接；仓库 `prototype/index.html` 仍是唯一版本化源。

## 13. 文件清单

```text
openspec/changes/keel-board-v1/ui/
├─ DESIGN.md
├─ REVIEW.md
└─ prototype/
   ├─ index.html
   ├─ shoot.mjs
   ├─ interact.mjs
   ├─ interact.log
   └─ shots/
      ├─ manifest.json
      └─ 92 PNG
```

- 未改 `web/src/**`、`internal/**`、`cmd/**` 或任何生产文件。
- 未提交 `node_modules`、浏览器缓存、临时 ZIP。
