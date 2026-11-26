# keel-board-v1 · Phase 2 任务面 UI 设计

> 状态：**设计稿 / 原型，待实现与 Owner 确认**。本切片只做设计与静态原型，不改生产代码。
> 基线：keel `main@8b9c1e2`；change `openspec/changes/keel-board-v1`。
> 可交互原型：[`prototype/index.html`](prototype/index.html)（自包含，无构建依赖）。
> 自检与待确认点：[`REVIEW.md`](REVIEW.md)。

## 0. 一句话结论

**任务面是一个“队列 + 线程”的运维账本，不是一块数据仪表盘。**

- 队列回答「这个租户有哪些活、谁在干、谁可以接、卡在哪里」；默认只显示打开的任务，**最早优先**。
- 任务页回答「这一件事做过什么、谁在等谁、现在能不能推进」；任务卡与线程在同一页，动作在任务卡里，工作日志在线程里。
- 旧 `/superagent` 工单看板**删除**，不跳转、不双看板、不保留兼容开关；`/superagent` 返回一个诚实的下线页（404 语义）。
- 实时只走 SSE；租约倒计时逐秒本地重算；同步/自动化的运维状态有独立的健康面板，故障必须可见、可处置、可解释。
- web 与 TUI 共用同一份 TSX 与同一状态对象；平台差异只允许图标、悬停、快捷键呈现等不可移植项。

设计方向不是复刻旧看板：旧看板是 `ticket list + detail` 的工单视图；新任务面是
`queue → task thread` 的通用任务/线程标准，Ticket 只是它的一个来源。

## 1. 设计原则（本页面所有取舍的依据）

| 原则 | 落地 |
| --- | --- |
| **一个任务面** | 租户只有 `/board` 一个任务入口；Ticket、插件任务、人工任务、外部 Issue 都进入同一队列。不设第二个“工单看板”。 |
| **队列优先，不做事后仪表盘** | 首屏是队列，不是统计卡。计数只作为状态 tabs 的副信息；没有聚合接口就不显示总数，绝不用“本页条数”冒充总数。 |
| **最早优先** | 默认排序固定 `root_id` 升序；不提供排序切换，避免“最新优先”把 starving work 藏起来。分页只向较新方向加载。 |
| **服务端是唯一真值** | 客户端不 fold 事件账本；只合并服务端 message/task 视图。租约到点只是本地“按 ready 呈现”，写操作仍由服务端校验。 |
| **诚实态** | 加载不放假数据；空态解释为什么空；错误保留筛选与已有内容；无权限说明缺什么；镜像只读、命令桥未开、Slack 重复风险都明说。 |
| **动作只出现在能发生的地方** | 服务端规则决定可做动作；客户端按同一张表保守镜像。不可做的动作不摆一个灰按钮“钓鱼”，必要处用一句权限说明代替。 |
| **危险动作二次确认** | 完成、失败、阻塞、取消、释放、重新打开、指派、改依赖、回答都走对话框；对话框写清后果，不只写“确认吗”。 |
| **键盘与读屏是一等公民** | `j/k` 移动、`Enter` 打开、每个动作有字母键与文字标签；焦点环全局一致；不依赖 hover。 |
| **令牌，不发明颜色** | 只用 `web/theme.json` 的语义令牌 + solid-glass 材质 + lucide 单色图标；状态同时有图标、文字、颜色三种通道。 |
| **TUI 不是二等公民** | 同一份组件树在终端里退化为单列、无悬停、键盘驱动；状态、筛选、动作、错误文案一致。 |

## 2. 信息架构

### 2.1 全局位置

现有账簿 shell 的四个日常章节保持不变：

```text
01 工作区   02 任务   03 员工   04 团队          （左栏 ≥1280 带文字；768–1279 图标栏；<768 底部四 tab）
```

- `02 任务` 的唯一页面从「超级助理工单看板」替换为「任务队列」。
- 章节可见性从 `task.read` 改为 `board.read`（见 §12 迁移风险）。
- 任务页内部再分三个视图：

```text
02 任务
├─ 队列          /board                 所有人（board.read）
├─ 同步          /board/sync            仅 board.manage（入口隐藏；深链给出权限页）
└─ 自动化        /board/automation      仅 board.manage（入口隐藏；深链给出权限页）
```

`同步` / `自动化` 是**运维面**，不是日常任务流。非管理者看不到入口；直接输入地址时显示
“缺少 board.manage”的权限页，而不是白屏或伪造内容。管理者看到入口时带故障计数：
`同步 1`（有凭据失效/死信）与 `自动化 2`（有失败/死信）。

### 2.2 页面层级

```text
/board
└─ 队列（默认 open）
   ├─ 状态 tabs：打开 / 完成 / 失败 / 已取消 / 全部
   │  └─ 打开时子筛选：全部打开 / 等待依赖 / 可认领 / 已认领 / 待回复
   ├─ 筛选：频道 / 标签 / 创建者 / 指派
   ├─ 队列行 → /board/:taskId
   └─ 新建任务（Sheet）
/board/:taskId
├─ 任务卡（状态、租约、owner/by/for、needs、blocked、result/reason、标签、外部副本、动作）
├─ 根消息（任务说明全文）
├─ 回复列表（分页）
└─ Composer（回复）
/board/sync
├─ 健康摘要
├─ 同步入口卡（entries）→ 目标（targets）/ 积压 / 失败 / 凭据
├─ 死信
└─ 收件箱抽屉（board.manage，脱敏）
/board/automation
├─ 监视器（watchers）：作用域、动作、Bot、启停、最近投递、失败/死信
└─ 定时任务（schedules）：节奏、下次运行、创建者、状态、失败原因、启停/取消
```

### 2.3 路由表

| 路由 | 页面 | 权限 | 备注 |
| --- | --- | --- | --- |
| `/board` | 队列 | `board.read` | 无参数默认 `state=open`、全部频道、最早优先 |
| `/board?state=ready&channel=work&label=priority=high` | 队列（带筛选） | `board.read` + 频道 viewer | 筛选全在 URL，可回退/分享 |
| `/board?after=<cursor>` | 队列下一页 | 同上 | cursor 是上一页最后一个 root_id |
| `/board/:taskId` | 任务线程 | `board.read` + 该频道 viewer | taskId 是十进制 board seq |
| `/board/sync` | 同步健康 | `board.manage` | 静态段优先于 taskId |
| `/board/automation` | 自动化 | `board.manage` | 同上 |
| `/superagent`、`/superagent/*` | **下线页（404）** | 无 | 不重定向、不映射 ticket id |

**TUI 路由状态**：`route="board"`，`boardView="queue"|"sync"|"automation"`，`objectId=<taskId>`；
查询参数进入同一个 `BoardQuery` 对象。TUI 没有地址栏，返回/前进由路由状态承担。

**旧地址处理**：`/superagent` 不解析到 `board`。SPA 路由返回 `notFound`（服务端对应 404），
页面文案见 §10.6。这是删除，不是兼容层：不保留旧组件、旧数据源、旧测试 id。

## 3. 视觉方向：运维账本（Operations Ledger）

延续账簿 shell 的材质与排版，不引入第二套视觉语言。

### 3.1 构图

```text
1440
┌ 系统栏 ──────────────────────────────────────────────────────────────┐
│ Keel · 账簿                             实时 · 声音 · 通知 · 主题 · 我 │
├ 章节栏 208 ┬ 纸面（max 1180）────────────────────────────────────────┤
│ 01 工作区  │ 02 / 任务 · 队列        [实时] [同步 1]      刷新  新建任务 │
│ 02 任务 ●  │ 任务队列                                                     │
│ 03 员工    │ 打开的任务，最早优先；认领后租约倒计时，过期自动回到可认领。 │
│ 04 团队    │ [打开 14] [完成 3] [失败 1] [已取消 1] [全部 19]              │
│            │ 全部打开 14 · 等待依赖 3 · 可认领 8 · 已认领 2 · 待回复 1     │
│            │ [频道 全部] [标签 全部] [创建者 全部] [指派 全部]   最早优先 │
│            │ ─────────────────────────────────────────────────────────── │
│            │ #798  等待依赖  上线新的渠道对账任务 …   依赖 1/2 · 前置失败  │
│            │ #809  可认领    补齐退款审计日志 …       林岚 · 池中 · 3 小时 │
│            │ #812  已认领    修复结算页偶发重复扣款   你 · 14:32           │
│            │ …                                                            │
│            │ [加载更多（较新）]                已显示 8 / 14 · 实时更新    │
└────────────┴─────────────────────────────────────────────────────────────┘

任务页 1440
┌ 章节栏 ┬ 纸面 1180 ───────────────────────────────────────────────────┐
│        │ 队列 / #812        修复结算页偶发重复扣款   [已认领] [实时]    │
│        │ ┌ 线程 720 ─────────────────────┐ ┌ 任务卡 340（sticky）────┐ │
│        │ │ 根消息（说明 + 作者 + 时间）   │ │ 状态 · 租约 14:32 ███   │ │
│        │ │ 回复 × n                      │ │ 创建者/发起/指派        │ │
│        │ │ [加载更早的回复]              │ │ 前置依赖 2/2            │ │
│        │ │ Composer                      │ │ 外部副本 · Slack/GitHub │ │
│        │ └───────────────────────────────┘ │ [完成] [续租] [更多…]   │ │
│        │                                   └─────────────────────────┘ │
└────────┴──────────────────────────────────────────────────────────────┘
```

- 队列是**一条纸面上的 ledged list**：hairline 分隔，不叠卡片；行内是任务，不是“卡片墙”。
- 任务页是**双栏**：线程是主栏（≤760），任务卡是右栏（340，sticky）。任务卡承载状态与动作，
  线程承载说明与工作日志。低于 1180 右栏落到主栏上方，变成横向摘要卡。
- 不使用大号计数瓦片、环形图、迷你折线图：board 的数据是任务与人，不是时序指标。

### 3.2 令牌与材质

| 项 | 取值 |
| --- | --- |
| 背景 | `--background` (#161616 dark / #ededed light) |
| 纸面 | 透明；系统栏/吸顶筛选条/对话框用 `--panel`/`--popover` + solid-glass |
| 卡片 | `--card` + `glassCard`：外 1px `--glass-edge`，内 1px `--glass-inner`，顶 1px `--glass-rim` |
| 控件 | 34px 字段高度（`controlHeight("field")`），手机 44px |
| 圆角 | 控件 6/8，卡片 12，对话框 16，pill 999 |
| 字号 | 正文 14；辅助 12；mono 数据 11；任务标题 30（桌面）/20（手机）；任务卡数字 24 mono |
| 间距 | 4 / 8 / 12 / 16 / 20 / 24 / 32 / 40；行内 6 只用于密集堆叠 |
| 图标 | lucide 线性，1.8pt，单色；状态图标同时配文字 |
| 焦点 | `outline 2px color-mix(foreground 65%, transparent)`，offset 0；单一全局规则 |

状态色不是唯一通道：每个状态 badge = 图标 + 文字 + tint；租约倒计时同时有剩余时间文本与进度条。

### 3.3 状态视觉

| 状态 | 文案 | 图标 | badge 变体 | 备注 |
| --- | --- | --- | --- | --- |
| `waiting` | 等待依赖 | `clock` | outline | 行内显示 `前置 2/3`；失败前置加 `alert` |
| `ready` | 可认领 | `inbox` | default（实心） | 队列的主状态；主操作是“认领” |
| `claimed` | 已认领 | `play` | info | 行内显示租约倒计时；过期按 ready 呈现 |
| `blocked` | 待回复 / 待审批 | `message` | warning | kind 决定文案；镜像任务显示“超级助理提问” |
| `completed` | 已完成 | `check` | success | 行内显示 result 首行 |
| `failed` | 已失败 | `alert` | destructive | 行内显示 reason 首行 |
| `canceled` | 已取消 | `x` | secondary | 终态；灰而非红，取消是人的决定 |

- 状态 badge 永远带文字；“已认领”不写成“运行中”，避免和 Session 状态混淆。
- `board.task` 标签不渲染成普通标签（它是状态投影）；`board.*` 只出现在“系统标签”披露区。
- 来源不是状态：`source=superagent` 渲染为“镜像自超级助理”一行（Bot 图标），`source=github`
  渲染为“来自 GitHub · acme/web#1042”。

## 4. 页面规格

### 4.1 BoardQueue（`/board`）

#### 4.1.1 页头

- Kicker：`02 / 任务 · 队列`（mono 11，muted）。
- 标题：`任务队列`（30/20）。
- 目的句：`打开的任务，最早优先；认领后租约倒计时，过期自动回到可认领。`
- badges：
  - `LiveTailStatus`（实时 / 连接中 / 重连中 / 离线），带最后可见事件时间（tooltip 或副行）。
  - 只读成员：`只读` + `lock`，说明“你可以查看，认领/回复需要频道编辑权限”。
  - 管理者且有同步故障：`同步 1` + `alert`，点击去 `/board/sync`。
  - 当前筛选的频道/标签不在这里重复，筛选条自己表达。
- 右侧动作：`刷新`（ghost，icon+label，loading 时“刷新中…”）；`新建任务`（primary，仅当有
  可编辑频道且 `board.write`）。手机端刷新为 44px icon 按钮，新建保留为 icon+text 或进入筛选条
  右侧的 primary 按钮。
- 没有主操作的读者（viewer）不会有“新建任务”；页面不是靠禁用按钮来教权限。

#### 4.1.2 视图 tabs

- `队列 / 同步 / 自动化` 用 `Tabs variant="line"`；同步与自动化仅 `board.manage` 渲染。
- 同步 tab 的 badge 数 = 有 `credential_invalid` 的 entry 数 + 死信数 > 0 的 entry 数（不是
  失败消息总数，避免数字猩红化）；自动化 tab 的 badge 数 = 有失败或死信的 watcher/schedule 数。
- 手机端这些 tab 在标题下，与状态 tabs 分开，不混成一条。

#### 4.1.3 状态 tabs + 子筛选

- 一级 tabs：`打开`（默认，`state=open`）、`完成`、`失败`、`已取消`、`全部`。
- `打开` 时出现子筛选 chips：
  `全部打开 · 等待依赖 · 可认领 · 已认领 · 待回复`
  对应 `state=open` / `waiting` / `ready` / `claimed` / `blocked`。
- 计数：
  - 服务端提供 `summary` 时显示真实总数（见 §11 缺口 G2）。
  - 未提供时 tabs 只显示文字，**不显示“本页 N 条”冒充总数**。
  - 原型里显示的是 fixture 计数，已在原型工具条标注“设计样例”。
- tabs 用 mono 11 计数、正文 14 标签；选中态是 `glassTabsActive`，不是底色纯白。
- 手机端一级 tabs 放得下 5 个（字号 12，横向 padding 12）；子 chips 允许换行成两行，不横向滚动
  到半个字。

#### 4.1.4 筛选条

| 筛选 | 控件 | 语义 | 说明 |
| --- | --- | --- | --- |
| 频道 | Combobox | 全部 / `work（含子频道）` / `work/production` / … | 父频道是子树查询；只列可读频道 |
| 标签 | Popover 筛选构造器 | `key=value`、`key=a|b`；多个 AND | 建议值来自已加载任务 + 手动输入；排除 `board.*` |
| 创建者 | Combobox（Account） | 全部 / 陈墨 / 林岚 / 周谨 | `owner` 是 Account；不是“最后操作人” |
| 指派 | Combobox（Actor） | 全部 / 指派给我 / 池中 / 具体 Bot | `for` 是 owner 的 Actor；池中筛选需服务端支持（G9） |

- 桌面：四个 pill 一行 + `清空` 文本按钮；空间不足自动换行，不压缩到 80px。
- 手机：一个 `筛选` 按钮（带活动筛选数 badge）打开底部 Sheet；Sheet 内四组选择 + 应用/清空。
  应用后筛选条位置显示 active filter chips（`频道 work/production ×`），可单个移除。
- 右侧固定注释：`最早优先`（clock 图标）+ `第 N 页`。没有排序切换。
- 所有筛选变化立即写入 URL；刷新/回退可恢复。`after` 在筛选变化时清空回第一页。

#### 4.1.5 队列行

桌面（≥1180）列：

```text
│ #812        修复结算页偶发重复扣款                      已认领       你 · 14:32 │
│             work/production · source=superagent         需要 2 个前置 · 依赖完成 │
```

- 左：`#812` mono 12，右对齐，固定 56px；任务 ID 可复制（点击行打开，ID 本身不是链接）。
- 中：首行标题（14 medium，1 行，移动 2 行）；第二行 12 muted：
  - 频道（mono 11），标签（最多 2 个普通标签 + `+N`），`needs` 摘要。
- 右：状态 badge + 期限/归属两行。
  - ready：`创建者 · 池中` / `3 小时前`（oldest 提示）
  - claimed：`你 或 发布员·小铖` / `剩余 14:32`（mono，warning < 60s）
  - blocked：`待回复` / `2 小时`（阻塞时长）
  - completed：`完成于 14:02` / result 首行（最多 1 行）
  - failed：`失败于 13:40` / reason 首行
  - waiting：`前置 2/3` / `#810 失败`（warning）
- 行高：桌面 68–76；手机 88–104（标题两行 + 元信息）。
- 租约过期：行状态 badge 呈现 `可认领`，右侧 mono `租约已到期 · 曾任 周谨`，并出现主操作入口
  （进入任务页）；不刷新、不等待事件。
- 被指派给他人的 ready：期限列显示 `已指派 / X`；过期时显示 `租约已到期 / 指派给 X`，行内不显示
  “认领”按钮；进入任务页后主操作说明“已指派给另一个执行身份”。
- 选中/键盘光标：`glassState` 背景 + 焦点环；hover 只作补充。
- 空/错/权限/加载见 §8。

#### 4.1.6 分页

- 每页 50（原型 8）。底部：`加载更多（较新）` outline 按钮 + `已显示 8 / 14 · 最早优先`。
- 加载中按钮变 loading，不重复点击；失败在按钮下方一行“这一页没加载出来 · 重试”，保留已加载行。
- `after` 写入 URL；非法 cursor：清空 `after` 回第一页 + `分页位置已失效，已回到第一页` 提示。
- 已到末尾：`已经到队列末尾 · 最早优先`（不是错误）。

#### 4.1.7 实时合并

- SSE 事件命中当前筛选：已在列表的行原地更新；任务离开筛选（例如被认领）时淡出并在列表上方
  显示 8 秒的“已移出当前筛选：1 条（#812 已认领）”，不整页刷新。
- 新任务：
  - 当前页是尾页且未满 → 追加到尾部（最早优先）。
  - 否则显示 `队列有 2 条新任务` 的吸顶提示，点击回到第一页/刷新，不把读者拽回顶部。
- 断线：保留当前列表与筛选，显示 stale banner（§9.1）；重连后从 `Last-Event-ID` 补齐，再对
  当前页做一次 refetch 兜底去重。
- 不可见事件只有 `id:` 空帧：客户端只推进 cursor，不更新“最后事件时间”，不显示“有更新”。

#### 4.1.8 新建任务（Sheet）

- 入口：页头 `新建任务`；空态主按钮。
- 字段：
  - 频道（必填；只列有 editor 的频道；默认当前筛选频道，否则第一个可编辑频道）。
  - 任务说明（Textarea，必填，≤10,000；首行作为标题，全文作为根消息）。
  - 标签（可选；key/value 行；`board.*` 拒绝并提示 `board.* 是系统标签`）。
  - 前置依赖（可选；最多 20；不可选已取消/不可读任务）。
  - 指派（可选；池中 / 我的账号 / 我的 Bot；只能指派给创建者自己的 Actor）。
  - `创建后立即认领`（Switch + 租约时长），仅 ready 且可认领时显示。
- 提交：`创建任务`；成功 → 跳 `/board/:id` + toast；失败保留输入并显示服务端错误。
- 手机：全屏 Sheet，底部固定提交按钮（44px）；桌面：右侧 480px Sheet。

#### 4.1.9 频道访问（管理者 / 频道 owner）

- 入口：频道筛选 pill 的 `…` → `频道访问`；任务卡频道行的 `…`。
- Sheet：频道路径 mono；关系列表（Account/Group + viewer/editor/owner）；添加行（搜索 Account/Group
  + 关系）；移除；说明“子树继承：最近的祖先生效；给子树显式 viewer 可收窄”。
- 这是 §11 的 G8；没有 API 时只显示当前 `can` 与说明，不伪造编辑能力。
- 非 owner/manage 不显示入口。

### 4.2 BoardTaskCard（任务卡，右栏 / 手机上方摘要）

任务卡是任务页的“控制面”，不是第二份线程内容。

#### 4.2.1 结构

```text
┌ 任务 #812 ─────────────────────────────┐
│ [已认领]  由本标签页持有                │
│ 14:32                                   │
│ ███████████████░░░░  租约剩余 15 分钟   │
│                                         │
│ 频道      work/production               │
│ 创建者    陈墨                           │
│ 发起操作  陈墨（账号）                   │
│ 指派给    池中                           │
│ 创建时间  今天 11:08                     │
│                                         │
│ 前置依赖  2/2  [#806 ✓] [#809 ✓]        │
│ [完成]  [续租]  [更多…]                 │
│ 完成后，2 个依赖任务会变为可认领。       │
│                                         │
│ 标签  area=billing  priority=urgent     │
│ 外部副本  Slack #errors-www 已同步       │
│           GitHub acme/web#1042 重试中    │
└─────────────────────────────────────────┘
```

- 标题：`任务 #812` + 状态 badge；不重复根消息全文。
- 租约块：
  - `claimed` 且本标签页持 token：大字 `14:32` + 进度条 + `由本标签页持有` + `续租`。
  - `claimed` 但不是本标签页：`由 发布员·小铖 持有`；若是你的 Actor 但 token 丢了，显示
    `这是你认领的任务，但认领凭据不在本标签页`（§9.2）。
  - `claimed` 且已过期：`租约已到期` + `按可认领呈现`；主操作变为 `认领`。
  - 非 claimed：不显示进度条，显示状态说明（例如 `等待 2 个前置完成`）。
- 事实行：频道、创建者（owner Account）、发起操作（by Actor）、指派给（for Actor / 池中）、
  创建/更新时间。`by` 与 `for` 用文字标签，不混成“负责人”。
- needs：`前置依赖 2/2` + 每个 `#id + 状态图标 + 状态词`；点击打开对应任务。空为 `无前置`。
  - 有 failed/canceled 前置：warning 行 + `前置 #810 已失败，不会自动就绪；可改依赖或等它重新打开。`
  - waiting 但所有 needs 已完成：`前置已完成，正在就绪…` + 3 秒后出现 `刷新`。
- blocked：warning 面板，kind 文案（`等待回复` / `等待审批`）+ reason 全文（≤2 行，展开可看全）
  + `回答`（镜像 needs_input）或 `解除阻塞`（owner/manage）。
- result/reason：completed 显示 result；failed 显示 reason；canceled 显示 reason。≤3 行，展开全文。
- 标签：普通标签 chips；`source/ticket` 变成来源行；`board.*` 收进 `系统标签` 折叠，不参与编辑。
- 外部副本（来自 sync，G5）：
  - 每行：平台（Slack/GitHub/Linear/Chat SDK）+ target + 状态（已同步/重试中/失败）+ 最后时间。
  - 失败：一行错误（截断）+ `查看同步`（manage）或 `等待重试`（普通成员）。
  - 无副本：不显示该区块，不写“暂无”。
  - Slack entry 显示 `重复风险` 小字说明（无 idempotency key），不假装 exactly-once。
- 动作区：见 §6。动作排在事实/依赖之后、标签与外部副本之前，保证 1000px 高视口内主操作可见；标签与副本属于低频信息。

#### 4.2.2 手机/平板

- ≤1179：任务卡从右栏落到线程上方，成为横向摘要：
  - 第一行：状态 + 租约大字 + 主操作。
  - 第二行：事实折叠为 `创建者 · 指派 · 频道`，`详情` 展开。
  - needs / blocked / result 默认展开（这些是“为什么不能做”的关键信息），标签与外部副本折叠。
- ≤767：单列；主操作 full width，次要动作 2 列；`更多` 打开 bottom sheet（同一动作集合）。
- 任务卡是 sticky（≥1180），不遮挡线程；焦点进入卡内时不会被 sticky 重新定位打断。

### 4.3 BoardThread + Composer（`/board/:taskId`）

#### 4.3.1 页头

- 面包屑：`任务 / 队列 / #812`（前者可点回队列，保留筛选）。
- 标题：根消息首行（≤2 行）；右侧 `已认领` badge、`实时` badge、复制链接 icon、刷新。
- 若当前任务不在已加载队列里（深链）：直接按 ID 读取；读取失败按 §8 处理。
- 任务已结束：标题旁 `已完成/已取消`，动作区只留读操作。

#### 4.3.2 根消息

- 作者行：头像（Account 首字 / Bot 图标）+ `陈墨` 或 `陈墨 · 发布员·小铖`（Bot via Account）+
  时间（`今天 11:08`，title 完整 ISO）+ 频道 chip + 来源行（镜像/GitHub/Slack）。
- 正文：保留换行，最多 10,000 字符；链接只允许 http/https；代码用 mono 块；无 HTML。
- 根消息下方：`#812` mono + `复制链接`，不重复状态（状态在任务卡）。
- 根消息的标签不重复任务卡的标签；只显示来源行。

#### 4.3.3 回复列表

- 单层，按 id 升序；每条：作者（Account / Bot via Account / 平台 actor）+ 时间 + 来源
  （`来自 Slack #errors-www` / `GitHub acme/web#1042`）+ 正文 + `已编辑`。
- 系统事件（状态变化）不作为回复正文；任务卡表达状态，线程只放人的工作日志。若需要审计轨迹，
  后续加“活动”折叠，不在本版混入回复。
- 分页：初始最新 20 条；顶部 `加载更早的回复（共 34 条）`（G3 需要 `before` cursor）。
  加载失败：顶部一行 `更早的回复没加载出来 · 重试`，不阻塞当前回复。
- 空：`还没有回复` + `写下第一条工作日志，说明你看到了什么、准备怎么做。` + `写回复` 聚焦 Composer。
- 不重复：SSE 与发送响应按 message id 去重；自己的回复不乐观插入，等服务端返回后追加。
- 新回复到达且读者在底部附近：跟随；读者向上翻：不拽回，显示 `有 2 条新回复` 按钮（复用聊天
  滚动纪律，但线程更短，按钮足够）。

#### 4.3.4 Composer

- 结构：glass field + auto-grow Textarea（3–8 行）+ 底部工具行（Markdown 提示、字符数、`回复`）。
- 快捷键：`Cmd/Ctrl+Enter` 发送；`Enter` 换行。发送中按钮 loading、输入锁定；成功清空并 toast
  （或不 toast，直接出现）；失败保留草稿 + 红色 inline 错误 + `重试`。
- 计数：≤9,000 不显示（避免噪声）；>9,000 显示 `9,120 / 10,000`，超限禁用发送。
- 权限：
  - 无 `board.write` 或频道非 editor：不渲染输入框，显示 `你在该频道只有查看权限；回复需要编辑权限。`
  - 镜像只读：`该任务由超级助理管理，命令桥未开启；在超级助理里回复。`
- 手机：Composer 吸底在全局底部 tab 之上（safe-area padding）；内容区预留同高 padding，避免最后
  一条回复被遮住；键盘弹出时输入框保持在可视区（TUI 无此问题）。
- 错误态：`回复没有发出去` + 服务端 message + request id + `重试` / `复制草稿`；草稿不丢。
- 空提交禁用，不玩“幽灵按钮”。

#### 4.3.5 活动/同步条

- 任务卡的外部副本已表达同步状态；线程不重复。
- 若根消息来自外部平台，根消息上方一行 `来自 Slack #errors-www`；这是导入事实，不是状态。

### 4.4 BoardSyncStatus（`/board/sync`）

#### 4.4.1 页头

- Kicker：`02 / 任务 · 同步`；标题 `同步`。
- 目的句：`外部平台的线程与任务双向镜像。这里只看健康与故障处置；凭据与路由由部署配置管理。`
- 右侧：`刷新`；`同步已停用`（若 `board.sync.enabled=false`）badge。
- 健康摘要（一行，不用大瓦片）：
  `4 个入口 · 3 个活跃 · 1 个凭据失效 · 2 条死信`，每个数字可点筛选到对应入口。
  无故障时：`4 个入口 · 全部正常 · 最后投递 14:02:11`。

#### 4.4.2 同步入口卡（entry）

每卡：

```text
┌ Slack · slack            [启用]    [凭据有效]  [活跃]        ┐
│ 路由  #errors-www → work/production   #prs → work/release     │
│ 入站  2 待处理 · 滞后 12s        出站  1 待投递 · 滞后 4s      │
│ 24h   3 次失败 · 1 条死信        最后投递  14:02:11           │
│ 目标  slack:C08…:172… → #812 已同步                           │
│       slack:C08…:172… → #814 重试中 3/5                       │
│ [重试死信] [查看收件箱] [停用]                                 │
└───────────────────────────────────────────────────────────────┘
```

- 平台 badge：Slack / GitHub / Linear / Chat SDK；key 用 mono。
- 状态：
  - `活跃` success；`已停用` secondary；`凭据失效` destructive；`同步已停用` outline。
  - `credential_invalid`：卡顶红色 Alert（不是整卡红底）：`凭据失效，已停止收发（fail closed）。
    轮换凭据后会自动恢复；在轮换完成前不会重试投递。` 不显示 token/secret。
- 指标：入站/出站 lag（秒/分钟）+ pending 数；24h 失败；死信；最后成功投递；凭据状态。
  - lag > 300s 或死信 > 0：数字用 warning/danger 色 + 图标，不只变色。
  - 没有数据：`—`，不写 0 冒充“正常”。
- 路由：`#channel → board/channel` 或 `repo → board/channel`；不显示平台 token。
- 目标：最多默认 3 行 + `展开全部`；每行 target mono、board 根 `#id`（可点）、状态、undelivered 数。
- 动作：
  - `启用/停用` Switch（停用需 AlertDialog：`停用后不再收发外部消息；已同步的副本保留。`）。
  - `重试死信`（仅 dead > 0；跳/滚动到死信区并筛选该 entry）。
  - `查看收件箱`（manage；抽屉显示脱敏 raw：dedupe、received、status、attempts、last_error；不含
    body 正文/secret）。
  - 不提供“新建入口/编辑凭据/编辑 URL”——这些是部署配置；本 UI 不发明 OAuth 安装向导。
- Slack 已知限制：卡底一行 `Slack 没有幂等键；极端崩溃窗口可能重复一条消息。` 12 muted + info 图标。
- Chat SDK bridge：显示 `bridge host`（只显示 host，不显示 path/secret）、最近 HMAC/nonce 错误数、
  `已停用` 状态；不可编辑 baseURL。

#### 4.4.3 死信

- 区标题 `死信 · 2`；表格/列表列：`dedupe`（截断）、`收到`、`尝试`、`错误`、`入口`、`重试`。
- 空：`没有死信 · 所有入站事件都已处理或正在重试`。
- 重试：单条 `重试`（loading → `已重新排队` / 错误 inline）；批量 `重试全部` 需二次确认。
- 说明：`死信只阻塞该外部线程；其他线程和 board 主路径继续。`
- 权限：仅 `board.manage`；普通成员看不到此区，也看不到 raw 收件箱。

#### 4.4.4 五态

- loading：摘要 + 2 个 entry 骨架。
- empty（`board.sync.enabled=false` 或 0 entries）：`同步没有启用` / `还没有同步入口`，说明由部署
  配置启用；不显示“创建”假按钮。
- error：`没能读到同步状态` + 重试 + request id；已加载的 entry 保留并标 stale。
- permission：`需要「管理任务面」能力（board.manage）` + 返回队列 + 复制申请。
- success：如上。

### 4.5 BoardAutomation（`/board/automation`）

#### 4.5.1 页头

- Kicker `02 / 任务 · 自动化`；标题 `自动化`。
- 目的句：`监视器按事件触发已声明的动作；定时任务按计划发消息。失败不阻塞任务主路径。`
- 右侧刷新；`只读` badge（无 manage）。

#### 4.5.2 监视器（watchers）

每行：

```text
triage-inbox     work/ 子树 · state=ready       插件：assign-by-label     发布员·小铖   [开]
最近 14:01:52 成功 · 24h 42 次 · 0 失败 · 0 死信
```

- 字段：watcher id（mono）、作用域（channel/子树）、filter chips、动作（部署时声明的名字，
  不是 URL）、Bot、Switch、最近投递、24h 成功/失败/死信、最后错误。
- 动作：`启停`（Switch，立即；失败回滚 + inline 错误）、`重试死信`（有死信时）、`查看投递`（抽屉：
  event seq、状态、尝试、next_at、last_error）。
- 说明：`动作与 URL 在部署时声明，界面不编辑任意运行时 URL。`
- 一键停用不弹确认？停用监视器是“暂停自动化”，可逆；但会停止通知。设计：Switch 立即生效 +
  toast `已停用 triage-inbox`，并提供 Undo（重新启用）5 秒。不弹模态，减少摩擦。
- 失败：`escalate-blocked` 一行 warning `最近一次失败：Bot 缺少频道 editor 权限`；Switch 仍可用。

#### 4.5.3 定时任务（schedules）

每行：

```text
晨间队列摘要     work · 每天 09:00 · 下次 11-28 09:00    陈墨    [运行中] 已发 14 次   [暂停]
```

- 字段：首行文本、频道、节奏（every/until/times）、下次运行、创建者、状态、已发次数、最后错误。
- 状态：`运行中` success / `已暂停` secondary / `已完成` outline / `已失败` destructive /
  `已取消` secondary。
- 动作：`暂停/继续`（Switch）、`取消`（危险，AlertDialog）、`查看历史`（可选，G6 未定）。
- 权限被回收：`渠道 B 对账提醒` 行显示 destructive `创建者已失去 work 的编辑权限，已自动停发。`
  只能 `取消`，不能继续；这是服务端语义，UI 不提供“强制继续”。
- 失败重试：定时任务的失败通常是永久权限/配置问题；只展示原因与取消，不伪造“重试发送”。

#### 4.5.4 五态

同 §4.4.4，文案改为自动化；empty：`还没有监视器或定时任务`，说明它们由部署/API 声明，此处只
观察与启停。

### 4.6 旧地址下线页（`/superagent*`，404）

- 标题：`这个地址已经下线`
- 描述：`旧的「超级助理工单看板」已并入任务队列；旧工单地址不会自动跳转。超级助理仍保存工单记录。`
- 路径：`/superagent/6a2f…` mono 11，便于用户复制给管理员。
- 主操作：`打开任务队列`；次操作：`复制说明`（给管理员/同事）。
- 不显示“正在跳转…”，不做 `replaceState`，不映射 ticket id 到 board id。
- 若 `board.read` 也没有：主操作变为 `返回工作区`，并说明需要 `board.read` 才能看任务。

## 5. 组件树（web/TUI 共用）

```text
BoardRoute
└─ BoardPage
   ├─ BoardHeader
   │  ├─ Kicker / Title / Purpose
   │  ├─ BoardLiveBadge (LiveTailStatus)
   │  ├─ BoardHealthBadges (read-only, sync failures)
   │  └─ BoardHeaderActions (Refresh, NewTask)
   ├─ BoardViewTabs (Queue / Sync / Automation)          [manage]
   ├─ BoardQueueView
   │  ├─ BoardStateTabs (Tabs)
   │  ├─ BoardStateChips (only when open)
   │  ├─ BoardFilterBar / BoardFilterSheet (mobile)
   │  │  ├─ ChannelCombobox
   │  │  ├─ LabelFilterPopover
   │  │  ├─ OwnerCombobox
   │  │  └─ ForCombobox
   │  ├─ BoardQueueList
   │  │  ├─ BoardQueueColumnHead (≥1180 only)
   │  │  ├─ BoardQueueRow × n
   │  │  │  ├─ TaskId
   │  │  │  ├─ TaskStateBadge
   │  │  │  ├─ TaskTitleLine
   │  │  │  ├─ TaskMetaLine (channel / labels / needs)
   │  │  │  └─ TaskDeadlineCell (lease / blocked age / result)
   │  │  ├─ BoardQueueNotice (new tasks / left filter)
   │  │  └─ BoardLoadMore
   │  └─ BoardFiveStates (Skeleton / Empty / Error / Permission / Content)
   ├─ BoardThreadPage
   │  ├─ BoardBreadcrumb
   │  ├─ BoardThreadHeader
   │  ├─ BoardTaskCard
   │  │  ├─ TaskStateBlock (badge + lease countdown + progress)
   │  │  ├─ TaskFacts
   │  │  ├─ TaskNeedsList
   │  │  ├─ TaskBlockedPanel
   │  │  ├─ TaskOutcomePanel
   │  │  ├─ TaskLabels / TaskSystemLabels
   │  │  ├─ TaskSyncCopies
   │  │  └─ BoardTaskActions (primary / secondary / overflow)
   │  ├─ BoardRootMessage
   │  ├─ BoardReplyList
   │  │  ├─ BoardReply × n (author / time / provenance / body)
   │  │  └─ BoardLoadEarlier
   │  └─ BoardComposer
   ├─ BoardSyncView
   │  ├─ SyncHealthSummary
   │  ├─ SyncEntryCard × n
   │  │  ├─ SyncEntryHeader (platform / key / switch / status)
   │  │  ├─ SyncLagFacts
   │  │  ├─ SyncRoutes
   │  │  ├─ SyncTargets
   │  │  └─ SyncEntryActions
   │  ├─ DeadLetterList
   │  └─ InboxDrawer
   ├─ BoardAutomationView
   │  ├─ WatcherList / WatcherRow
   │  └─ ScheduleList / ScheduleRow
   ├─ ChannelAccessSheet
   ├─ NewTaskSheet
   └─ BoardDialogs
      ├─ CompleteDialog / FailDialog / BlockDialog
      ├─ AssignDialog / NeedsDialog / CancelDialog
      ├─ ReleaseDialog / ReopenDialog / AnswerDialog
      └─ ShortcutHelp
```

### 5.1 组件 ↔ kit / shadcn 映射

| 设计件 | 生产组件（nsh kit） | shadcn 对应 | 备注 |
| --- | --- | --- | --- |
| BoardHeader | `SheetHead`（books/common） | `CardHeader`/排版 | 复用章节页头语法 |
| BoardViewTabs / StateTabs | `Tabs` / `Tab` `variant="line"` / `"default"` | `Tabs` | 手机高度 44 |
| Badges | `Badge` | `Badge` | 只用 success/warning/destructive/info/secondary/outline |
| Queue row | `PressRow` + `Separator`/`Hairline` | `Item`/`Separator` | 列表不是 `Table`；行可键盘聚焦 |
| Filter pills | `Combobox` / `Popover` / `Select` | `Combobox`/`Popover` | 移动用 `Sheet` |
| Task card | `Card` + `Separator` | `Card` | `glassCard` |
| Lease progress | `Progress` + `Text` | `Progress` | 同时有文本，不只颜色 |
| Needs chips | `Badge` (pressable) + `Button variant="link"` | `Badge` | 点击打开任务 |
| Actions | `Button` + `DropdownMenu` | `Button`/`DropdownMenu` | 主操作最多 1 个 |
| Dialogs | `Dialog` / `AlertDialog` | `Dialog`/`AlertDialog` | 必填标题；焦点陷阱 |
| New task / filters / access | `Sheet` | `Sheet` | 手机全屏 |
| Replies | `Card`/`Text`/`Separator` + `Avatar` | `Message`/`Item` | 工作日志，不做聊天气泡 |
| Composer | `Textarea` + `Button` + `Card` | `Field`/`Textarea` | `InputGroup` 不适用（多行+工具栏） |
| Sync/automation metrics | `Badge` + `Card` + `Separator` | `Card` | 不用图表 |
| Switch | `Switch` | `Switch` | 立即生效 + 失败回滚 |
| Loading | `Skeleton` | `Skeleton` | 行骨架与真实行同高 |
| Empty | `Empty` + `EmptyMedia/Title/Description/Content` | `Empty` | 不用自绘空态 |
| Error | `Empty` + `Alert`/`PageError` | `Alert` | 带 request id |
| Toast | kit `toast`/`Toast` | `sonner`/`toast` | 成功/失败反馈 |
| Shortcut hint | `Kbd` | `Kbd` | 只在帮助面板与按钮 title |

**原型说明**：`prototype/index.html` 是视觉规格的静态实现，用来验证布局/状态/交互手感；生产实现
必须组合上表的官方组件，**不得把原型的 HTML/CSS 直接搬进 `web/src`**。

## 6. 交互与动作矩阵

### 6.1 动作可见性（客户端保守镜像，服务端最终裁决）

记号：`✓` 显示；`—` 不显示；`H` 仅 claim holder（token 在本标签页）；`O` owner/manage；
`C` 频道 owner / board.manage；`R` 需要 `board.write` + 频道 editor。

| 动作 | waiting | ready | claimed | blocked | completed | failed | canceled | 说明 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 认领 claim | — | ✓ R（for 匹配） | —（过期后按 ready） | — | — | — | — | 租约 30s–3600s，默认 900s |
| 续租 renew | — | — | H | — | — | — | — | 租约内任意时刻 |
| 释放 release | — | — | H | — | — | — | — | 二次确认 |
| 完成 complete | — | — | H | — | — | — | — | 对话框，可选 result |
| 失败 fail | — | — | H | — | — | — | — | 对话框，必填 reason |
| 阻塞 block | — | O | H 或 O | — | — | — | — | claim 时需 holder；非 claim 需 owner |
| 解除阻塞 unblock | — | — | — | O | — | — | — | 可选补充回复 |
| 回答 answer | — | — | — | O / H（镜像 needs_input） | — | — | — | 需命令桥（G1） |
| 重新打开 reopen | — | — | — | — | — | O | — | 回到 ready |
| 指派 assign | O | O | O | O | — | — | — | failed 先 reopen |
| 改依赖 needs | O | O | — | O | — | — | — | 有 live claim 不可改 |
| 取消 cancel | O | O | O | O | — | O | — | 终态拒绝 |
| 回复 reply | R | R | R | R | R | R | R | 需频道 editor |
| 新建任务 | R | R | R | R | R | R | R | 需 board.write |
| 频道访问 | C | C | C | C | C | C | C | 与任务状态无关 |
| 同步/自动化 | manage | manage | manage | manage | manage | manage | manage | 运维面 |

补充规则：

- `O` 的判定：Account 身份 → `task.owner == 我`；Bot 身份 → `task.owner_actor == 我的 Actor`；
  频道 owner 关系可对任意任务做 owner 操作（仍需频道 editor）。被收回频道关系后只剩频道 owner。
- `for` 指向其他 Actor 的 ready 任务：认领显示为不可用说明，不显示按钮。
- `completed`/`canceled` 是终态：不显示任何状态变更；显示 `复制链接` 与（若 terminal 有结果）
  `展开结果`。
- `failed` 只接受 reopen/cancel；`blocked` 只接受 unblock/cancel/assign/needs（owner）。
- 只读成员（频道 viewer）：看得到任务与线程，看不到写动作；任务卡底部一行 `只有查看权限` 说明。
- 镜像只读（命令桥关闭）：除查看外全部隐藏；`回答` 也不显示，改为说明（G1）。

### 6.2 动作流程与文案

#### Claim（认领）

- 交互：点 `认领` 立即执行，默认租约 900s；旁边小 `15 分钟` 下拉可选 30 秒 / 15 分钟 / 60 分钟
  （服务端 cap）。
- 客户端生成 32 字节随机 token（base64url），**随请求提交并只存内存**；这样网络失败重试时仍持有
  同一 token，不会出现“服务端认领成功、客户端丢 token”。（服务端允许调用方自带 token，§7.1）
- 成功：toast `已认领 #812 · 租约至 14:47`；任务卡变为持有态，倒计时开始。
- 409 `task_claimed`：`刚刚被 林岚 认领了`；行/卡刷新。
- 409 `task_assigned`：`这个任务指派给 发布员·小铖，当前身份不能认领。`
- 403：`需要 work/production 的编辑权限才能认领。`
- 网络超时：`认领结果未确认` + `刷新任务`；不自动重试（即使客户端有 token，也先确认 epoch）。

#### Renew（续租）

- 点 `续租` 立即执行，从 `now()` 延长一个租约；不弹窗。
- 成功 toast `租约延至 15:02`；倒计时重置。
- 409 `claim_lost`：`认领已失效（到期或已结算）`；按 §9.2 处理。

#### Release（释放）

- AlertDialog：标题 `释放 #812？`；描述 `任务会回到池中，其他人可以认领。已发布的回复不会撤回。`
  按钮 `继续持有` / `释放`（destructive）。
- 成功 toast `已释放 #812`；任务回到 ready。

#### Complete（完成）

- Dialog：标题 `完成 #812`；描述 `完成后不可撤销；依赖它的 2 个任务会变为可认领。`
  - `结果（可选）` Textarea，0/1000；placeholder `交付了什么、验证了什么、链接或版本号。`
  - 按钮 `取消` / `完成`（primary）。
- 成功：原地更新 + toast `已完成 #812 · 2 个依赖任务已就绪`。
- 409 `claim_lost`：`认领已失效，结果没有写回。` + `刷新任务`；保留输入。
- 409 `task_finished`：`任务已被其他操作结束。` + 刷新。

#### Fail（失败）

- Dialog：标题 `标记 #812 失败`；描述 `失败会结束认领；依赖它的任务不会自动就绪，需要重新打开并
  完成。` 必填 `失败原因`（0/1000）。
- 按钮 `取消` / `标记失败`（destructive）。

#### Block（阻塞）

- Dialog：标题 `阻塞 #812`；`阻塞类型` RadioGroup：`等待回复（needs_input）` /
  `等待审批（awaiting_approval）`；`说明` 必填（0/1000）。
- 描述：`阻塞会结束当前认领；解除阻塞只有创建者、频道管理员或当前持有者可以操作。`
  若当前持有：`你的认领会立即结束。`
- 按钮 `取消` / `阻塞`（warning/destructive）。
- 成功 toast `已阻塞 #812 · 等待回复`。

#### Unblock（解除阻塞）

- Dialog：标题 `解除阻塞 #812？`；可选 `补充说明（会作为回复发布）`；描述
  `任务会按前置依赖回到「可认领」或「等待依赖」。`
- 成功 toast `已解除阻塞 · 任务回到可认领`（或等待依赖）。

#### Reopen（重新打开）

- AlertDialog：`重新打开 #812？` 描述 `任务回到可认领；失败原因保留在记录里；依赖者仍会等到它完成。`
  按钮 `保持失败` / `重新打开`。

#### Assign（指派）

- Dialog：标题 `指派 #812`；Select：
  - `池中（任何有编辑权限的人）`
  - `陈墨（账号）`
  - `发布员·小铖（陈墨的 Bot）`
- 说明：`只能指派给创建者自己的账号或 Bot；跨账号指派不在本版范围。`
- 成功：任务卡 `指派给` 更新 + toast；指派不改状态。

#### Needs（前置依赖）

- Dialog：标题 `前置依赖 #812`；当前 chips；搜索/选择任务（按 #id 或标题）；最多 20；保存空数组
  表示无前置。
- 错误：
  - `invalid_needs`：`这个前置不可用（不存在、无权查看、已取消，或会形成循环）。`
  - 超 20：`最多 20 个前置。`
  - 409 `task_claimed`：`任务正被认领，先释放或完成后再改前置。`
  - 409 `task_finished`：`任务已结束，不能改前置。`
- 保存成功：状态可能 waiting→ready / ready→waiting；toast 说明结果。

#### Cancel（取消）

- Danger Dialog：标题 `取消 #812`；必填 `取消原因`；描述 `取消后不可恢复；依赖它的任务会一直等待，
  需要改依赖或取消。`
- 按钮 `保留任务` / `取消任务`（destructive）。

#### Answer（回答，镜像任务）

- Dialog：标题 `回答并解除阻塞`；引用 `提问：移动端通知文案由谁定稿？`（blocked.reason）；
  `回复` 必填（≤10,000）；描述 `回答会作为回复写入线程，并把问题交回超级助理。`
- 成功：任务 unblocked + 回复出现 + toast；失败保留输入；桥关闭时入口不显示（G1）。

#### Reply（回复）

- 见 §4.3.4；无二次确认（可编辑/删除不在本版）。

### 6.3 二次确认清单

| 动作 | 二次确认 | 形式 |
| --- | --- | --- |
| 认领 / 续租 | 否 | 低风险、可释放/到期 |
| 释放 | 是 | AlertDialog |
| 完成 | 是 | Dialog（可选结果 + 后果） |
| 失败 | 是 | Dialog（必填原因 + 后果） |
| 阻塞 | 是 | Dialog（类型 + 原因 + 结束认领） |
| 解除阻塞 | 是 | Dialog（可选补充） |
| 重新打开 | 是 | AlertDialog |
| 指派 | 是 | Dialog（选 Actor） |
| 改依赖 | 是 | Dialog（校验 + 保存） |
| 取消 | 是 | Danger Dialog（必填原因） |
| 回答 | 是 | Dialog（答案即回复） |
| 停用同步 / 取消定时任务 | 是 | AlertDialog |
| 启用/停用监视器、暂停定时任务 | 否（可逆） | Switch + toast + 5s Undo |
| 重试死信 | 否（幂等） | Button + loading |

## 7. 状态模型（客户端）

### 7.1 数据源

| 状态 | 来源 | 备注 |
| --- | --- | --- |
| `me` | `GET /v1/board/me` | account、可选 bot、actor、`can.board.*`、频道 can 映射 |
| `channels` | `GET /v1/board/channels` | 只含可读频道；`can` 决定编辑器 |
| `queue` | `GET /v1/board/tasks` + SSE | 查询对象 + 页数组 + cursor + tail 标记 |
| `thread` | `GET /v1/board/messages/{id}` | 根 + 最新回复 + reply cursor |
| `live` | `/v1/board/events/stream` | status、cursor、lastVisibleEventAt、attempt |
| `sync` | `GET /v1/board/sync/entries` + `/status` + `/inbox` | manage |
| `automation` | `/watchers*` + `/schedules*` | manage |
| `claimTokens` | 内存 `Map<taskId,{epoch,token,until}>` | **不进 localStorage** |
| `drafts` | 内存 | 回复/对话框草稿；刷新即丢 |
| `actions` | 内存 | in-flight、error、确认目标 |

### 7.2 派生

```text
effectiveState(task, now):
  if task.state == "claimed" and task.claim.until <= now: return "ready"
  return task.state

matches(query, task):
  state tab + sub state + channel subtree + label AND/OR + owner + for

deadlineText(task, now):
  ready      → "3 小时前"（创建/就绪时长）
  claimed    → "剩余 14:32"（mono；<60s warning）
  blocked    → "已阻塞 2 小时"
  waiting    → "前置 2/3"（失败前置单独 alert）
  completed  → "完成于 14:02"
  failed     → "失败于 13:40"
```

- `effectiveState` 只影响呈现与认领入口；任何写操作仍由服务端 epoch/token/state 校验。
- 倒计时逐秒更新文本节点；归零时**只把该行的 badge/动作切换为 ready**，不整页重排。服务端
  `task.expired` 到达后以服务端视图为准。
- 筛选在客户端即时应用（已加载页）；修改筛选会触发服务端查询并从第一页开始。
- 排序不在客户端重排：服务端 oldest first；SSE 插入按 root_id 排序，新任务放尾部。

### 7.3 SSE 合并

- 连接：`Last-Event-ID` 优先于 `after`；断线指数退避 1s→30s；55s 服务端关闭属正常，静默重连。
- 帧：`id` 是 board seq；`data.type` + `task`/`message` 视图；只按 cursor 去重，不按时间。
- 不可见事件：只有 `id:` 空帧 → 推进 cursor，不更新 lastVisibleEventAt，不提示“有更新”。
- 线程：命中当前 task 的消息追加/更新；缺失则 refetch。
- 队列：命中筛选的任务更新/移出；新任务按 §4.1.7。
- 服务端 503（重建中）：显示 `看板正在重建，读取暂时不可用` + 自动重试；不显示半重建数据。
- 重连后：先回放到 cursor，再 refetch 当前队列页/线程，按 id 去重。

### 7.4 认领 token 与刷新

- token 由客户端生成（32 字节随机，base64url），只存当前标签页内存；服务端只存 SHA-256。
- 刷新/重开标签页：token 丢失。若任务仍是 claimed 且 holder 是我：
  - 显示 `这是你认领的任务，但认领凭据不在本标签页。`
  - 不显示 complete/renew；显示租约剩余 + `租约到期后可重新认领`。
  - 可做的只有 owner/manage 操作（cancel/assign/needs，按规则）；提供 `复制任务链接`。
  - 这是诚实的限制，不用 localStorage 恢复 token；恢复路径缺口见 §11 G4。
- 同一浏览器其他标签页：显示 `由你（另一个标签页）持有`，同样不可结算。

## 8. 权限、403/404 与五态

### 8.1 权限模型

| 层 | 判定 | UI |
| --- | --- | --- |
| 章节入口 | `board.read` | 不可读则整章不渲染（shell 规则：隐藏，不灰） |
| 队列页 | `board.read` + 至少一个可读频道 | 无频道 → “你还没有可访问的频道”引导 |
| 写动作 | `board.write` + 频道 editor | 只读 → `只读` badge + 动作隐藏 + 一句说明 |
| 管理动作 | `board.write` + 频道 owner 或 `board.manage` | 指派/取消/解除/重开/改依赖/频道访问 |
| 同步/自动化 | `board.manage` | 入口隐藏；深链权限页；敏感字段（raw inbox、target 错误）仅 manage |
| 频道内容 | 频道 viewer | 无 view → 404，不确认存在 |

**404 vs 403（必须严格）**：

- 不可读频道/消息/任务/事件：**404** `not_found`，页面 `任务不存在，或你没有访问这个频道`。
  不显示“存在但无权限”，不泄露标题/ID 之外的任何内容。
- 可读但无写：**403** `forbidden` + `missing`（`board.write` / `channel:editor`），页面可显示任务内容，
  动作区说明缺什么。
- 跨租户：404，与不存在合并。
- `board_disabled`：404；页面 `任务看板未启用`，说明由租户管理员开启，不是“没权限”。
- 未登录：401；登录入口 + 返回路径。
- owner-only 动作由非 owner 发起：409 `task_not_owned`；只对可读任务显示 inline 说明，不进 403 页。

### 8.2 五态矩阵

| 界面 | loading | empty | error | permission | success |
| --- | --- | --- | --- | --- | --- |
| 队列 | 6 行骨架（与真实行同高）；筛选条保留 | 无打开任务 / 无匹配筛选 / 无可读频道 三种文案 | 错误 + request id + 重试；保留筛选与已加载行（stale banner） | 无 board.read / 未登录 / board_disabled 三态 | 行 + 分页 + 实时 |
| 任务 | 标题 + 根 + 回复骨架 | 无回复 ≠ 无任务；根永远在 | 读失败：返回队列 + 重试；已有内容标 stale | 不可读频道 404；只读 403 内容可见 | 卡 + 线程 + 动作 |
| 同步 | 摘要 + 2 卡骨架 | 同步未启用 / 无入口 | 状态读取失败 + 重试；保留旧数据 | 无 board.manage | 入口/目标/死信 |
| 自动化 | 列表骨架 | 无监视器/定时任务 | 读取失败 + 重试 | 无 board.manage | 监视器/定时任务 |

### 8.3 空态文案（三分别）

| 场景 | 标题 | 描述 | 动作 |
| --- | --- | --- | --- |
| 没有打开的任务 | `队列是空的` | `没有打开的任务。新任务会实时出现在这里。` | `新建任务` / `查看已完成` |
| 筛选无结果 | `没有符合筛选的任务` | `当前筛选：频道 work/release · 标签 priority=high · 创建者 林岚。` | `清空筛选` / `新建任务` |
| 没有可读频道 | `你还没有可访问的频道` | `任务按频道隔离。找频道 owner 或租户管理员把你加入 viewer/editor。` | `复制申请说明` / `返回工作区` |
| 无回复 | `还没有回复` | `写下第一条工作日志，说明你看到了什么、准备怎么做。` | `写回复` |
| 无前置 | `无前置` | （不写段落） | — |
| 同步未启用 | `同步没有启用` | `这个租户还没有开启外部同步；任务与线程不受影响。` | `返回队列` |
| 无同步入口 | `还没有同步入口` | `入口由部署配置声明，这里只观察健康与处置故障。` | `返回队列` |
| 无自动化 | `还没有监视器或定时任务` | `自动化在部署/API 里声明；这里只显示运行状态与启停。` | `返回队列` |
| 无死信 | `没有死信` | `所有入站事件都已处理或正在重试。` | — |

### 8.4 错误态文案

| code | 标题 | 描述 | 动作 |
| --- | --- | --- | --- |
| `unauthenticated` | `登录已失效` | `重新登录后回到这个页面。` | `重新登录` |
| `forbidden` | `缺少权限` | `需要「{capability}」；当前账号缺少 {missing}。` | `返回队列` / `复制申请说明` |
| `not_found` | `任务不存在，或你没有访问这个频道` | `地址没有对应到你可读的任务；它可能已被删除，或频道权限已变化。` | `返回队列` |
| `board_disabled` | `任务看板未启用` | `这个租户还没有开启任务看板。找租户管理员开启后再试。` | `返回工作区` |
| `task_claimed` | `任务刚被认领` | `{actor} 在几秒前认领了它；页面已更新。` | `刷新任务` |
| `claim_lost` | `认领已失效` | `租约到期、已被结算，或凭据来自另一个标签页。` | `刷新任务` |
| `task_assigned` | `已指派给另一个执行身份` | `这个任务指派给 {actor}；只有该身份能认领。` | `查看任务` |
| `task_not_owned` | `只有创建者或频道管理员可以这样做` | `你仍可以查看线程；指派/取消/改依赖需要任务 owner 或频道 owner。` | — |
| `task_finished` | `任务已结束` | `已完成或已取消的任务不能再改状态。` | `刷新任务` |
| `already_a_task` | `这个线程已经是任务` | `一个线程只能对应一个任务。` | `打开任务` |
| `invalid_needs` | `前置不可用` | `不存在、无权查看、已取消，或会形成循环；最多 20 个。` | 修改选择 |
| `reserved_label` | `系统标签不能手动设置` | `board.* 由任务状态维护。` | 移除该标签 |
| `duplicate_id` | `这次操作已经提交过` | `同一个请求返回了第一次的结果；没有重复写入。` | `刷新` |
| `invalid_cursor` | `分页位置已失效` | `已回到第一页。` | — |
| `unavailable` | `服务暂时不可用` | `没有写入任何内容；稍后重试。` | `重试` |
| `rate_limited` | `操作太频繁` | `稍等几秒再试。` | `重试` |
| `mirror_read_only` | `镜像任务只读` | `该任务由超级助理管理；命令桥未开启时不能在这里改状态。` | `去超级助理`（若可） |
| network | `连接中断，结果未确认` | `可能已经写入。先刷新任务再决定是否重试。` | `刷新任务` / `重试` |

错误卡片一律：人话标题 + 一句原因 + 可执行下一步 + request id（服务端提供时）。不显示堆栈。

## 9. 实时、租约与故障呈现

### 9.1 Live tail

| 状态 | badge | 附加说明 |
| --- | --- | --- |
| 已连接 | `实时` success + wifi | `最后更新 14:02:11`（仅可见事件） |
| 连接中 | `连接中` info + refresh | 首次连接，不遮挡内容 |
| 重连中 | `重连中` warning + refresh | 第 N 次重试；超过 3 次显示 stale banner |
| 离线 | `离线` destructive + wifi-off | `实时连接已断开，显示的是 14:02:11 的数据。` + `立即重连` |

- 断线不整页清空；保留列表/线程与筛选。
- 重连成功：badge 回 `实时`，stale banner 消失，toast `已恢复实时更新`。
- 15s 心跳 `: ping` 不显示；55s 服务端关闭不提示“断开”，直接重连。
- 连续失败：`重试中（第 4 次，最多 30 秒）` + `改为手动刷新` 不必；自动即可。
- 隐藏事件只推进 cursor；lastVisibleEventAt 不变。

### 9.2 租约

- 倒计时用 `claim.until` 与本地时钟；每秒更新；服务端时钟偏差在 ±2s 内忽略。
- <60s：warning 色 + 文本 `剩余 58 秒`；<10s：danger + `即将到期`。
- 到点：行/卡即时按 ready 呈现，显示 `租约已到期`，认领动作出现；不等待服务端事件。
- 到点后用户点认领，服务端若已写入 expired/被他人认领，按 409 处理并刷新。
- 刷新丢失 token：§7.4。
- 续租失败 `claim_lost`：不弹重复对话框；inline + 刷新。
- Owner 不能强制释放他人的 claim；UI 不提供该按钮（G4）。

### 9.3 同步故障

| 现象 | 位置 | 呈现 |
| --- | --- | --- |
| entry `credential_invalid` | 同步页 | 红色 Alert：已停止收发（fail closed）；轮换后恢复；不显示 secret |
| inbox lag > 5min | entry 卡 | `入站积压 6 分钟` warning + `待处理 12`；不阻塞 board |
| outbox lag > 5min | entry 卡 | `出站积压 8 分钟` warning + `待投递 3` |
| 单条投递失败 | 任务卡外部副本 + 同步页 | `重试中 3/5` / `失败` + 最后错误；后续成功会清除 undelivered |
| 死信 | 同步页死信区 | dedupe、尝试次数、错误、单条/批量重试；说明只阻塞该外部线程 |
| Slack 无幂等键 | entry 卡底 | `重复风险` 说明，不声称 exactly-once |
| 凭据轮换失败 | 同步页 | fail closed；`凭据失效`；轮换入口在设置-凭据（不在此页编辑） |
| 同步未启用 | 同步页 | 空态，不是错误 |
| 不可见事件 | 队列/线程 | 只推进 cursor，不显示“有活动” |

### 9.4 监视器/定时任务故障

| 现象 | 呈现 |
| --- | --- |
| watcher 投递失败 | 行内 `最近一次失败：<error>`；失败计数 +1；Switch 仍可用；死信可重试 |
| watcher 无权限 | `Bot 缺少频道 editor 权限`；动作不执行；不重试到成功幻觉 |
| schedule 权限被回收 | `创建者已失去 <channel> 的编辑权限，已自动停发。`；只能取消 |
| schedule 失败 | `已失败` + last_error；不提供“强制重试”，避免绕过权限复检 |
| 启停失败 | Switch 回滚到原值 + inline 错误 + toast；不假装成功 |
| 单个失败 | 不影响其他 watcher/schedule 与 board 主路径（页脚说明） |

## 10. 键盘、可达性与文案

### 10.1 键位

| 键 | 作用 | 作用域 |
| --- | --- | --- |
| `j` / `↓` | 下一行 | 队列；任务页焦点在动作区时不用 |
| `k` / `↑` | 上一行 | 队列 |
| `Home` / `End` | 首/末行 | 队列 |
| `Enter` | 打开任务 / 触发按钮 | 队列行 / 聚焦控件 |
| `c` | 认领 | 队列选中行 / 任务页 |
| `r` | 续租 | 任务页（持有） |
| `e` | 完成 | 任务页（持有） |
| `f` | 失败 | 任务页（持有） |
| `b` | 阻塞 | 任务页（可阻塞） |
| `u` | 解除阻塞 | 任务页（blocked + owner） |
| `o` | 重新打开 | 任务页（failed + owner） |
| `a` | 指派 | 任务页（owner） |
| `n` | 改依赖 | 任务页（owner） |
| `x` | 取消 | 任务页（owner） |
| `[` / `]` | 一级状态 tabs | 队列 |
| `1`–`5` | 子筛选 chips（打开时） | 队列 |
| `/` | 聚焦筛选 | 队列 |
| `Esc` | 关闭对话框/筛选 / 返回队列 | 全局 |
| `?` | 快捷键帮助 | 全局 |
| `Cmd/Ctrl+Enter` | 发送回复 | Composer |
| `Space` | 切换 Switch | 自动化 |

- 输入框/Textarea 聚焦时，单字母动作键失效；`Cmd/Ctrl+Enter` 仍有效。
- 每个动作按钮都有 `accessibilityLabel` 与可见文字；图标按钮（复制、更多、刷新）有 tooltip +
  aria-label；不依赖 hover。
- 焦点顺序与视觉顺序一致；对话框打开时焦点进入标题/第一个字段，`Tab` 循环，关闭后回到触发按钮。
- 焦点环全局一条规则（`outline` 2px foreground 65%），不用 `box-shadow`。
- 队列行 `role="option"`/`aria-selected`；tabs `role="tablist"`/`aria-selected`；badge 的文字可读。

### 10.2 文案纪律

- 用人的词：`认领`、`租约`、`前置依赖`、`待回复`、`指派给`，不用 `claim/lease/needs/for` 作为界面文案
  （调试/技术详情除外）。
- 一句一事；错误先说什么发生了，再说什么没发生（“没有写入任何内容”），最后下一步。
- 不用“暂无数据”四个字；空态必须解释为什么空。
- 时间用 `今天 14:02` / `3 小时前`；任务 ID 用 `#812`；频道用 mono。
- 不出现 fixture/测试/内部 id（除 task id、request id、channel path、actor 名）。
- 中文 + 专有名词（Slack/GitHub/Linear/Bot/claim token 只出现在技术详情）；不混英文状态词。

## 11. 响应式策略（390 / 768 / 1440）

沿用账簿 shell 断点：`≤767` 手机、`768–1023` 平板、`1024–1279` 紧凑栏、`≥1280` 宽栏。

| 断点 | 队列 | 任务页 | 同步/自动化 |
| --- | --- | --- | --- |
| **1440** | 208 rail；纸面 max 1180；列头 + 右侧期限列；筛选一行；每页 50 | 双栏：线程 1fr（≤760）+ 任务卡 340 sticky | 2 列入卡片或单列宽卡（1180）；死信表 |
| **1024–1279** | 60 icon rail；纸面满宽；筛选可换行；列头保留，右列压到 140 | 双栏在 ≥1180；1180 以下任务卡落到线程上方 | 单列卡；指标 2×2 |
| **768–1023** | 60 icon rail；列头保留；筛选可换行；行保持四列但标题/元信息自然换行；期限在右列 | 单列：摘要卡 → 根 → 回复 → Composer；动作行内（34px） | 单列；entry 指标 2×2；死信列表 |
| **≤767** | 无 rail；底部四 tab；行卡片；筛选底 Sheet；状态 chips 换行；44px 控件 | 摘要卡（动作 full width）→ 根 → 回复 → Composer 吸底；标签/副本折叠 | 单列；指标 2×2；死信卡片；危险动作 bottom sheet |

### 11.1 手机细节（390）

- 顶部系统栏压缩为 44px；标题 20px；kicker 可省略但保留“任务”。
- 队列行：
  - 第一行：`#812` mono 11 + 状态 badge + 右侧倒计时（mono 12）。
  - 第二行：标题（14，2 行）。
  - 第三行：频道 + 指派/创建者 + `依赖 2/3`（12 muted，换行）。
- 筛选按钮与 `新建任务` 同一行；两者 44px；筛选 badge 显示活动条件数。
- 任务摘要卡：
  - 状态 + 主操作同一行；租约大字第二行。
  - `详情` 展开事实；needs/blocked/result 默认展开。
  - 次要动作 2 列；`更多` 打开 bottom sheet。
- Composer 吸底：`position: sticky; bottom: 56px`（底部 tab 高度）+ safe-area；内容区底部 padding
  等于 Composer 高度，避免最后一条回复被遮。
- 对话框：`Sheet` 从底部升起，圆角 16 顶部；标题 + 描述 + 字段 + 底部按钮（primary full width）。
- 无横向滚动：`overflow-x: clip`；长频道路径/actor 名 `min-width: 0` + ellipsis；代码块横向滚动
  只限自身。
- 触控目标 ≥44px；Switch 行高 ≥44；行内链接/图标按钮 44。

### 11.2 平板（768）

- icon rail 60（与 shell 一致），内容 padding 24。
- 队列保留列头与四列栅格（768 仍放得下）；标题与元信息自然换行，右列保留期限/归属。
- 任务卡横向摘要：左 240（状态/租约），右 1fr（事实），动作整行；needs/blocked 全宽。
- 同步 entry 指标 2×2；目标默认 2 行 + 展开。
- 不把桌面双栏硬压成 3 栏；1024 以下不出现被挤压的第三列。

### 11.3 1440

- 纸面 max 1180，左右留白 32；rail 208。
- 队列列宽：ID 56 / 状态 104 / 主体 1fr / 期限 180。
- 任务页 720 + 32 gap + 340；线程正文行宽 ≤ 72ch（`max-width: 72ch`）。
- 同步卡内部：平台 160 / 指标 1fr / 动作 200；死信表 6 列。
- 不使用 3 列以上的仪表盘；纸面保持“账本”。

## 12. web / TUI 同源映射

### 12.1 同源

- 同一份 TSX：`web/src/screens/board/{page,queue,task,thread,sync,automation,dialogs}.tsx`。
- 同一状态源：`web/src/data/board.ts`（归一化缓存 + SSE + 查询对象 + claim token 内存）。
- 同一动作表：`boardActions(task, me, channelCan)` 纯函数；web/TUI 都从它取 `moves`。
- 同一文案表：状态词、错误码、空态/五态文案集中在 `boardVocabulary.ts`。
- 平台差异只允许：图标渲染、hover、tooltip、滚动条、键盘事件来源、Composer 吸底；**不允许**
  状态、筛选、动作、权限文案不同。

### 12.2 平台差异清单

| 能力 | web | TUI | 处理 |
| --- | --- | --- | --- |
| 布局 | 1440 双栏 / 手机单列 | 单列，宽度自适应 | 同一断点函数；TUI 视为 narrow |
| 悬停 | hover 提示 | 无 | 所有信息都有可见文本/焦点态 |
| 图标 | Canvas lucide | 同（或文字回退） | 状态永远有文字 |
| 弹层 | Dialog/Sheet/Popover | 模态面板/内联 | 同一组件，nsh 渲染差异 |
| 快捷键 | 键盘 + 鼠标 | 全键盘 | 同一 keymap；鼠标事件可选 |
| Composer | 吸底 + 软键盘 | 底部行 | 同一 `BoardComposer`，样式差异 |
| 复制 | Clipboard API | 终端复制/提示 | `copyText` 抽象 |
| 外部链接 | openUrl | 打印/提示 | 不在核心路径 |
| 倒计时 | 每秒重绘文本 | 同 | 同一 ticker |

### 12.3 TUI 验收点

- 80×24 与 120×40 两种终端下，队列行不折成乱码；标题/状态/期限三列仍可读。
- `j/k/Enter/c/r/e/f/b/u/o/a/n/x` 全部可达；对话框可用 Tab/方向键；Esc 返回。
- 状态 badge 在无色终端仍有文字；颜色不是唯一通道。
- Composer `Ctrl+Enter` 发送；无鼠标可完成全部动作。
- 同一 fixture 在 web 与 TUI 的 `texts()`/`accessibility()` 包含相同状态词与动作标签。

## 13. 破坏性变更：旧 `/superagent` 看板退役

### 13.1 删除清单

| 对象 | 处理 |
| --- | --- |
| `web/src/screens/books/superagent.tsx` | 删除 |
| `web/src/screens/books/superagent.harness.tsx` | 删除 |
| `web/src/screens/books/superagent.tui.test.tsx` | 删除（由 `board.*` 测试替代） |
| `web/src/data/superagent.ts` + `superagent.test.ts` | 删除（只读数据源；不得保留“备用”） |
| `route.ts` 的 `"superagent"` route / `/superagent` 解析 | 删除；新增 `"board"` 与 404 |
| `chapters.tsx` 的 `SuperagentPage` 分支 | 替换为 `BoardPage` |
| `shell.tsx` 的 chapter id `"superagent"` / `TASK_KEYS` | 改为 `"board"` / `board.read` |
| 仅服务旧看板的 API 读取 | 随实现核实；不再被任何屏幕引用后删除（不删 superagent 领域 API） |
| 旧测试 id `superagent-*` | 改为 `board-*`；不留别名 |

**不删除**：superagent 的 Ticket/Assignment/Attention 领域 API 与 `collab_*` 真值；board 只通过
适配器读取。是否删除“仅服务旧看板的只读投影路由”由 Phase 2 实现时按引用清点决定（§13.3）。

### 13.2 地址与数据

- `/superagent`、`/superagent/:ticketId` → 404 下线页；**不重定向**，因为 ticket UUID 与 board
  root seq 不是同一 id 空间，重定向只能猜。
- 浏览器书签/文档链接失效：下线页给出 `/board` 入口；CHANGELOG/README 升级说明写清。
- 旧看板的本地草稿/答复状态不迁移；旧看板没有 claim/lease 语义。
- 已归档 Ticket：旧看板删除后，若适配器选择“跳过归档”，用户将没有入口查看；见 §11 G11 / REVIEW。

### 13.3 权限迁移风险

- 章节可见性从 `task.read` 改为 `board.read`。**只拥有 `task.read` 的账号会看不到 02 任务**。
- 这是有意的破坏性变更：board 是新权限面。上线前必须：
  - 租户管理员给相关角色授予 `board.read`（以及需要的 `board.write`/`board.manage`）；
  - 或在首次开启 board 时给默认角色/管理员一次性 bootstrap（需 Owner 决定，G10）；
  - 发布说明写清“旧 `task.read` 不再打开任务章节；任务日志（Session 内）仍用 task.read”。
- 频道关系：新频道创建者自动获得 owner；其他成员由 owner/manage 授予。没有批量迁移旧工单的
  频道关系（旧看板无频道概念）。运维需在启用前排好频道访问。

### 13.4 验收

- `rg "/superagent|SuperagentPage|superagentAccess|superagent-" web/src` 无产品引用（测试替换后）。
- 直接访问 `/superagent` 显示下线页；HTTP 直接请求返回 404。
- 02 任务章节、队列、任务页、同步、自动化全部由 board 承接；旧看板的“工单/答复/完成”旅程在
  board 上有对应动作（Ticket 镜像 → 任务；Attention → blocked → 回答；report → 回复）。
- 不保留双看板、兼容开关、旧数据回退。

## 14. 原型与证据

### 14.1 打开方式

```bash
open openspec/changes/keel-board-v1/ui/prototype/index.html
# 或带参数：
#   ?scene=queue|task|sync|automation|gone
#   &state=success|loading|empty|filtered|error|permission|disabled|stale
#   &me=manager|reader|none
#   &id=812
#   &dialog=complete|fail|block|assign|needs|cancel|release|reopen|answer|filters|access
#   &sync=success|loading|none|error
#   &automation=success|loading|none|error
#   &claim=lost
#   &composer=error
#   &facts=1         # 手机任务卡展开事实
#   &bridge=0        # 镜像任务命令桥关闭（只读）
#   &scroll=thread|composer|dead|linear|schedules
#   &chrome=0        # 隐藏原型工具条（截图用）
#   &theme=light
```

- 原型用 fixture 数据；工具条明确标注“设计样例”。生产 UI 不得用假数据。
- 原型是视觉/交互规格，不是生产代码；生产必须用 nsh kit/shadcn 官方组件重写（§5.1）。

### 14.2 截图

`prototype/shots/` 下的 PNG 由本地 Chrome + Playwright 生成，覆盖 390/768/1440 与五态/对话框。
构建身份：`index.html` SHA-256 `69c9605cdb3a4bcd08d91e7c77758e7e51477dfa78479467827de343e29b9291`。截图矩阵（`shoot.mjs`）、尺寸/非空白/差异统计与可复算命令见 [`REVIEW.md`](REVIEW.md) §6。

### 14.3 设计工作台

- **OpenMotion Design Studio**：项目 ID `acb9e7de-491e-42d5-a87f-a1c15b17a1af`，v1 导入
  （`scenario=prototype`，entry `index.html`）。
  - 本机预览：`http://127.0.0.1:63535/motion-api/design/raw/acb9e7de-491e-42d5-a87f-a1c15b17a1af/index.html`
  - 本机编辑器：`http://127.0.0.1:63535/?page=design&scene=projects&designProject=acb9e7de-491e-42d5-a87f-a1c15b17a1af`
  - 说明：以上是**设计工作站本机地址**，不是 Owner 交付链接；公网暴露需走 alpha/隧道。
  - 仓库内静态原型是唯一可版本化源；工作台副本是 v1 导入，后续修改以仓库为准再重新导入。
- 静态原型是可复算的基线（`shoot.mjs` 逐 URL 生成 74 张截图；`interact.mjs` 跑 10 组交互断言）。

## 15. 后端契约缺口（设计倒逼，待确认）

> 这些不是 UI 可以自行造出来的数据/语义。phase 2 前需与 keel-board / keel 实现对齐；若已存在，
> 按此形状复用。

| ID | 缺口 | 为什么需要 | 建议 |
| --- | --- | --- | --- |
| **G1** | 任务视图没有权威 `moves` / `mirror` / bridge 状态 | 客户端镜像规则会漂移；镜像只读/回答入口需要知道命令桥是否开启 | `task.moves: string[]`、`task.mirror: {source, readOnly, bridge}`；或 `/me` 带 flags |
| **G2** | `/tasks` 没有状态计数聚合 | 状态 tabs 不显示假总数 | `GET /v1/board/tasks/summary?channel&label&owner&for` → `{open:{waiting,ready,claimed,blocked}, completed, failed, canceled, all}` |
| **G3** | 线程只有 `after`（向后）游标 | “加载更早的回复”无法实现 | `GET /messages/{id}?before=` 或返回 `headCursor` |
| **G4** | claim 凭据丢失后无恢复路径 | 刷新/换标签页后 holder 不能 complete/renew；owner 也不能强制 release | `POST /tasks/{id}/release` 允许 owner/manage（无 token，需审计）或 `claim {takeover:true}`（同 Actor） |
| **G5** | 消息视图没有 `origin` / `copies` | 线程无法显示“来自 Slack”、外部副本失败 | message/task 视图带 `origin {key,platform,target,author}` 与 `copies[{target,status,lastError}]`；或 `GET /messages/{id}/sync` |
| **G6** | watcher/schedule 启停与状态形状未在 API 表定义 | 自动化页需要 pause/resume/status/最近投递 | `PATCH /watchers/{id} {enabled}`、`PATCH /schedules/{id} {status:paused|pending}`、`GET .../deliveries`；返回字段按 §4.5 |
| **G7** | 同步管理状态无实时通道 | 秒级健康面 | `GET /sync/status` 带 `updatedAt` + 可见期 10s 轮询，或 `events/stream` 支持 `sync.*` 管理事件（不作为任务实时通道） |
| **G8** | 频道访问关系无管理 API | 频道 owner 无法在 UI 授权 | `GET/POST/DELETE /channels/{path}/relations`（Account/Group × viewer/editor/owner） |
| **G9** | `for` 过滤没有“池中”值；`/actors` 形状未定 | 指派筛选与 Actor 名称 | `for=pool` 或 `for=null` 约定；`GET /actors` 返回 account/bot 显示名 |
| **G10** | `task.read` → `board.read` 的权限迁移/bootstrap | 否则旧账号看不到任务章 | 租户启用 board 时给默认角色授予 `board.read`（Owner 决策） |
| **G11** | 归档 Ticket 在旧看板删除后无入口 | 审计/回溯需求 | 适配器镜像归档 Ticket（只读）或在 board 提供“归档来源”只读页；否则保留一个只读导出 |
| **G12** | 任务视图没有 `links`（superagent source/ticket/archive） | 来源行、镜像只读提示 | `task.links[]` 或按 labels 约定读取；不建议只靠 `source=` 标签 |

## 16. 验收清单（设计 → 实现）

- [ ] `/board` 队列：状态 tabs + 子 chips、channel/label/owner/for 过滤、最早优先、分页、行内
      状态/needs/租约倒计时；URL 可恢复。
- [ ] `/board/:id` 任务卡：全部动作按 §6.1 显示；每个危险动作有二次确认与后果文案。
- [ ] 线程：根卡、回复分页、空/错、Composer 成功/失败/只读、`Cmd/Ctrl+Enter`。
- [ ] 租约：倒计时、<60s 警告、到点 ready、刷新丢 token 提示、claim_lost 处理。
- [ ] live tail：四态、断线保留内容、重连补齐、隐藏事件不泄露。
- [ ] 同步：entries/targets/lag/failures/dead letter/凭据失效/启停/重试/收件箱（manage）。
- [ ] 自动化：watchers/schedules 状态、启停、失败、权限被回收自动停发、死信重试。
- [ ] 五态：队列/任务/同步/自动化 × loading/empty/error/permission/success 均有专属帧。
- [ ] 权限：403/404/401/board_disabled/task_not_owned/task_assigned/mirror_read_only 文案与入口。
- [ ] 键盘：§10.1 全部键位；焦点环；对话框焦点陷阱；无 hover-only 信息。
- [ ] web/TUI：同一 TSX；80×24/120×40；状态/筛选/动作/文案一致。
- [ ] 响应式：390/768/1440 截图；无横向溢出；Composer 不被底部 tab 遮挡。
- [ ] 退役：`/superagent` 404 下线页；旧文件/路由/数据源/测试 id 清除；无重定向/兼容开关。
- [ ] 证据：脚本生成截图 + HTML 报告，`UNCOVERED=0`；独立子代理复核（禁 Codex）P0/P1 清零。
