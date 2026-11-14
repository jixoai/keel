# findings — trace 取证（T1）与代码对表（T3）

来源：`Trace-20261113T225129.json.gz`（Chrome DevTools TraceEvents）。解压后 14 483 185 B，**15 123 个事件**，
22 个 category，3 个进程（Browser `36020` / GPU Process `13356` / Renderer `36580`）。采样窗口极短，
且**窗口内几乎没有用户输入**（唯一一次鼠标按下在 t0=4 524.0 ms，晚于全部长任务）——因此本文中的每次重渲染都是**空转自激**，不是交互引起。

> 脱敏：本文件只写类型与位置，不写任何凭据、口令、会话 ID、用户输入文本或 trace 内的主机绝对路径。扫描与处置见 §6。

---

## 0. 证据基线

| 项 | 值 |
|---|---|
| trace 起始墙钟 | `2026-11-13T14:51:29.914Z`（`metadata.startTime`） |
| 事件时间跨度 | 4 840.7 ms（首事件 ts=155940637340 → 末事件 155945478027） |
| **本文时间基准 T0** | **首个事件 ts = 155940637340**（下文 `t0=…ms`） |
| 浏览器追踪起点 | `TracingStartedInBrowser` 在 **t0=1 231.3 ms**（= ts 0.0），有效窗口 **3 609.4 ms** |
| 换算 | `t0 − 1 231.3 = TracingStarted 相对值`；两个基准都列在 §0.1，便于对齐 DevTools 时间轴 |
| 页面 | 单主帧，`https://local-gaubee-keel-app.fde2e.com/`（本机隧道域，**非** www 生产域）；子帧 1 个 `about:blank` |
| 构建身份 | `app.4927a018ca.js`(428 529 B) + `nsh.7d929f1f29.js`(168 247 B) + `nsh.e5915f5b7b.wasm` + `browser.30ff7d2f73/*` |
| 主线程 | Renderer pid 36580 / tid 47852（`CrRendererMain`）；Compositor 48136 |
| CPU profile | 7 106 采样，自 `CpuProfiler::StartProfiling`（t0=1 231.4，572 ms —— 该段是**插桩开销**，不计入产品性能） |

### 0.1 关键事件的两种时间基准

| 事件 | t0 相对 | TracingStarted 相对 |
|---|---|---|
| 首个产品长任务（nsh pump） | 2 177.9 ms | 946.6 ms |
| 4 次整屏重渲染簇起点 | 2 589.5 / 3 174.0 / 3 968.4 / 4 504.2 ms | 1 358.2 / 1 942.7 / 2 737.1 / 3 272.9 ms |
| 4 次样式/布局重算 | 2 636.4 / 3 196.5 / 4 020.4 / 4 521.0 ms | 1 405.1 / 1 965.2 / 2 789.1 / 3 289.7 ms |
| 5 张 DevTools 截图 | 1 267.6 / 2 651.5 / 3 209.3 / 4 030.5 / 4 531.3 ms | 36.3 / 1 420.2 / 1 978.0 / 2 799.2 / 3 300.0 ms |
| 唯一一次 MouseDown / MouseUp | 4 524.0 / 4 838.7 ms | 3 292.7 / 3 607.4 ms |

---

## 1. 异常清单（按严重度排序）

| 编号 | 类别 | 严重度 | 证据（t0 时间戳 + 事件） | 代码位置 | 状态 |
|---|---|---|---|---|---|
| A01 | 运行时/主线程 | **P1（体验阻断）** | t0=2 177.9 起 **8 个 ≥200 ms 顶层长任务**（458.5 / 243.2 / 304.6 / 266.8 / **546.6** / 242.2 / 247.3 / 305.5 ms）；主线程占空比 **91.8%**（union 3 315 / 3 609 ms）；窗口内仅 **8 帧上屏（2.22 fps）、9 帧 `STATE_DROPPED`**；`AnimationFrame.blocking_duration_ms` 为 408/193/254/216/496/192/197/255 ms；5 张定时截图 **MD5 完全一致**（`699bf6d7…`）说明画面实际静止 | 触发面：`web/src/surfaces/image/index.tsx:619`（`renderGrid`）、`:628`（`onLayout` 内 `gridWidths.update`）、`:556`（`<Image src>`）；读取面：`web/src/ui/kit/tokens.ts:155-180`（token getter） | 待修（T4） |
| A02 | 产品完整性 | **P1（功能异常）** | 界面全程呈现「演示」形态：标题条 `图像生成 · 专用面板 · 演示`；常驻告警条含「图像 Provider 尚未配置，当前为演示面板…来自本地演示资产…」；每个图块角标 `v1` + `演示`；画廊内容是 3 个本地 PNG（`aurora` / `prism` / `tidal`，PaintImage 各 4 次；`bloom` 未出现） | 该构建 → `web/src/surfaces/image/index.tsx:1384`、`:1375`、`:1231`、`:858`、`:101/282`、`:111`；`web/src/surfaces/registry.ts:262-330`；`web/src/screens/model-picker.tsx:279/478` | **旧构建残留**，当前源码已清除；需 T5 用新构建重放确认 |
| A03 | 渲染/重建 | P2（性能） | 4 个动画帧各修改 **1 564 / 788 / 1 564 / 788** 个节点（合计 4 704 条 `AddedModifiedNodeInAnimationFrame`），累计 DOM 修改计数 180 838 → 185 255+；构成为 772 个裸 `DIV.nsh-v`、192 个 `DIV.nsh-v.nsh-3f6c62e7`、96 个 `SPAN.nsh-run`、**48 个 `IMG.nsh-fill.nsh-fit-cover`**；全窗口 **144 次 IMG 重建 + 168 次 `load` 派发**；`Paint` 440 次（4 簇 × 110） | `web/src/surfaces/image/index.tsx:619-650`（`renderGrid` / `renderAsset`）、`:284-292`（`tileWidth`）、`:628` | 待修（T4） |
| A04 | 调度/定时器 | P2（性能） | 声明 **0 ms** 的 4 个定时器实际晚 **210.6 / 277.4 / 429.1 / 501.4 ms**；声明 **4 ms** 的 5 个晚 **522.2 / 545.4 / 674.5 / 745.3 / 829.6 ms**；t0=4 836.5 触发的 2739（声明 4 ms）是窗口内最后一个 | A01 的下游症状，无独立代码点 | 随 A01 修复 |
| A05 | 内存/GC | P2（性能） | 堆峰谷 **177.2 MB**（t0=2 133.5）→ **278.0 MB**（t0=4 836.5）；**毛分配 160.2 MB / 2.85 s ≈ 56.2 MB/s**（相邻采样正增量求和），净增长 5.4 MB/s；**7 次 MinorGC 的 `args.type` 全部为 `allocation failure`**（16.3 / 5.3 / 24.6 / 22.6 / 10.4 / 20.0 / 11.1 ms）；1 次 MajorGC 54.7 ms（t0=2 076.4，`finalize incremental marking via stack guard`）；DOM 节点 90 211 → 91 393；监听器 47 937 → 36 838 | `web/src/surfaces/image/index.tsx:619-650`（每轮新建整棵图块子树） | 随 A03 修复 |
| A06 | 掉帧 | P2（性能） | 9 次 `STATE_DROPPED`：t0≈2 576.8 / 2 593.5 / 2 610.2 / 3 160.2 / 3 177.0 / 3 960.4 / 3 977.9 / 3 993.9 / 4 493.6 ms，逐簇对应 4 次重渲染；`affects_smoothness` 均为 `false`（未进入正式动画，只是空转掉帧）；另有 11 次 `STATE_NO_UPDATE_DESIRED`（空闲帧）、8 次 `STATE_PRESENTED_ALL` | 同 A03 | 随 A03 修复 |
| A07 | 第三方 | P2（非产品） | **5 个 Chrome 扩展**的 content script 共 29 个事件（`content-script-start.js` 6 次 `FunctionCall`，`content_main.js` / `content_guard.js` / `content.js` / 一批编号 chunk 各 1 次 `ScriptCatchup`），全部 0–1 ms 级；扩展 ID 不予记录 | 无（仓外） | 不修 |
| A08 | 取证卫生 | P2（流程） | trace 内嵌 **5 张全屏 JPEG 截图**（各 31 172 B base64），画面含侧栏会话标题、模型名与相对时间等用户内容；5 张 MD5 相同 | 无（DevTools 导出行为） | 流程约束 |

**结论（一句话）**：trace 中 **P0 泄露 = 0**（凭据形态全零命中，且本 trace 根本没录 network/console 面）；
真正的问题是「应用静止时主线程被框架运行时吃满 **91.8%**、画面 **2.22 fps**、4 轮整屏重建」，
叠加一个当时尚未拆除的「演示面板」产品形态（当前源码已由 `keel-demo-provider-v1` 清除）。

按严重度分布：**P0 = 0，P1 = 2（A01、A02），P2 = 6（A03–A08），合计 8 条。**

---

## 2. 逐条结论

### A01 — 静止画面把主线程吃到 91.8%，8 个长任务两个来源交替

8 个长任务严格交替来自两处，各 4 次：

| 序 | t0 (ms) | 时长 | TimerFire | 最内层 `FunctionCall` |
|---|---|---|---|---|
| 1 | 2 177.9 | 458.5 ms | 2729 | `nsh.7d929f1f29.js:273:15760` |
| 2 | 2 647.4 | 243.2 ms | 537 | `bootstrap.js:2:43973` |
| 3 | 2 891.9 | 304.6 ms | 2732 | `nsh.7d929f1f29.js:273:15760` |
| 4 | 3 206.1 | 266.8 ms | 538 | `bootstrap.js:2:43973` |
| 5 | 3 473.7 | **546.6 ms** | 2734 | `nsh.7d929f1f29.js:273:15760` |
| 6 | 4 030.5 | 242.2 ms | 539 | `bootstrap.js:2:43973` |
| 7 | 4 273.7 | 247.3 ms | 2737 | `nsh.7d929f1f29.js:273:15760` |
| 8 | 4 525.3 | 305.5 ms | 540 | `bootstrap.js:2:43973` |

两处归属经逐字核对（不是猜测）：

- `nsh.7d929f1f29.js:273:15760` 落在 `requestPump(){ this.pumpRequested||(…, setTimeout(()=>{…, this.call(e=>e.nsh_pump())}, 0)) }` —— **NSH 运行时的 WASM pump**，由 0/4 ms 定时器驱动，进入 `nsh_pump()` 后一次跑完整个工作队列。
- `bootstrap.js:2:43973` 落在 `schedule(e){ if(this.trace??=e, this.collectTimer) return; let t=Math.max(0,this.lastCollect+100-k()); … this.collect(e) }` —— **NSH 响应式依赖收集器**（100 ms 节流），对应 `collect()`；其自身采样在 CPU profile 中占 **22.3%**（`promote` 4.0%、`slot` 2.4%）。

CPU profile 自身时间按文件切分（7 106 采样）：`bootstrap.js` **40.7%**、`(native)` 31.2%、`nsh.e5915f5b7b.wasm` **24.0%**、
`nsh.7d929f1f29.js` 2.1%、`runtime.js` 1.4%、**`app.4927a018ca.js` 仅 0.6%**。

> 结论：耗时不在业务代码（应用层 0.6%），而在「框架每轮把整棵树重新收集 + WASM pump」。
> 产品侧的抓手是**别让每轮都需要重收集**（减少每帧的 token 读取与节点重建），而不是去优化业务函数。
> 旁证：8 帧的 `blocking_duration_ms` 为 408/193/254/216/496/192/197/255 ms —— **每一帧都在阻塞渲染**；
> 期间掉 9 帧、只上屏 8 帧（2.22 fps），5 张按 0.8–1.4 s 间隔拍的截图完全一样（画面静止）。
> 同窗口**无任何用户输入**（唯一 MouseDown 在 4 524.0，晚于全部 8 个长任务）→ 空转自激，排除「打字/点击打出长任务」。
> 注：t0=1 229.8 那个 947 ms（blocking 897 ms）是 `CpuProfiler::StartProfiling` 插桩开销，不计入产品性能。

### A02 — 「演示面板」以产品形态出现在界面上（旧构建）

截图与 DOM 两侧互证：标题条读作 `图像生成 · 专用面板 · 演示`；正文顶部常驻告警条（`演示` 徽标 +「图像 Provider 尚未配置，
当前为演示面板：生成结果来自本地演示资产，不会调用任何服务。」）；图块左上 `v1`、右上 `演示`；画廊内容是本地 PNG。

代码对表走 trace 构建的 sourcemap（`web/.nsh/out/bundle.js.map`，87 个 source，含 `sourcesContent`）：
`src/surfaces/image/index.tsx:1384`（告警条文案）、`:1375`（`Badge … label="演示"`）、`:1231`（`label="演示" accessibilityLabel="本地演示面板"`）、
`:858`（空态「当前为演示面板」）、`:101/282`（`DEMO_IMAGES` / `demoImage()`）、`:111`（`GENERATION_MS = 900` 本地模拟时长）；
`src/surfaces/registry.ts:262-330`（注释自陈「explicit 演示 section」+ `DEMO_PROVIDER_ID = "panel-demo"` + `DEMO_MODELS`）；
`src/screens/model-picker.tsx:279`（选择器「面板演示」分组）、`:478`（「退出演示」）。

> 结论：这正是 Owner 在 `keel-demo-provider-v1` 裁决里否掉的形态（「客户端根本没法上线」）。
> **当前工作区源码已无 `DEMO_*` / 「演示」符号**（`web/src/surfaces/**`、`model-picker.tsx`、`registry.ts` 全 0 命中；
> `web/assets/surfaces/**` 已不在工作树内，只剩 `web/assets/demo-shot.png`），也**不随 git 入库**（`web/.nsh/`、`web/dist/` 均 gitignored）。
> 但**构建输出目录仍有残留**：`web/.nsh/out/assets/assets/surfaces/image/{aurora,bloom,prism,tidal}.png`（4 个，共 ≈1.06 MB，mtime 11-13 00:16）
> 仍躺在 gitignored 的 `out/` 里；当前两代 app bundle（`app.f2d54b247f.js`、`app.dda8e4d1c7.js`）均 0 引用，属**陈旧产物**而非现行缺陷，
> 但会污染"本地静态目录是否还带演示资产"的判断，重建时应清空 `web/.nsh/out` 再确认。
> 故 A02 属**老构建残留观察**，不是现行缺陷；但它证明了该构建确实把演示面板当产品界面交付，T5 必须用新构建重放同一路径做对比。

### A03 — 每帧整屏重建（48 个 `<img>` 重新挂载并重新 load）

单轮渲染簇 = 一个动画帧内 1 564 个节点被标记修改，其中 48 个是 `IMG.nsh-fill.nsh-fit-cover`；
全窗口 **144 次 IMG 重建**，伴随 **168 次 `EventDispatch(type="load")`** 与 **168 次**运行时
`document.createElement('img'); … addEventListener('load', …)`（`nsh.7d929f1f29.js:273:23160`）。

强制布局/样式重算恰好 4 次（每次紧跟一个渲染簇，滞后 47–57 ms）：

| 序 | `UpdateLayoutTree` | `Layout` | 元素数 / 脏对象 | 布局根 |
|---|---|---|---|---|
| 1 | t0=2 636.4，1.27 ms | t0=2 637.7，0.34 ms | 119 / 120（总 168） | `DIV.nsh-v.nsh-12459f68`，盒宽 ≈1136 px |
| 2 | t0=3 196.5，1.65 ms | t0=3 198.2，0.31 ms | 同上 | 同上 |
| 3 | t0=4 020.4，1.35 ms | t0=4 021.8，0.24 ms | 同上 | 同上 |
| 4 | t0=4 521.0，1.02 ms | t0=4 522.0，0.25 ms | 同上 | 同上 |

`Paint` 440 次 = 4 簇 × 110（每簇覆盖 ~250 ms 窗口），即**每次重渲染都把整屏重画一遍**。

> 结论：图块不是「更新属性」，而是**整棵子树重建**——`<img>` 被销毁重建，于是每轮都走一次 `load`（即使图片已缓存）。
> 与 `web/src/surfaces/image/index.tsx:628`（`onLayout` 里 `gridWidths.update(...)`，**布局回调里写信号**）+ `:284-292`（`tileWidth` 读该测量值）
> 构成「测量 → 写信号 → 重算宽度 → 再测量」回路，是这类整屏重建最可疑的抓手。**这是最可疑假设，需 T4 用最小复现确认，不当作已证结论。**

### A04 — 定时器迟到 210–830 ms

| 定时器 | 声明延迟 | 安装 t0 | 触发 t0 | 迟到 |
|---|---|---|---|---|
| 2732 | 4 ms | 2 217.4 | 2 891.9 | 674.5 ms |
| 537 | 0 ms | 2 218.3 | 2 647.4 | 429.1 ms |
| 2734 | 4 ms | 2 928.3 | 3 473.7 | 545.4 ms |
| 538 | 0 ms | 2 928.7 | 3 206.1 | 277.4 ms |
| 2736 | 4 ms | 3 195.4 | 4 025.0 | **829.6 ms** |
| 2737 | 4 ms | 3 528.4 | 4 273.7 | 745.3 ms |
| 539 | 0 ms | 3 529.1 | 4 030.5 | 501.4 ms |
| 2739 | 4 ms | 4 314.3 | 4 836.5 | 522.2 ms |
| 540 | 0 ms | 4 314.7 | 4 525.3 | 210.6 ms |

> 结论：主线程被上一轮长任务堵住，pump 与收集器互相排队，形成「越跑越晚、一次跑更多」的正反馈。
> 这是 A01 的量化后果，不单独立项修复。

### A05 — 内存压力：7 次 MinorGC 全是 `allocation failure`

堆序列（`UpdateCounters` 912 次采样）呈锯齿：177.2 MB（t0=2 133.5 谷）→ 180.6 → 185.2 → 195.5 → 228.7 → 203.4 → 217.2 →
222.5 → 227.2 → 230.9 → 246.0 → **277.9** → 249.1 → 267.3 → 261.9 → **278.0**（t0=4 836.5）。
按「相邻采样正增量求和」计，**毛分配 160.2 MB / 2.85 s ≈ 56.2 MB/s**；扣掉 GC 回收后净增 5.4 MB/s。
逐簇对照（簇前 120 ms → 簇后 320 ms）：簇 1 185→229、簇 2 217→227、簇 3 246→278、簇 4 267→262（恰逢回收）——
**每轮重渲染都推高一次堆**，与 A03 同源。

7 次 MinorGC：16.3 / 5.3 / 24.6 / 22.6 / 10.4 / 20.0 / 11.1 ms（`args.type` **全部 `allocation failure`**）；
1 次 MajorGC 54.7 ms @ t0=2 076.4（紧贴第一轮 pump 结束，`finalize incremental marking via stack guard`）。
DOM 节点 90 211 → 91 393（+1 182，不涨说明节点被重建而非累积）；监听器 47 937 → 36 838（中途在 37 216 / 47 063 跳变）。

> 结论：GC 由「每轮新建整棵图块子树」直接触发，不是长寿命缓存膨胀；修 A03 即可消掉主要分配源。

### A06 — 掉帧与渲染簇一一对应

9 次 `STATE_DROPPED` 全部紧贴 4 个渲染簇的起始（簇起点 t0=2 589.5 / 3 174.0 / 3 968.4 / 4 504.2）：
t0 ≈ **2 576.8 / 2 593.5 / 2 610.2**（簇 1）、**3 160.2 / 3 177.0**（簇 2）、**3 960.4 / 3 977.9 / 3 993.9**（簇 3）、**4 493.6**（簇 4）。
`affects_smoothness=false` 说明这些帧不属于正式动画。窗口内另有 11 次 `STATE_NO_UPDATE_DESIRED`（浏览器判定无需更新）、8 次 `STATE_PRESENTED_ALL`。
注意 4 个 `frame_sequence`（8071352 / 8071387 / 8071435 / 8071467）同时上报 `DROPPED` 与 `PRESENTED_ALL` 两条 —— 这是 forked frame 的双路径上报；
按 frame_sequence 去重后，窗口内**实际上屏 8 帧 / 掉 9 帧**，即平均 0.45 s 才有一帧，且有 4 个序号完全相同。

> 结论：与 A03 同一因果链，只用于确认规模，不单独立项。

### A07 — 第三方扩展

29 个来自 5 个 Chrome 扩展 content script 的事件，其中 6 次 `FunctionCall` 集中在同一扩展的 `content-script-start.js`，
其余 23 次是一批编号 chunk 的 `ScriptCatchup`。全部 0–1 ms 级，与 4 簇节奏不对齐。

> 结论：与本项目长任务不同源，第三方噪声，不修。

### A08 — trace 自身携带用户画面

5 张 `Screenshot`（`disabled-by-default-devtools.screenshot`），base64 JPEG 各 31 172 B，**5 张 MD5 完全相同**（`699bf6d7305f38ae4bcaca643e2dd58e`）。

> 结论：① 5 张同图反证「3.6 s 内屏幕实质没变」（A01 的直接旁证）；② 这类 trace 外发/入库等于外发用户界面像素——
> 交付与入库只放「已脱敏的解析结论」，不附 trace 原件与截图（本条只写类型与位置，未抄录任何画面文字）。

---

## 3. 明确「未出现」的类别

| 类别 | 结论 | 核查方式 |
|---|---|---|
| console `error` / `warning` | **未出现**：22 个 category 中无 `console` / `blink.console` / `Log`；`ConsoleMessage` = 0、`LogEntry` = 0 | 全事件名 + category 枚举 |
| 网络（`ResourceSendRequest` / `ResourceReceiveResponse` / `ResourceFinish`） | **未出现**：各 0 条。该 trace 未录 loading/network 面，**无法据此判定任何 4xx/5xx、失败、取消或重复请求** | 事件名精确计数 + 全文检索 |
| 同一 URL 高频重复请求 | **无法判定**（无网络事件）。仅能从 `FunctionCall` 推出资源文件的引用热度：`bootstrap.js` 14 774 次、`nsh.wasm` 14 663、`runtime.js` 13 112、`app.js` 1 946、`nsh.js` 1 185 —— 这是**函数调用点**计数，不是请求次数 | CPU profile callFrame 聚合 |
| CSP / 安全拦截 | **未出现**：`CSP_violation` / `Security` / `Refused to` / `Content-Security-Policy` 全 0；`csp` 的 5 次字符命中全在 base64 截图内部（假阳性） | 大小写不敏感全文扫描 + 命中上下文核对 |
| 未捕获异常 | **未出现**：`Uncaught` 0；结构化字段 `"error"` / `"exception"` / `"failed"` / `"failure"` 0 命中；`fail` 的 7 次命中全是 GC 的 `type:"allocation failure"` | 结构化字段 + 正则扫描 |
| 凭据形态（`Authorization` / `Bearer` / `cookie` / `api_key` / `apikey` / `sk-` / `password` / `secret` / `x-api-key` / `token=`） | **未出现**：全部 0 次（大小写不敏感）。`token` 一词 29 次出现**全是 CPU profile 的函数名 `"functionName": "token"`**（`web/src/ui/kit/tokens.ts` 的取值函数），不是值；JWT 头前缀模式（三点式 base64 段，此处仅记模式名、非样本）10 次命中全在 base64 JPEG 数据内部 | 全文扫描 + 每处命中上下文核对 |
| 邮箱 / 手机号 / 主机绝对路径 | **未出现**：`@`、大陆手机号正则、Unix 家目录前缀模式（`/Users`、`/home`、`/root`，此处仅记模式名）、`file://` 全 0 | 正则全文扫描 |
| `localStorage` / `sessionStorage` 访问 | **未出现** | 全文扫描（源码侧也只有 `settings-shell.tsx:15` 的一句注释） |
| 滚动抖动 / 长列表 | **未出现**：`scroll_state` 全为 `SCROLL_NONE`；`HitTest` 仅 2 次；`MouseDown`/`MouseUp` 各 1 次 | PipelineReporter + InputLatency |
| 布局抖动（forced reflow 高频） | **未出现**（频次意义上）：`UpdateLayoutTree` 4、`Layout` 4、`ScheduleStyleRecalculation` 4、`InvalidateLayout` 28，全部与 4 簇一一对应，耗时均 1 ms 级——**真正的问题是「次数不少但每次都整屏」，不是「每秒几十次」** | 精确计数 |

> 因此 `specs/trace-hygiene/spec.md` 的 Scenario「trace 中不出现 `Authorization`、`Bearer`、`sk-` 或 cookie 原文」在本 trace 上**成立**。
> 但必须写明：**「没出现」不等于「路径不存在」**——本 trace 没有网络与 console 面，凭据泄露只能由 T2（代码侧审计）判定，
> 不能用这份 trace 结案。

---

## 4. T3 代码对表

### 4.1 构建身份（trace 与当前工作区不是同一版）

| 来源 | app bundle | 大小 | 与 trace 的差异 |
|---|---|---|---|
| trace（被观察版本） | `app.4927a018ca.js` | 428 529 B | 基线 |
| 当前 `web/.nsh/out/Keel/` | `app.f2d54b247f.js` | 416 574 B | 应用层重编译 |
| 当前 `web/dist/web/keel/Keel/` | `app.dda8e4d1c7.js` | 319 149 B | 另一代 dist |

三者 `nsh.7d929f1f29.js` / `nsh.e5915f5b7b.wasm` / `browser.30ff7d2f73/*` 哈希一致 → **差异全在应用层**，
所以「框架侧耗时」结论对新旧两版都成立，而「演示面板」只属于 trace 那一版。

### 4.2 事件位置 → 产品源码

| trace 中的位置 | 对应产品源码 | 归属 |
|---|---|---|
| `nsh.7d929f1f29.js:273:15760`（`requestPump`→`nsh_pump`） | 无仓内源码（NSH 运行时，构建产物） | 第三方框架；产品侧触点在 `web/src/surfaces/image/index.tsx` |
| `nsh.7d929f1f29.js:273:23160`（`createElement('img')` + `load`） | 同上 | 第三方框架 |
| `nsh.7d929f1f29.js:273:45513`（`watchBackdrops` / MutationObserver） | 同上 | 第三方框架 |
| `browser.30ff7d2f73/bootstrap.js:2:43973`（`schedule`→`collect`） | 同上（响应式运行时）；读取面来自 `web/src/ui/kit/tokens.ts:155-180` | 第三方框架 |
| `app.4927a018ca.js` 的演示文案/徽标 | `web/src/surfaces/image/index.tsx:858/1011/1231/1375/1384`、`web/src/surfaces/registry.ts:262-330`、`web/src/screens/model-picker.tsx:279/407/440/445/478` | **产品缺陷（已由 `keel-demo-provider-v1` 覆盖）** |
| `assets/assets/surfaces/image/{aurora,prism,tidal}.*.png` | `web/src/surfaces/image/index.tsx:45-48`（`import … from "../../../assets/surfaces/image/*.png"`） | 同上（演示资产，已移出树） |
| `browser.30ff7d2f73/{loader,realm,runtime}.js`、3 个 `blob:` 模块 | 框架自身 | 第三方框架 |
| 5 个 `chrome-extension://` content script | 无 | 第三方扩展 |

### 4.3 产品缺陷 / 开发构建特有 / 第三方

| 判定 | 对象 | 依据 |
|---|---|---|
| 产品缺陷 | A02 演示面板 | 出现在产品界面与产品文案；已被 `keel-demo-provider-v1` 裁决移除，当前源码 0 残留 |
| 产品缺陷（诱因在产品代码） | A03 → A01/A04/A05/A06 | 长任务在框架运行时，但「每帧重建 48 个图块 / 整棵子树」由 `image/index.tsx:619-650` 的渲染写法与 `:628` 布局期写信号驱动 |
| 开发构建特有 | 页面走 `local-gaubee-keel-app.fde2e.com`（本机隧道域）+ `web/.nsh/out`（gitignored）+ 带 hash 的开发产物 | 生产形态应为 `www.fde2e.com` 静态产物；本 trace 不代表生产包体 |
| 第三方 | `bootstrap.js`/`nsh.js`/`nsh.wasm` 的 collect/pump、5 个扩展 content script | 无可归属的仓内源文件 |

### 4.4 已排除的「看起来像」的候选

这些是本仓确实存在的周期性调度，但**在本 trace 里没有对应事件**（已按声明延迟逐一比对 `TimerInstall`），故不作为成因：

| 候选 | 位置 | 排除依据 |
|---|---|---|
| 会话面板 1 s 时钟 | `web/src/screens/conversation.tsx:225` | 无 1000 ms 定时器（实测声明延迟仅 0/4/190/240/742/751 ms） |
| 语音/实时 250 ms 心跳 | `web/src/surfaces/speech/index.tsx:431`、`web/src/surfaces/realtime/index.tsx:906` | 无 250 ms 定时器；两 surface 在本 trace 中未激活 |
| 实时网关 100 ms 轮询 / 电平表 | `web/host.js:811`、`web/host.js:1115` | 无 100 ms 定时器；无 `keel.realtime.*` 事件 |
| 会话列表 600 ms 刷新节流 | `web/src/data/store.ts:909` | 无 600 ms 定时器 |
| SSE 重连退避 | `web/src/data/http.ts:663` | 无网络事件面，也无 `Last-Event-ID` 痕迹 |
| fixture 引擎 20/320 ms 定时器 | `web/src/data/fixture.ts:953/1029/1153` | 无 20/320 ms 定时器；全文无 `fixture` 痕迹（未走 `?fixture=1`） |

---

## 5. 复现步骤（按事件序列推断，未做额外假设）

1. 用 trace 对应那一版开发构建打开图像生成会话面（标题条 `图像生成 · 专用面板 · 演示`，页面根路径 `/`）。
2. **不做任何操作**，静置 3–4 s：t0≈2 177.9 起出现 458–547 ms 长任务，间隔 ~450–540 ms 一轮，交替来自 WASM pump 与响应式收集器。
3. DevTools Performance 观察：主线程占空比 ~92%；`AnimationFrame.blocking_duration_ms` 每帧 190–500 ms；
   帧序列呈「presented → dropped×2~3 → no-update-desired」；定时截图 5 次，画面完全一致。
4. 第 3.3 s 左右点一次（唯一 MouseDown，t0=4 524.0）：输入到上屏 20.5 ms —— 说明点击仍被响应，但**画面本身没有变化**。
5. 若要看 A02：同构建进入图像面即可见「演示」徽标、「当前为演示面板…本地演示资产」告警条，画廊是本地 PNG 而非 Provider 返回。

**T5 重放判据**：同路径重放后
① `RunTask ≥200 ms` 应为 0（或 ≤1 且来源可解释）；
② 主线程占空比显著低于 90%（目标 <30%）；
③ 窗口内 `STATE_DROPPED` 应为 0，`STATE_PRESENTED_ALL` 应随实际变化而非每 450 ms 一轮；
④ 堆毛分配 <5 MB/s（本次 56.2 MB/s），且 MinorGC 不再全是 `allocation failure`；
⑤ 界面与 trace 中 0 个「演示」字样、0 个 `assets/surfaces/**` 演示资产。

---

## 6. 脱敏记录

- 落盘只写**类型与位置**，未写任何凭据、口令、会话 ID、用户输入文本、trace 内主机绝对路径。
- 凭据形态扫描：`authorization` / `bearer` / `cookie` / `api[_-]?key` / `sk-` / `password` / `passwd` / `secret` / `x-api-key` / `token=` **全部 0 命中**（大小写不敏感）。
- `token` 的 29 次出现**全是函数名**（`"functionName": "token"`），非值；JWT 头前缀形态的 10 次命中全在 base64 JPEG 数据内部（假阳性，未解码、未抄录）。
- `hex32` 1 511 次命中全是 Chrome 的 frame / layer / trace ID，非凭据。
- 5 个 Chrome 扩展 ID **已知但不写入本文件**（只记数量与脚本名）。
- 5 张界面截图只描述「存在 + MD5 相同」，未转录任何画面文字。
- 页面主机名 `local-gaubee-keel-app.fde2e.com` 属团队开发隧道域、非凭据，保留以便复现。

---

## 7. 交给 T4 的修复清单（本次不改代码）

| 序 | 目标 | 建议动作 | 验收证据 |
|---|---|---|---|
| 1 | A01/A03 | 图块列表在数据不变时**不重建**：稳定 key + 只更新 `src`/`version` 属性；把 `onLayout` 的 `gridWidths.update` 移出布局回调（或做等值短路，避免布局期写信号→再布局） | 每帧节点修改数 1 564 → 实际变化数；`load` 派发 168 → 0–3 |
| 2 | A01 | 与框架侧确认 `nsh_pump` 调度：一次 pump 不应跨帧累积；0/4 ms 定时器不应排队成 500 ms 批 | TimerInstall→TimerFire 迟到 <20 ms |
| 3 | A05 | 上述修完后复测堆与 GC 类型（不应再全是 `allocation failure`） | 毛分配 <5 MB/s；无 20 ms+ MinorGC |
| 4 | A02 | 回归确认 `keel-demo-provider-v1` 在生产构建中确无「演示」文案/资产；用新构建重放本路径 | 新构建 trace：0 个演示资产 URL、0 个「演示」徽标 |
| 5 | A08 | 报告与仓库只收解析结论，不收 trace 原件/截图 | 入库物无 base64 JPEG 段 |
