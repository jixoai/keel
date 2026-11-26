# keel-board-v1 · Phase 2 任务面设计自检

> 状态：**设计稿 / 可交互原型，待 Owner 与实现确认**。本切片不改生产代码。
> 设计正文：[`DESIGN.md`](DESIGN.md)；原型：[`prototype/index.html`](prototype/index.html)；
> 截图：[`prototype/shots/`](prototype/shots/)（74 张，390/768/1440）。
> 基线：keel `main@8b9c1e2`；change `keel-board-v1`。

## 1. 结论

- **可以进入实现评审**，但不建议直接进入 Phase 2 实现：有 **4 个 P0 决策**（§9）必须先定，
  其中 3 个是产品/后端语义，不是 UI 能绕过的。
- 设计覆盖了任务面全部要求的表面：队列、任务卡、线程/Composer、同步、自动化、五态、
  键盘、web/TUI 同源映射、390/768/1440、旧 `/superagent` 退役。
- 原型可交互；74 张截图全部真实生成、互不相同、无横向溢出、无 console/page error。
- 自检发现 2 个**既有 token 层**的对比度不达标（不是本设计引入）：dark destructive 按钮
  白字 3.03:1、light muted 正文 4.36:1；已列入 §9，未私改令牌。

## 2. 交付物

| 文件 | 说明 |
| --- | --- |
| `ui/DESIGN.md` | 信息架构、路由、组件树、状态模型、动作矩阵、权限/403/404、五态、键盘、web/TUI、响应式、sync/自动化故障、退役、后端缺口 |
| `ui/prototype/index.html` | 自包含静态原型（内嵌 CSS/JS，零构建、零运行时依赖） |
| `ui/prototype/shoot.mjs` | 截图矩阵；逐 URL 产出 74 张 PNG，输出 bytes/sha1/overflow/console |
| `ui/prototype/interact.mjs` | 10 组原型交互断言（分页/claim/complete/answer/reply/tabs/filter/claim-lost/readonly/toggles/mobile sheet） |
| `ui/prototype/shots/*.png` | 1440×1000（50 张）、768×1000（6 张）、390×844（15 张） |
| `ui/REVIEW.md` | 本文件 |

## 3. 「像产品」自检

| 判据 | 结论 | 证据 |
| --- | --- | --- |
| 页面有目的陈述与主操作 | 队列有目的句 + `新建任务`；任务页主操作随状态变化（认领/完成/回答/重新打开） | `queue-1440-success.png`、`task-1440-claimed.png` |
| 数据有主次、分组、解释 | 行分 4 段（ID/状态/标题与元信息/期限归属）；任务卡把状态、租约、依赖、动作分层；同步按 entry→target→dead letter 分层 | 队列/任务/同步截图 |
| 渐进披露 | 筛选在桌面一行、手机底部 Sheet；危险动作在 `更多` 或确认框；手机事实默认折叠、`详情` 展开；标签/外部副本在动作之后 | `task-390-claimed.png`、`task-390-facts.png` |
| 文案是人话 | `认领/租约/前置依赖/待回复/指派给`；错误先说发生了什么、再说没写入什么、最后下一步 | `DESIGN.md` §8.4，`task-1440-composer-error.png` |
| 空/错/权限/加载/成功有设计 | 四个表面 × 五态都有专属帧（§5） | 见 §5 |
| 不是裸表格/裸 JSON | 行有 lease/needs/reason/actor；同步有 lag/dead/credential 解释与动作 | `sync-1440-success.png` |
| 移动端可达 | 390 无横向滚动；筛选 Sheet；动作 44px；Composer 吸底 | `queue-390-*`、`task-390-*` |
| 不做“演示版” | 每个状态/动作/失败/无权限都有明确设计；镜像只读、claim 丢失、Slack 重复风险都明说 | `task-1440-claim-lost.png`、`task-1440-mirror-readonly.png` |

## 4. 层级、视觉与令牌

### 4.1 排版阶梯

| 层 | 字号 | 用途 |
| --- | --- | --- |
| 页面标题 | 30（桌面）/20（手机） | 队列/同步/自动化/任务标题 |
| 区块标题 | 20 | 任务卡/区段（设计中） |
| 动作/正文 | 14 | 行标题、消息正文、按钮 |
| 辅助 | 12 | 元信息、描述、次要行 |
| 数据 | 11 mono | ID、频道、倒计时、状态计数、错误码 |
| 最小 | 10/9 | 标签、mono key；不承载中文正文 |

- 没有“读数大于标题”的倒挂；任务卡租约数字 24 mono 是唯一的大号数字，且服务于状态。
- 正文行宽 ≤ 72ch（任务根消息/回复），避免满宽长文。
- 中文正文最小 12px；10px 只用于 mono key/英文标签，符合 kit 的 `typeScale.key` 约束。

### 4.2 间距与触控

- 间距取 4/8/12/16/20/24/32/40；6/2 仅用于密集行内。
- 控件桌面 34px（`controlHeight("field")`），手机 44px；桌面按钮 34、密集 28。
- 手机筛选 Sheet 字段/按钮 44px（`interact.mjs` 断言 44px）。
- 焦点环：单一 `outline 2px color-mix(foreground 65%)`；实测对背景 5.82:1（非文本 ≥3:1）。

### 4.3 材质与图标

- 只用 `theme.json` 语义令牌；hex 只出现在 `:root` 的令牌定义处，组件 CSS 全部 `var()`。
- Solid-glass：面板/卡片/popover/field/state/primary/tabs 七种材质按 `styles.tsx` 的 rim→inner→edge→shade 顺序复现。
- 图标：lucide 24×24、1.8pt 单色；状态 badge 同时有图标 + 文字 + tint，颜色不是唯一通道。

### 4.4 实测对比度（深色默认，light 关键对）

| 对 | 实测 | 门限 | 结果 |
| --- | --- | --- | --- |
| foreground / background | 12.21 | 4.5 | PASS |
| muted-foreground / background | 8.83 | 4.5 | PASS |
| foreground / card | 9.29 | 4.5 | PASS |
| muted-foreground / card | 6.72 | 4.5 | PASS |
| foreground / popover | 9.42 | 4.5 | PASS |
| primary-foreground / primary | 16.01 | 4.5 | PASS |
| foreground / success-tint 16% | 9.31 | 4.5 | PASS |
| foreground / warning-tint 16% | 9.33 | 4.5 | PASS |
| foreground / destructive-tint 16% | 9.94 | 4.5 | PASS |
| foreground / info-tint 16% | 9.70 | 4.5 | PASS |
| link / background | 9.24 | 4.5 | PASS |
| focus ring（fg 65%）/ background | 5.82 | 3 | PASS |
| **dark destructive-foreground / destructive** | **3.03** | 4.5 | **FAIL** |
| light foreground / background | 8.86 | 4.5 | PASS |
| **light muted-foreground / background** | **4.36** | 4.5 | **FAIL** |
| light muted-foreground / card | 4.68 | 4.5 | PASS |
| light destructive-foreground / destructive | 4.83 | 4.5 | PASS |

- `3.03` 与 `4.36` 都是**既有 kit / theme token**，不是本设计新增的颜色；本设计不私改令牌。
  建议见 §9 P1-9。
- 主题：默认深色（`nsh.json appearance=dark`）；浅色令牌已支持但未截浅色图（§10）。

## 5. 五态覆盖矩阵

### 5.1 队列

| 态 | 帧 | 文案/动作 |
| --- | --- | --- |
| loading | `queue-1440-loading.png`、`queue-390-loading.png` | 6 行骨架，与真实行同高；不显示假数据 |
| empty | `queue-1440-empty.png`、`queue-390-empty.png` | `队列是空的` + 新建任务 / 查看已完成 |
| filtered empty | `queue-1440-filtered-empty.png` | 保留 tabs/筛选与 active chips；`清空筛选` / `新建任务` |
| error | `queue-1440-error.png`、`queue-390-error.png` | 错误 + request id + 重试；tabs/筛选保留 |
| permission | `queue-1440-permission.png`、`queue-390-permission.png` | `board.read` 缺失 + 返回工作区 / 复制申请 |
| disabled | `queue-1440-disabled.png` | `任务看板未启用`（404 语义），无 tab/动作 |
| stale | `queue-1440-stale.png` | 保留列表 + 重连 banner；不整页清空 |
| success | `queue-1440-success.png`、`queue-390-success.png` | 队列 + 分页 + 实时 |
| 只读成员 | `queue-1440-readonly.png` | `只读` badge；无新建；行仍可读 |

### 5.2 任务/线程

| 态 | 帧 | 说明 |
| --- | --- | --- |
| success | `task-1440-claimed/ready/waiting/blocked/blocked-owner/completed/failed/failed-readonly/canceled.png` | 七态各一帧；另含「他人失败」只读帧 |
| claim lost | `task-1440-claim-lost.png` | 不显示完成/续租；`认领凭据不在本标签页` |
| blocked owner | `task-1440-blocked-owner.png` | 非镜像阻塞；主操作 `解除阻塞` |
| read-only | `task-1440-readonly.png` | 只读说明，无 claim/回复 |
| mirror read-only | `task-1440-mirror-readonly.png` | 命令桥关闭；无动作，去超级助理 |
| empty replies | `task-1440-empty-replies.png` | 无回复 ≠ 无任务 |
| loading | `task-1440-loading.png` | 标题 + 行骨架 |
| error | `task-1440-error.png` | 返回队列 + 重试 |
| permission/404 | `task-1440-permission.png` | 不区分不存在与无权 |
| composer error | `task-1440-composer-error.png` | 草稿保留 + 复制草稿 / 重试 |

### 5.3 同步 / 自动化

| 态 | 同步 | 自动化 |
| --- | --- | --- |
| loading | `sync-1440-loading.png` | `automation-1440-loading.png` |
| empty | `sync-1440-empty.png` | `automation-1440-empty.png` |
| error | `sync-1440-error.png` | `automation-1440-error.png` |
| permission | `sync-1440-permission.png` | `automation-1440-permission.png` |
| success | `sync-1440-success.png` | `automation-1440-success.png` |
| 故障 | `sync-1440-deadletters.png`、`sync-1440-credential-invalid.png` | `automation-1440-failure.png` |
| 手机 | `sync-390.png` | `automation-390.png` |

### 5.4 旧地址

- `gone-1440.png`、`gone-390.png`：404 下线页，不重定向；无实时/刷新；路径示例为
  `/superagent/6a2f…`（旧路由是 `/superagent/:ticketId`，不是 `/tickets/`）。

## 6. 证据与可复算

### 6.1 截图

- 构建身份：`prototype/index.html` SHA-256 `69c9605cdb3a4bcd08d91e7c77758e7e51477dfa78479467827de343e29b9291`；74 张图由该文件生成。
- 74 张：1440×1000（53）、768×1000（6）、390×844（15）；全部为本地 Chrome + Playwright。
- 生成命令：
  ```bash
  cd openspec/changes/keel-board-v1/ui/prototype
  PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs
  ```
- 每次运行输出每张图的 `bytes / sha1 / overflow / console errors`；本次结果：
  - `overflow: false` 74/74；
  - `errors: []` 74/74；
  - 74 个 SHA1 全部不同（无重复帧）；
  - 最小文件 25 KB（`queue-390-loading.png`），最小非主色像素占比 4.62%
    （`queue-390-permission.png`），最少 unique colors 216（`queue-1440-disabled.png`）。
  - 定档门限（本设计板）：数据页非主色像素 ≥5%、空/权限页 ≥4%；unique colors ≥100；
    文件 ≥20 KB。全部通过。空白/黑图会是 0% 非主色与 1 种颜色，因此该门限能拦住黑图。
  - `automation-1440-success` 与 `automation-1440-failure` 的像素统计接近，但 SHA1 不同；
    failure 帧滚动到 schedules 并显示权限被回收的行。
- 可复算边界：字体/Chrome 版本不同会导致字节级差异；本报告承诺的是**结论级可复算**
  （同一矩阵、同一状态、同一 overflow/error 结论），不是跨机器逐字节相同。

### 6.2 交互

- `interact.mjs` 10 组断言全部 PASS：队列首页、claim、complete、answer、reply、tabs/filter、
  claim-lost/read-only、watcher/schedule 开关、手机筛选 Sheet 44px。
- 无 pageerror / console error。

### 6.3 独立截图判读

- 本切片没有引入第二个人类评审；截图判读由实现者完成，**属于自检，不计独立视觉评审**。
  独立评审应在实现后用 `fde2e-delivery-gates` 的 fresh-context 子代理执行（禁 Codex）。
- 已用 contact sheet 做过一轮全量目检；发现并修掉：移动端 header 动作重叠、行状态与
  标题重叠、facts 图标换行、needs 编辑在只读时仍显示、disabled 页仍显示动作/tabs、
  claim-lost 未覆盖、composer error 初始不显示、dead letter 无表头。

## 7. 键盘与可达性

| 键 | 行为 | 验证 |
| --- | --- | --- |
| `j/k`、`↑/↓` | 队列移动 | 原型 keydown 已实现；未做完整焦点管理 |
| `Enter` | 打开任务 | 已实现 |
| `c/r/e/f/b/u/o/a/n/x` | 认领/续租/完成/失败/阻塞/解除/重开/指派/依赖/取消 | 已实现任务页；队列 `c` 已实现 |
| `[`/`]` | 一级状态 tabs | 已实现（原型简化为打开/完成） |
| `1–5` | 子筛选 | 设计有，原型未绑定全部数字键（见 §10 未验证） |
| `/` | 聚焦筛选 | 原型打开筛选 Sheet |
| `?` | 快捷键帮助 | `task-1440-dialog-shortcuts.png` |
| `Esc` | 关闭/返回队列 | 已实现 |
| `Cmd/Ctrl+Enter` | 发送回复 | 已实现 |
| `Space` | 切换 Switch | 原生 button/switch 焦点键；未单独测 |
| 输入框聚焦 | 单字母动作失效 | 已实现 |

- 所有动作有可见文字；图标按钮有 `aria-label`；状态 badge 有文字；无 hover-only 信息。
- 对话框有标题、焦点进入第一个字段、Esc 关闭；完整 Tab 焦点陷阱未在原型中实现（生产用
  shadcn Dialog 自带）。
- 焦点环全局一条规则；TUI 无色终端仍可读（文字 + 图标）。

## 8. 响应式与几何

| 断点 | 结果 |
| --- | --- |
| 1440 | 208 rail；50 张 1440 帧；队列四列；任务双栏 1fr + 340 sticky；sync/automation 宽卡 |
| 768 | 60 icon rail；6 张 768 帧；队列保留列头与四列；任务卡两栏摘要 → 线程；动作 34px |
| 390 | 无 rail，底部四 tab；15 张 390 帧；队列卡片行、筛选 Sheet、任务卡动作 full width、Composer 吸底 |
| 1024–1279 | 设计沿用；任务双栏在 <1180 折叠；sync 指标 2×2 |
| 溢出 | 74/74 `scrollWidth == clientWidth`，无横向滚动 |
| 触控 | 手机字段/按钮/Sheet 44px；桌面 34px；行可点高度 ≥68/88 |
| Composer | 390 吸底在底部 tab 之上，内容区留 96px padding；`task-390-composer.png` 可见 |

- 780 与 1024 未单独截图；768 + 1440 覆盖两端，1024 的规则是同一套（见 DESIGN §11）。
- 未在真机验证 iOS 安全区/软键盘；CSS 只用 `env(safe-area-inset-*)` 设计值。

## 9. 待确认（按优先级）

### P0 — 进入实现前必须定

1. **认领凭据丢失后的恢复（G4）**
   - 现状：刷新/换标签页后 token 丢失；holder 不能 complete/renew；owner 也不能强制 release。
     设计只能给出“到期后重新认领”的诚实态（`task-1440-claim-lost.png`）。
   - 需要：`POST /tasks/{id}/release` 允许 owner/manage（无 token，审计记录）或
     `claim {takeover:true}`（同 Actor）。否则“浏览器刷新”会卡住 15 分钟。
2. **`task.read` → `board.read` 的迁移/引导（G10）**
   - 只拥有 `task.read` 的账号上线后会看不到 02 任务；这是破坏性变更的一部分。
   - 需要：默认角色 bootstrap、迁移公告或一次性授权流程；在发布说明写清“旧 task.read 不再打开任务章节”。
3. **归档 Ticket 的可见性（G11）**
   - 旧看板删除后，若适配器“跳过归档”，用户将没有入口查看已归档工单；这与审计/回溯需求冲突。
   - 需要：镜像归档 Ticket（只读）或提供归档导出/只读页；不能默认“跳过”且删 UI。
4. **权威 `moves` / `mirror` / bridge 状态（G1）**
   - 当前客户端按 §6.1 保守镜像；服务端规则变化会漂移；镜像任务是否只读、是否可回答，
     不能只靠 `source=` 标签判断。
   - 需要：任务视图带 `moves` 与 `mirror {source, readOnly, bridge}`（或 `/me` flags）。

### P1 — 实现前补齐/确认

5. **状态计数聚合（G2）**：无 `summary` 时 tabs 不显示数字；有则显示真实总数，不用本页冒充。
6. **更早回复游标（G3）**：当前只有 `after`；`加载更早的回复` 需要 `before` 或 `headCursor`。
7. **消息来源/副本（G5）**：线程“来自 Slack/GitHub”、任务卡外部副本、retry 状态需要
   `origin` + `copies`；否则同步条无法显示。
8. **watcher/schedule 启停 API（G6）**：`PATCH /watchers/{id} {enabled}`、
   `PATCH /schedules/{id} {status:paused|pending}`、最近投递；确认 status 词表。
9. **两个既有对比度问题**：
   - dark `destructive-foreground #ffffff` on `#ff5c5c` = 3.03:1（按钮文字 AA 不达标）；
     建议 kit 将 dark destructive-foreground 改为深色（`#171717` 实测约 6.0:1）或新增 soft destructive。
   - light `muted-foreground #6e6e6e` on `#ededed` = 4.36:1；建议 `#666666` 或只用于非正文。
   - 这是 token 层问题，本设计不私改；需要 kit owner 决策。
10. **频道访问管理 API（G8）**：频道 owner 在 UI 里授权 viewer/editor/owner；当前 Sheet 只展示
    现状，没有 API 时不能编辑。
11. **`for=pool` / actors 形状（G9）**：筛选“池中”需要服务端约定（`for=pool` 或空值）；
    `/actors` 需要 account/bot 显示名。
12. **任务链接/来源（G12）**：来源行、镜像只读、归档状态建议 `task.links[]`，不要只靠标签。
13. **终态排序/分页**：最早优先对 open 合理；completed/failed 的“最早优先”会让最近记录
    需要翻很多页。是否对终态改为“最新优先”或增加 `before`？需要 Owner 定。
14. **`answer` 的审批选项**：当前只做自由文本；superagent Attention 的 options 是否投影到 board
    尚未定义。

### P2 — 可在实现中迭代

15. **同步/自动化故障计数语义**：header `同步 2` 当前 = 有凭据失效的 entry + 有死信的 entry；
    是否改为“故障条数”需统一。自动化同理。
16. **旧 `/superagent` 的 404 文案**：对没有 `board.read` 的用户，主操作应改“返回工作区”；
    当前原型固定“打开任务队列”。
17. **Board disabled 时章节是否隐藏**：当前直接 URL 显示未启用页；shell 是否也从 rail 隐藏
    02 任务，需要产品决定。
18. **浅色主题截图**：令牌支持浅色，但本轮只截深色；实现后需补浅色五态。
19. **Terminal state 行的 result/reason**：当前只显示首行，长结果在任务卡展开；是否需要在行内
    提供“查看全文”链接需确认。

## 10. 未验证 / 未交付（诚实边界）

| 项 | 状态 |
| --- | --- |
| 真实 nsh TSX / TUI 渲染 | **未实现**；本切片只有设计与静态 HTML。TUI 等价是设计映射，不是实测。 |
| 真实 keel-board API / SSE | **未接入**；原型用 fixture，工具条标注“设计样例”。生产不得用假数据。 |
| 完整键盘焦点陷阱 / Tab 顺序 | 原型只实现动作 keydown；生产应使用 shadcn Dialog/Tabs 的焦点管理。 |
| 真机 iOS/Android 安全区、软键盘 | 只用 CSS `env(safe-area-inset-*)` 设计；未真机验证。 |
| 浅色主题截图 | 令牌已支持，本轮未截。 |
| 780/1024 中间断点 | 未单独截图；768 + 1440 覆盖两端。 |
| OpenMotion 预览公网可达 | 本机 200 校验通过；地址是 `127.0.0.1`，不是 Owner 交付链接。 |
| 独立视觉评审 | 未做；本自检不计独立评审。 |
| 对比度门限的自动化 | 已用脚本测令牌对；未接入构建门禁（生产需 `ui-experience-gate` A 层）。 |

## 11. Owner 需要拍板的 5 件事

1. **刷新丢 claim**：接受“等到期”，还是让 owner/manage 强制 release / 同 Actor takeover？
2. **权限迁移**：是否为启用 board 的租户默认授予 `board.read`（以及哪些角色拿 write/manage）？
3. **归档工单**：旧看板删除后，归档 Ticket 是否必须只读可见？如果需要，适配器不能“跳过”。
4. **同步/自动化范围**：本版只做观察、启停、重试、死信；不提供 entry 创建/凭据/URL 编辑——
   确认这是产品边界（与“不嵌入 OAuth 安装”一致）。
5. **终态排序**：`completed/failed/canceled` 是否仍坚持最早优先，还是改为最新优先/双向分页？

## 12. 反模式清单（ui-experience-gate）

- [x] 没有把假数据写进生产路径；原型 fixture 在工具条与 DESIGN 中标注。
- [x] 没有在组件里硬编码 hex；`var(--token)` 统一。
- [x] 没有死状态文案：loading/empty/error/permission/success 各有专属文案与动作。
- [x] 没有把测试 id / fixture 词写进产品正文；只出现任务 ID、request id、频道、actor。
- [x] 没有 hover-only 信息；状态/动作都有文字。
- [x] 没有横向溢出（74/74 measured）。
- [x] Composer 未被底部 tab 遮挡；内容区有底部留白。
- [x] 没有“删除/停用”无确认的高危动作；完成/失败/阻塞/取消/释放/重开/指派/改依赖/回答有确认。
- [x] 没有双看板/兼容开关；旧 `/superagent` 明确 404。
- [ ] 对比度：2 个既有 token 对不达标（§9 P1-9）；未整改，不得表述为已通过。
- [ ] 独立视觉评审：未做；不得表述为已通过。
- [ ] TUI 实测：未做；不得表述为已通过。

## 13. 复现

```bash
cd openspec/changes/keel-board-v1/ui/prototype

# 截图（需要本机 Chrome + Playwright；不提交 node_modules）
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs

# 交互断言
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node interact.mjs

# 只跑一张
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node shoot.mjs --only queue-1440-success
```

原型 URL 参数见 `DESIGN.md` §14.1；`shoot.mjs` 内是 74 张图的完整矩阵。

## 14. 文件清单（本切片）

```text
openspec/changes/keel-board-v1/ui/
├─ DESIGN.md
├─ REVIEW.md
└─ prototype/
   ├─ index.html
   ├─ shoot.mjs
   ├─ interact.mjs
   └─ shots/           (74 PNG)
```

- 未改 `web/src/**`、`internal/**`、`cmd/**` 或任何生产文件。
- 未提交 `node_modules`、浏览器缓存、临时 ZIP（`/tmp/keel-board-prototype.zip` 仅用于导入
  OpenMotion，未入库）。
