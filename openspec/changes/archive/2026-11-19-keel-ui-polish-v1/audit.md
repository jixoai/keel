# keel-ui-polish-v1 · 界面评审

方法：`better-interface` 编排，按 `better-accessibility` → `better-layout` → `better-writing` →
`better-typography` → `better-colors` → `better-ui` 的顺序逐域评。技能集来自
<https://jakub.kr/skills>（`better-interface`、`better-ui`、`better-typography`、`better-colors`、
`better-accessibility`、`better-layout`、`better-writing`）。

## 范围

- 评审对象：`web/src/screens/**`（工作区、会话列表、会话面板、编辑器、模型选择器、设置、插件、登录、
  命令面板、状态栏）与 `web/src/ui/kit/**`（组件 + layout blocks）的既有呈现。
- 覆盖状态：默认 / 焦点 / 禁用 / 空 / 加载 / 错误 / 窄栏（对照 kit-gate 的 390-768-1280 三档）。
- 明确排除：`web/src/surfaces/**` 与 `web/src/data/**`（主线正在改，本 change 不碰），
  以及服务端文案。

## 证据来源

| 类型 | 位置 |
|---|---|
| 运行态截图（深色） | `/tmp/ui-polish/03-workspace.png`、`04-session.png`、`05-model-picker.png`、`06-settings.png`、`07-focus-input.png` |
| 运行态截图（浅色） | `/tmp/ui-polish/13-light-settings.png`、`14-light-workspace.png`、`14-light-empty-crop.png` |
| 对比度实测 | 脚本按 WCAG 2.2 相对亮度公式对 `web/theme.json` 的渲染配对计算（数值列在每条「证据」里） |
| 键盘走查 | `agent-browser` 在 `https://local-gaubee-keel-app.fde2e.com/` 上 Tab 步进，读 `document.activeElement` 与 `getComputedStyle` |
| 基线门禁 | `nsh check` 绿；`nsh test --target tui` 82/82 通过（1m51s）；改动后终态 86/86（主线在其间新增 4 条） |

> 注 1：本次未取到 320px 视口的运行态画面（`agent-browser` 当前版本无 viewport 尺寸命令），
> 相关条目标为「未覆盖」，不作为已判定缺陷。
>
> 注 2：为让运行态截图反映改动，`nsh package --target web` 之后用 `scripts/serve-local.sh`
> 重启了 13760 的评审服务本地进程（`/tmp/keel-app serve`）。服务已恢复并保持 200。
> 该服务是评审用本地进程，不属主线的 `keel serve` 实例。

## 汇总

- 共 **23** 条：P0 **4** 条（A1–A4）、P1 **8** 条（B1–B8）、P2 **11** 条（C1–C7 与 D1–D4）。
- 处置（逐条状态见「实施结果」，未做项见「未做项」）：
  - **完全实施 11 条**：A1、A2、A3、B1、B2、B4、B5、B6、B7、B8、C1（D4 并入 B6 文件）。
  - **部分实施 2 条**：A4（设置导轨已修；展开条与「回到底部」受文件额度限制）、
    D2（`menuRow` 已改用令牌；dialog 关闭键那半为腾额度已回滚）。
  - **未做 7 条**：A4 余项、B3、C3、C4、C6、C7、D1 —— 每条附原因。
  - **判定无需改 3 条**：C2、C5、D3。
- 改动上限 12 个源码文件，实际用满 12；未新增任何依赖、令牌值或接口。

---

## P0 · 阻断（键盘不可达 / 焦点不可见 / 文本对比度不达标）

### A1 · 焦点环在深色外观下几乎不可见（全部可聚焦控件）

- **维度**：可访问性（WCAG 2.2 · 1.4.11 非文本对比 3:1；2.4.13 焦点外观）
- **证据**：`web/src/ui/kit/components/styles.tsx:27-30` 的 `focusRing()` =
  `borderColor: color.ring` + `boxShadow: 0 0 0 2px alpha(color.ring, 0.35)`。
  `color.ring` 在深色是 `rgba(255,255,255,0.15)`，再乘 0.35 → 有效 alpha **0.0525**；
  合成到 `#161616` 上实测 **1.14:1**（对 panel `#202020` 1.03:1），远低于 3:1。
  浏览器实测（`agent-browser`，深色）：聚焦按钮 `boxShadow: rgba(255,255,255,0.05) 0px 0px 0px 2px`。
  浅色同样失败（有效 alpha 0.07，**1.16:1**）。
  截图：`/tmp/ui-polish/07-focus-input-crop.png`（聚焦的搜索框，环只能靠边框亮度分辨）。
- **影响面**：`focusRing()` 是 kit 的唯一焦点环，被 `button.tsx:124`、`input.tsx:67`、
  `textarea.tsx:55`、`select.tsx:136`、`combobox.tsx:101`、`collapsible.tsx:87`、
  `accordion.tsx:101`、`sheet.tsx:105`、`dialog.tsx:102`，以及 `screens/session-list.tsx:200` 使用。
- **建议**：焦点环改用 `color.foreground` 派生的高对比描边（`alpha(color.foreground, 0.65)`
  实测深色 5.78:1 / 浅色 3.73:1，覆盖 background / panel / popover 三种底色），
  并保留 `borderColor: color.ring` 作为静止态的边框色不动。
- **实施**：是（A1 已在 `styles.tsx` 修正）。

### A2 · 会话行的溢出操作只在指针悬停时出现，键盘用户拿不到「删除会话」

- **维度**：可访问性（WCAG 2.2 · 2.1.1 键盘；`better-interface` 升级触发项「指针可达而键盘不可达」）
- **证据**：`web/src/screens/session-list.tsx:252` — `<If when={props.wide && (hovered() || props.active)}>`
  包住整个 `DropdownMenu`；`hovered` 只由 `onMouseEnter` / `onMouseLeave`（同文件 189-190）写入，
  键盘焦点进入行时不触发。行本身 `focusable`（183 行），但 Tab 到行上时隐藏的菜单按钮不在 tab 序里。
  `04-session.png` 中「⋯」只在被悬停/选中的行上出现。
- **影响面**：删除会话的**唯一**键盘路径退化为右键 `ContextMenu`（`context-menu.tsx` 依赖
  shift+F10 或右键），在一个 web 界面上不可发现。
- **建议**：把显示条件加上「行内有焦点」——`focused` 信号由行的 `onFocus` / `onBlur` 驱动
  （NSH 的 `View` 已有 `onFocus` / `onBlur`），条件改为
  `props.wide && (hovered() || focused() || props.active)`。
- **实施**：是（不改数据流，仅多一个呈现开关）。

### A3 · 错误/警告徽章的彩色文字在浅色外观下对比度仅 2.46–3.85:1

- **维度**：颜色（WCAG 2.2 · 1.4.3 文本 4.5:1）
- **证据**：`web/src/ui/kit/components/badge.tsx:34-53` 的 `badgeTint()` 把 ink 取为语义色本身，
  face 取同一色的 tint。实测（ink 在 face 上）：
  浅色 warning **2.46**、success **2.77**、info **3.38**、destructive **3.85**；
  深色 info **3.83**、destructive **3.78**（warning/success 4.56/4.54 勉强过线）。
  同一批 face 上改用 `color.foreground` 实测 **8.26–8.72**（浅） / **7.19–7.73**（深）。
  运行态可见：`03-workspace.png` 里「失败」徽章、`04-session.png` 的「未连接 / 连接中」。
- **影响面**：`badge.tsx` 的 `destructive` / `success` / `warning` / `info` 四档全部；
  被 `session-list.tsx:224`（行状态）、`session-list.tsx:489`（连接状态）、
  `messages.tsx`、`settings.tsx`、`model-picker.tsx` 使用。
- **建议**：ink 改用 `color.foreground`，语义色只留在 face tint 与边线上（颜色不再独自承载语义，
  徽章本身已有 `icon` 与文字）。图标 tint 同步跟 ink，避免图标比文字更亮。
- **实施**：是（`badge.tsx`）。

### A4 · 四处 `focusable role="button"` 的 kit 布局控件完全不画焦点环

- **维度**：可访问性（WCAG 2.2 · 2.4.7 焦点可见 / `better-accessibility` 升级触发项
  「可聚焦控件无可见焦点指示」）
- **证据**：这四处是 `View` + `role="button"` + `focusable`，但它们**没有传 `focusStyle`**，
  所以既不走 `focusRing()`，宿主也不补默认环：
  `web/src/ui/kit/layout/settings-shell.tsx:264-272`（设置页左侧导轨的八个入口）、
  `web/src/ui/kit/layout/sidebar.tsx:222-232`（收起后的 4px 展开条）、
  `web/src/ui/kit/layout/composer-dock.tsx:153-160`（「回到底部」圆钮）、
  `web/src/ui/kit/layout/workspace-shell.tsx:274`（抽屉背板，故意不给——它是遮罩，符合预期）。
  实测：Tab 停在设置导轨的「能力」上，`getComputedStyle(activeElement).boxShadow === "none"`。
- **影响面**：设置页导航是核心路径；键盘用户在这条导轨上完全看不到焦点位置。
- **建议**：三处（导轨、展开条、回到底部）各自补 `focusStyle={focusRing()}`；
  背板保持无环（它不是可操作入口，只负责关抽屉）。
- **实施**：**部分 —— 导轨 [已实施]；展开条与「回到底部」[未做 + 原因]**。
  - [已实施] `web/src/ui/kit/layout/settings-shell.tsx:272-277` 补 `focusStyle={focusRing()}`
    （并 import `focusRing`）；这是四处里唯一的**核心导航**，优先保住。
    **运行态已验**：Tab 到导轨「关于」，`getComputedStyle().boxShadow ===
    "rgba(212, 212, 212, 0.65) 0px 0px 0px 2px"`（改前为 `none`）；
    截图 `/tmp/ui-polish/25-after-rail-focus-crop.png`。
  - [未做 + 原因] `sidebar.tsx` 的 4px 展开条与 `composer-dock.tsx` 的「回到底部」圆钮：
    各需新增 1 个文件，会突破 12 文件额度（原额度已满，为腾位已回滚 `dialog.tsx` 的 P2 改动）。
    下一轮首个修，各一行。
  - `workspace-shell.tsx:274` 的抽屉背板**保持无环**——它是关闭遮罩，不是可操作入口，
    `focusable` 也未设，符合预期，不算缺陷。

---

## P1 · 应修（可读名称 / 布局 / 文案 / 排版）

### B1 · 可读名称与可见文字不一致：「保存并生效」的可读名是「保存模型配置」

- **维度**：可访问性（WCAG 2.5.3 标签在名称中；`better-accessibility` 检测项
  「`aria-label` 漏掉可见标签」）
- **证据**：`web/src/screens/settings.tsx:782-788` — `label="保存并生效"` 与
  `accessibilityLabel="保存模型配置"` 不含可见文字。语音控制用户说「保存并生效」点不到。
- **影响面**：单点，但直接破坏 WCAG 2.5.3。
- **建议**：让可见标签与可读名**同为一个动词 + 对象**的短句。既有测试按
  `label:保存模型配置` 定位（`App.tui.test.tsx:105,118`），所以保留该字符串、把它提升为
  可见标签，同时把「立即生效」交给上方卡片已有的文案承担。
- **实施**：是 —— 可见标签与可读名都改为「保存模型配置」（`settings.tsx:781-796`），
  `ProviderEditorCard` 的 description 里的引用同步改为「按上方「保存模型配置」生效」，
  `web/README.md:124` 的对应记述同步。基线 82/82 通过（含两处 `label:保存模型配置` 断言）。
- **残余**：这是一次**文案改名**（可见文字变化），仍属「不改行为、不改信息架构」——
  动作、位置、启用条件都没变。

### B2 · 登录页口令错误没有给下一步

- **维度**：文案（`better-writing`：错误必须说怎么修）
- **证据**：`web/src/data/store.ts:386-388` 把 401 映射为 `"口令不正确"`；
  `web/src/screens/login.tsx:54` 直接把该串喂给 `Field` 的 error。
  一句「口令不正确」只说明失败，没说去哪拿正确口令。
- **影响面**：登录是每个用户的入口，失败即卡死。
- **建议**：两条一起做——(1) 常驻 `hint` 说明口令从哪来（`KEEL_SERVE_PASSWORD`）；
  (2) 401 那一句在**呈现层**补上下一步，store 不动（`src/data/**` 属主线，本 change 禁改），
  其余错误（网络、5xx）仍原样透出。
- **实施**：是 —— `web/src/screens/login.tsx` 的 `error()` 把 `"口令不正确"` 映射为
  「口令不正确。请向管理员或启动服务的终端确认 KEEL_SERVE_PASSWORD，再重新输入。」，
  `hint` 改为「口令由启动服务的人设置（服务端环境变量 KEEL_SERVE_PASSWORD）」。
  其余错误分支 `return message` 原样返回，行为不变。

### B3 · 命令面板空结果只教换词，没有一键清空的出口

- **维度**：文案（`better-writing`：空态指向下一步）
- **证据**：`web/src/screens/command-palette.tsx:236-243` 的空态
  「换个关键词试试：会话、模型、设置、插件。」只教了怎么换词；原生 command 的 Escape
  在有查询时先清查询、需要第二次才关闭（见该文件头注释），所以空结果时用户只能逐字退格。
  对照：`session-list.tsx:326-337` 的同类空态已经给了「清除搜索」按钮，两处不一致。
- **影响面**：命令面板是全局 Mod+K 入口，空结果是最常见的分支之一。
- **建议**：`empty` 槽里补一个「清除搜索」的次级按钮（走已有的受控 `paletteQuery`）。
- **实施**：否 —— 补按钮会新增一个交互控件（超出「不改行为」的边界），记入未做项。

### B4 · 侧栏顶部三枚图标按钮的间距低于 kit 的无边框控件间距下限

- **维度**：布局（`better-layout`：无边框控件相关字形之间需更大留白）
- **证据**：`web/src/screens/session-list.tsx:360-372` 的 header 用 `gap: space.sm`（6px）
  串起 logo、标题块与三枚 28px 的 ghost IconButton；按钮自身无边框无底色，
  相邻字形净距 ≈ 6px + 各自内边距，低于技能给的 24px（无密度系统时的起点），
  也低于项目自身的 `space.lg`（12px）控制内距阶梯。`03-workspace.png` 顶部三枚图标读成一片。
  改后对照：`/tmp/ui-polish/24-after-header-crop.png`。
- **影响面**：一眼扫不到三个独立目标；误触「刷新会话」时按到「新建会话」的代价是新建垃圾会话。
- **建议**：三枚按钮各自包一层 `gap: space.md` 的行容器（视觉净距 8px+），
  与标题块之间保留 `flex: 1` 的弹性间隔——只改间距，不改控件。
- **实施**：是（header 右侧三枚按钮收进 `gap: space.md` 的组）。

### B5 · 会话行的字号阶梯在 10px 处触底，低于 UI 文本 12px 下限

- **维度**：排版（`better-typography`：UI 文本很少低于 12px）
- **证据**：`web/src/screens/session-list.tsx:251-258` 时间戳用 `typeScale.xs`（10px）；
  同类：`plugins.tsx:264` 表头 `headText()`、`plugins.tsx:267-272` 的 `Fact` 标签、
  `session-status-pane.tsx:64` 的 fact 标签、`session-list.tsx:415` 的 9px 副标题
  「AI 员工会话」。实测渲染：`getComputedStyle` 读到 `9px/11px`、`10px/12px`。
  10px 中文（「43 分钟前」）在 1 倍缩放下笔画已成糊状（`14-light-sidebar-crop.png`）。
- **影响面**：`typeScale.xs` 共 81 处、`typeScale["2xs"]` 共 13 处引用，跨 8 个文件。
- **建议**：把**承载信息**的 9–10px 文本升到 `typeScale.sm`（12px）；纯装饰（kbd 键帽内字符、
  头像角标、`mono` 的 wire id）保留。逐处判定，不做全局替换。
- **实施**：部分 —— 升了 5 处承载信息的：会话行时间戳、侧栏副标题、状态栏 fact 标签、
  插件表头、插件 `Fact` 标签。改后实测 `12px/15px`
  （`/tmp/ui-polish/24-after-header-crop.png`）。其余（`messages.tsx` 等）留给下一轮，见「未做项」。

### B6 · 长段落无行高约束，中文长句挤在一起

- **维度**：排版（`better-typography`：三段以上必须 ≥1.4 行高）
- **证据**：`web/src/screens/model-picker.tsx:206` 的模型说明行用 `typeScale.xs` +
  `lineHeight.normal`（1.5，正确），但 `model-picker.tsx:350` 的分组名、
  `settings.tsx:1131` 的空态说明、`session-list.tsx:415` 的副标题都没有 `lineHeight`，
  落到宿主默认（实测 `17px` for 14px 文本 = 1.21，`15px` for 12px = 1.25），
  低于 1.4。kit 的 `EmptyDescription`（空态说明）与 `CardHeader.description`
  （卡片说明）是同类问题的共享源头。
- **影响面**：全部未显式给 `lineHeight` 的多行说明文本；kit 三处是共享件。
- **建议**：kit 的 `EmptyDescription`、`CardHeader.description`、`Field.hint` 三处统一给
  `lineHeight: Math.round(size * lineHeight.normal)`（1.5），一处修，全站生效。
- **实施**：是 —— `empty.tsx`（`EmptyDescription`）、`card.tsx`（`CardHeader.description`）、
  `field.tsx`（hint 与 error 两行）。

### B7 · 会话行的时间戳与标题争夺宽度，长标题下时间被挤成「11月13日 2…」

- **维度**：布局（`better-layout`：截断内容必须有到达完整值的路）
- **证据**：`web/src/screens/session-list.tsx:231-247` 第二行是
  `<Highlighted flex:1 ellipsis>` + `<Text>{time()}</Text>`；时间戳没有 `flexShrink: 0`，
  在 264px 列里两侧同时被压缩。`03-workspace.png` 的「11月13日 21:53」在当前宽度下完整，
  但在更窄的列（或较长预览）会先压时间。
- **影响面**：侧栏全列表。
- **建议**：给时间戳 `flexShrink: 0`（时间必须完整可读，预览可截断），
  预览保持 `flex: 1` + `ellipsis`。
- **实施**：是。

### B8 · Toast 的关闭控件是一个「×」字符，无焦点环、无 24px 命中区

- **维度**：可访问性（WCAG 2.2 · 2.4.7 焦点可见 / 2.5.8 目标尺寸 24px）
- **证据**：`web/src/ui/kit/components/toast.tsx:107-114` — `<Text role="button" onPress=…>×</Text>`。
  `Text` 在 NSH 里不接受 `focusStyle` / `focusable` / `hitSlop`（
  `.nsh/types/nsh/types.d.ts` 的 `TextHostProps` 只有 `allowFontScaling` 等五项），
  因此这个控件**无法**参与 Tab 序，也没有可见焦点。
  它的可读名「关闭提示」是既有测试的定位点（`overlay.test.tsx:58`）。
- **影响面**：所有 toast（保存成功、上传失败等）。
- **建议**：换成 kit 的 `IconButton`（`icon="x"`，`label="关闭提示"`，`size="sm"`），
  位置与可读名不变，测试定位点不变。
- **实施**：是（保留 `label:关闭提示`，仅把 `Text` 换成 `IconButton`）。

---

## P2 · 打磨

### C1 · 「AI 员工会话」用 9px（`typeScale["2xs"]`）做副标题

- **证据**：`web/src/screens/session-list.tsx:415`。9px 中文在浅色下笔画几乎连成块
  （`14-light-sidebar-crop.png`）。
- **建议**：升到 `typeScale.sm`（12px），与 `mutedForeground` 一起仍读作次要信息。
- **实施**：[已实施]（已并入 B5 那批；改后 `24-after-header-crop.png`）。

### C2 · 模型选择器里「思考档」的四个单选未分组的可读名

- **证据**：`web/src/screens/model-picker.tsx:214-220` 用 `RadioGroup`
  配 `accessibilityLabel={`思考档 ${model}`}`；中文读出来是「思考档 DeepSeek V4.1 Flash 关」，
  选项名只有「关/低/中/高」，语义靠分组名。分组名已提供，可接受。
- **建议**：把选项标签写成「思考档 关」级别会与分组名重复，保持现状；`orientation` 已正确。
- **实施**：**不改**（判定无缺陷）。

### C3 · 「恢复默认」在无会话选择时被禁用，但无解释

- **证据**：`web/src/screens/model-picker.tsx:430` `disabled={!session()}`，
  旁边有 tooltip「清除本会话的模型选择」，但 tooltip 在 `disabled` 的原生控件上不显示
  （`better-accessibility` 检测项：禁用的原生控件上的 tooltip）。
- **建议**：保留禁用（行为正确），把提示改为可见的一行说明，或改用 `aria-disabled` 保持可聚焦。
- **实施**：**[未做 + 原因]** 需改交互形态（disabled 原生控件 → `aria-disabled`），
  超出「不改行为」的边界。

### C4 · 设置页大量 `typeScale.xs` 用于表单辅助文字

- **证据**：`web/src/screens/settings.tsx` 的 `hint` 由 `Field` 渲染为 `typeScale.sm`（12px，正确），
  但页内自绘的说明行仍有 10px。逐处调整会触及信息层级，超出「不改信息架构」的边界。
- **建议**：下一轮 change 统一收敛；本轮已把三处共享件（`Field.hint`、`EmptyDescription`、
  `CardHeader.description`）修好。
- **实施**：**[未做 + 原因]** 超范围（逐处升降字号会改变信息层级）。

### C5 · 会话列表的骨架屏只有前两块带可读名

- **证据**：`web/src/screens/session-list.tsx:282-291` 第一块 `accessibilityLabel="正在加载会话"`，
  其余三块无标签。
- **建议**：只保留第一块的可读名即可（避免重复播报），现状符合「一个区域一个名字」。
- **实施**：**不改**（判定无缺陷）。

### C6 · 连接状态徽章把「未连接」染成 destructive

- **证据**：`web/src/screens/session-list.tsx:480-487`。`03-workspace.png` 右下「未连接」是红底，
  与「失败」徽章同色系，但语义是「尚未连接」而非「出错」。
- **建议**：`offline` 用 `secondary`（中性），把 `destructive` 留给真正的失败。
- **实施**：**[未做 + 原因]** 属语义映射判定（`offline` 是否算错误态），需主线/Owner 确认。

### C7 · 命令面板空态缺一键清空

- **证据**：`web/src/screens/command-palette.tsx:236-243`。
- **建议**：见 B3。
- **实施**：**[未做 + 原因]** 同 B3，补按钮属新增交互。

### D1–D4 · 细节

- **D1**：`messages.tsx` 的运行统计数字缺 `tabular-nums`。
  **实施**：**[未做 + 原因]** 文件额度已满，且属跨文件细节。
- **D2**：`styles.tsx:82` 的 `menuRow()` 高度写死 `28` → 改用 `controlHeight("default")`。
  **实施**：[已实施]（部分）—— `dialog.tsx` 关闭键那半**已回滚**以腾出文件额度给 A4。
- **D3**：`composer.tsx` 的窄栏布局。
  **实施**：**不改**（`compact` 下提示本就隐藏，布局正确）。
- **D4**：`field.tsx` hint 三元的两个分支相同（死代码）。
  **实施**：[已实施]（删掉三元）。

---

## 未做项

| 条目 | 严重度 | 原因 |
|---|---|---|
| **A4 余项**：`sidebar.tsx:222` 展开条、`composer-dock.tsx:153`「回到底部」的焦点环 | **P0** | 各需新增 1 个文件；12 文件额度已满。**下一轮第一优先**，各一行 `focusStyle={focusRing()}`。 |
| B3 / C7（命令面板空态补「清除搜索」按钮） | P1 / P2 | 新增一个交互控件，超出「不改行为」的边界。 |
| C3（「恢复默认」禁用态无解释） | P2 | 需把 disabled 原生控件换成 `aria-disabled` 才能显示提示，属交互形态变更。 |
| C4（设置页残余的 10px 辅助文字） | P2 | 逐处升降字号会改变信息层级，超出本 change 范围；共享件（Field/Empty/CardHeader）本轮已修。 |
| C6（连接状态配色语义） | P2 | 语义映射判定，需主线/Owner 确认 `offline` 该不该算 destructive。 |
| D1（`messages.tsx` 用量数字 tabular-nums） | P2 | 文件额度已满；且属跨文件细节。 |
| D2 的 dialog 半部（关闭键 24 → `controlHeight`） | P2 | 已回滚，用该文件额度换取 A4 的设置导轨焦点环。 |
| 320px 视口 / 200% 缩放实测 | – | `agent-browser` 当前版本无 viewport 尺寸命令；标为**未覆盖**，不作为已判定缺陷。 |

---

## 实施结果

状态标记：`[已实施]` / `[未做 + 原因]`。行号指**改动后**的位置。

| 编号 | 状态 | 文件:行 | 改动（一句话） | 依据 |
|---|---|---|---|---|
| A1 | [已实施] | `ui/kit/components/styles.tsx:35-42` | `focusRing()` 的环改为 `alpha(color.foreground, 0.65)`；静止态 `borderColor` 仍为 `color.ring` | 环对底色 5.44–5.78:1（深）/ 3.66–3.80:1（浅），过 1.4.11 的 3:1 |
| A2 | [已实施] | `screens/session-list.tsx:174-179, 197-199, 262-264` | 行加 `focused` 信号（`onFocus`/`onBlur`），溢出菜单显示条件加 `focused()` | 键盘可达 2.1.1 |
| A3 | [已实施] | `ui/kit/components/badge.tsx:34-57` | 四档语义徽章的 label ink 改用 `color.foreground`；语义色只留在 face / 边线 / 图标 | ink 2.46–3.85 → 7.19–8.72 |
| A4 | 部分 | `ui/kit/layout/settings-shell.tsx:272-277` | 设置导轨补 `focusStyle={focusRing()}`，运行态已验证 | 2.4.7 |
| A4 余项 | [未做 + 原因] | `layout/sidebar.tsx:222`、`layout/composer-dock.tsx:153` | 展开条与「回到底部」的焦点环 | 需 +2 文件，超 12 文件额度 |
| B1 | [已实施] | `screens/settings.tsx:781-796`、`web/README.md:124` | 可见标签与可读名统一为「保存模型配置」；引用同步 | WCAG 2.5.3 |
| B2 | [已实施] | `screens/login.tsx:28-42, 62` | 常驻 hint 说明口令来源；401 错误串在呈现层补「下一步」 | `better-writing`：错误说下一步 |
| B3 | [未做 + 原因] | – | 命令面板空态补「清除搜索」按钮 | 新增交互控件，超「不改行为」边界 |
| B4 | [已实施] | `screens/session-list.tsx:415-429` | 头部三枚图标按钮收进 `gap: space.md` 的组 | `better-layout` 无边框控件留白 |
| B5 / C1 | [已实施] | `screens/session-list.tsx:251-262, 414` | 时间戳 10→12px、副标题 9→12px | `better-typography` ≥12px |
| B5 | [已实施] | `screens/session-status-pane.tsx:64` | fact 标签 10→12px | 同上 |
| B5 | [已实施] | `screens/plugins.tsx:264, 271` | 表头与 `Fact` 标签 10→12px | 同上 |
| B6 | [已实施] | `ui/kit/components/empty.tsx:87-104`、`card.tsx:85-96`、`field.tsx:63-92` | 说明/hint/error 三处给 1.5 行高 | `better-typography` 多行 ≥1.4 |
| B7 | [已实施] | `screens/session-list.tsx:257` | 时间戳 `flexShrink: 0`（预览保留 `ellipsis`） | 截断不吃必读值 |
| B8 | [已实施] | `ui/kit/components/toast.tsx:105-115` | 「×」`Text` 换 kit `IconButton`（可读名仍「关闭提示」） | 2.4.7 / 2.5.8 |
| C2/C5/D3 | 不改 | – | 判定无缺陷（分组名已给出语义；骨架屏一个区域一个名字；compact 下提示已隐藏） | – |
| D2 | 部分 | `ui/kit/components/styles.tsx:82` | `menuRow` 高度改用 `controlHeight("default")`；**dialog 关闭键那半已回滚以腾文件额度** | 禁魔法数 |
| D4 | [已实施] | `ui/kit/components/field.tsx:80-90` | 删掉 hint 的同值三元（死代码） | 清理 |

**改动文件数：12（源码）** —— 全部落在 `web/src/screens/**` 与 `web/src/ui/kit/**`：

```
web/src/ui/kit/components/styles.tsx        A1, D2
web/src/ui/kit/components/badge.tsx         A3
web/src/ui/kit/components/card.tsx          B6
web/src/ui/kit/components/empty.tsx         B6
web/src/ui/kit/components/field.tsx         B6, D4
web/src/ui/kit/components/toast.tsx         B8
web/src/ui/kit/layout/settings-shell.tsx    A4
web/src/screens/login.tsx                   B2
web/src/screens/plugins.tsx                 B5
web/src/screens/session-list.tsx            A2, B4, B5/C1, B7
web/src/screens/session-status-pane.tsx     B5
web/src/screens/settings.tsx                B1
```

外加 `web/README.md:124` 一行（B1 的文案改名连带）。`web/src/surfaces/**` 与 `web/src/data/**`
**零改动**（主线归属）。

## 回归

| 门禁 | 结果 |
|---|---|
| `nsh check`（`web/`） | **绿**，exit 0（最终代码上重跑两次） |
| `nsh test --target tui`（全量） | **86 / 86 通过**，0 失败（2m 00s）。基线是同一天的 82 条；中间两轮分别为 84、86，增量全部来自主线新增的 `settings-capabilities.test.tsx`，与本 change 无关。本 change 的 12 个文件**没有一条测试失败，也没有回滚任何一条改动**。 |
| `nsh package --target web` | 通过 —— 仅用于截取改后画面 |

**A1 实测**（`agent-browser` 读 `getComputedStyle(activeElement).boxShadow`，在公网入口上 Tab 步进）：

| 外观 | 焦点环实测值 | 对 background | 对 panel | 对 card/popover |
|---|---|---|---|---|
| 深色（改后） | `rgba(255,255,255,0.65) 0 0 0 2px` | **5.78:1** (`#161616`) | **5.44:1** (`#202020`) | **4.96:1** (`#2b2b2b`) |
| 浅色（改后） | `rgba(64,64,64,0.65) 0 0 0 2px` | **3.73:1** (`#fafafa`) | **3.66:1** (`#f5f5f5`) | **3.80:1** (`#ffffff`) |
| 深色（改前） | `rgba(255,255,255,0.05) 0 0 0 2px` | **1.14:1** | – | – |
| 浅色（改前） | 有效 alpha 0.07 | **1.16:1** | – | – |

全部底色 ≥3:1（1.4.11），且**同一个值**覆盖两种外观与三种底色。

**视觉项截图**（`/tmp/ui-polish/`）：

- before：`03-workspace.png`、`04-session.png`、`05-model-picker.png`、`06-settings.png`、
  `07-focus-input-crop.png`、`14-light-workspace.png`、`14-light-sidebar-crop.png`
- after：`20-after-workspace-dark.png`、`22-after-focus-row.png`（+`-crop`，同时证明
  **焦点行既画出焦点环又显示「⋯」删除入口**）、`23-after-badge-crop.png`、
  `24-after-header-crop.png`、`25-after-rail-focus-crop.png`（设置导轨焦点环）
- **未覆盖**：320px 视口与 200% 缩放（`agent-browser` 当前版本无 viewport 尺寸命令）。
