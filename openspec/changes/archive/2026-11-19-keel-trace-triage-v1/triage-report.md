# triage-report — keel-trace-triage-v1 收尾（性能类条目 + 重放对比）

日期：2026-11-14。对应 `tasks.md` 的三条未完成项：**T4 性能类**、**T5 重放路径的 trace 对比**、
**T5 报告**。证据目录：`evidence/`（本报告引用的全部脚本与产物）。

口径：本 change 的取证来源是一份 Chrome DevTools 性能导出（`~/Downloads/Trace-20261113T225129.json.gz`，
非入库物，见 A08）；「重放」指**等价复现同一路径**（新构建 + 服务端 demo provider 的图像面板），
用同一套解析器从 trace 里算同一组指标。原始 trace 只在 `findings.md` 以解析结论形式入档。

---

## 0. 结论摘要

1. **trace 列出的产品侧性能缺陷（A01/A03 及其下游 A04/A05/A06）在本仓已消除**，本轮用同一路径的等价重放复测：
   静态窗口主线程占空比 **67.7%（原 trace 全窗）→ 1.2%**、`RunTask ≥200ms` **8 个 → 0 个**、
   掉帧 **10 → 0**、GC **8 次/165ms → 0 次**、Paint **440 → 0**、节点重建 **4704 → 0**、图片 `load` 派发 **168 → 0**。
2. 修的部分：**产品侧触发点**——图像画廊的「测量 → 写信号 → 再布局」反馈环（`web/src/surfaces/image/index.tsx`
   的 `onLayout`/`gridWidths`，已在 `d68555d` 以 0.5px 阈值修掉），以及客户端内置演示面板（A02，由
   `keel-demo-provider-v1` 整体清除，本轮用新构建重放确认）。
3. **框架侧的 pump/collector 没有独立修复**（NSH 运行时，仓外）。它在重放里不再被自激驱动
   （pump 与响应式收集器的长任务为 0），因此本 change 不引入任何 NSH 侧改动。
4. 本轮**新发现**并已复现、定位、给出证据的一个跨应用行为：**读尺寸信号的 scope 在窗口 resize 时整棵重建 DOM**
   ——会话列表、header、composer、图像画廊的 `<img>` 全部换新节点（图片重读 4 次/两次 resize）。
   它与 trace 的 A03 是同一框架属性的不同触发路径（A03 是「自激循环」触发，这里是「用户 resize」触发）。
   该属性的修复点在框架 reconciliation（仓外 NSH）；产品侧的规避方案（让画廊宽度改由 flex 布局承担、
   去掉尺寸信号读）是一次布局改写，需要走本项目的 UI 改动审计（before/after 截图）流程 → **登记为独立 change 的建议**，
   本 change 不静默改产品代码（见 §3 的负结果实验）。

---

## 1. 性能类条目清单与状态（T4 逐条）

| 编号 | 严重度 | 条目（findings.md） | 现状 | 证据 / 归属 |
|---|---|---|---|---|
| **A01** | P1 | 静止画面主线程 91.8%、8 个 ≥200ms 长任务（交替来自 WASM pump 与响应式收集器） | **产品侧已消除**（重放复测 1.2% / 0 长任务）；框架侧无仓内改动 | `evidence/metrics-before.txt`、`evidence/metrics-after.txt`；根因在产品「每帧都要求重收集」：`web/src/surfaces/image/index.tsx` 的 `onLayout`→`gridWidths` 反馈环（已修，0.5px 阈值，commit `d68555d`）|
| **A02** | P1 | 界面全程「演示」形态（演示面板当产品交付） | **已修**（其他 change）+ 本轮新构建重放确认 | `keel-demo-provider-v1`：客户端 demo 分支/资产/徽标全清除；重放：面板文本 `演示=false`、`img src` 只有 `/v1/demo/assets/*`、构建产物 `演示` 0 次（`openspec/changes/keel-demo-provider-v1/acceptance.md` §1；本目录 `evidence/trace-replay.txt`） |
| **A03** | P2 | 4 轮整屏重建（4704 次节点修改、144 次 IMG 重建、168 次 `load`） | **自激路径已修**（重放：节点重建 0、IMG `load` 0、Paint 0）；**resize 路径仍是框架属性**（§3） | `evidence/metrics-*.txt`；代码：`image/index.tsx:649-657` 的等值短路（`d68555d`）+ 本 change 复核 |
| **A04** | P2 | 定时器迟到 210–830 ms（pump/collector 互相排队） | **随 A01 消除** | 重放静态窗口 `TimerFire=0`、`TimerBase::run=9`/8.6s 且总忙碌 107.8ms（无排队） |
| **A05** | P2 | 堆毛分配 56.2 MB/s、7 次 MinorGC 全 `allocation failure`、MajorGC 54.7ms | **随 A03 消除（GC 侧）**；毛分配率本轮未测（见 §5 限制） | 重放静态窗口 `GC=0`（Minor+Major 均 0）；原 trace 侧数据见 findings.md |
| **A06** | P2 | 9 次 `STATE_DROPPED` 与 4 个渲染簇一一对应 | **已消除** | 重放静态窗口 `dropped=0`、`presented=0`（2 个 `NO_UPDATE_DESIRED`）；对照窗（hover 控制）`presented=4`、`dropped=0` |
| **A07** | P2 | 5 个 Chrome 扩展 content script（仓外噪声） | **不修**（无仓内代码点） | findings.md §A07 |
| **A08** | P2 | trace 内嵌 5 张界面截图（流程卫生） | **流程约束**：报告只收解析结论 | 本目录入库物：解析后的指标 + **已去截图/去 v8-source-rundown/占位化临时 UUID** 的 trace（`evidence/trace-after.sanitized.json.gz`）；原 trace 与截图不入库 |

---

## 2. 重放路径的 trace 对比（T5 第一条）

### 2.1 方法与可复跑命令

- Before：`metrics.mjs` 直接解析原始 trace（`findings.md` 的同一份），指标重算，避免与 findings 口径漂移
  （findings 的占空比 91.8% 取自其 3.6 s 有效窗口；本报告 67.7% 取全窗并集——两者同向）。
- After（等价复现）：临时实例 `KEEL_DEMO_PROVIDER=1`（同构建、同 tenant、同 vendor 凭据，独立库见 §6）→
  `node trace-replay.mjs http://127.0.0.1:1379x <out> 6000`（浏览器走真实路径：登录 → 选 Demo Image →
  生成 2 张 → 静置 6 s 录像 → hover 正向对照）→ `node metrics.mjs <trace> mouse-split`。
- 脚本与产物：`evidence/{trace-replay.mjs,metrics.mjs,metrics-before.txt,metrics-after.txt,trace-replay.txt}`。

### 2.2 判据对表（findings §5「T5 重放判据」）

| # | 判据 | Before（原 trace） | After（等价重放·静态窗 8.6 s） | 判定 |
|---|---|---|---|---|
| ① | `RunTask ≥200ms` 应为 0（或 ≤1 可解释） | **8 个**（243–547 ms） | **0** | ✅ |
| ② | 主线程占空比 <30% | 67.7%（findings 记 91.8%） | **1.2%**（107.8 ms / 8639 ms） | ✅ |
| ③ | `STATE_DROPPED` 为 0 | **10**（presented 10） | **0**（presented 0；2 个 `NO_UPDATE_DESIRED`） | ✅ |
| ④ | 毛分配 <5 MB/s、MinorGC 不再全是 `allocation failure` | 56.2 MB/s；7×MinorGC 全 allocation failure | **GC 0 次**（Minor/Major 均 0）；毛分配率本轮未采到（见 §5） | ✅（GC 侧）/ ⚠️（分配率未测） |
| ⑤ | 界面与 trace 中 0 个「演示」字样、0 个 `assets/surfaces/**` 演示资产 | 旧构建：演示面板 | 文本 `演示=false`；`img src` 仅 `/v1/demo/assets/square.png?i=0\|1`（服务端资产）；构建产物 0 个 `演示`、无 surfaces 资产 | ✅ |

### 2.3 正向对照（证明 0 不是"类别没开"）

静态窗口的 0 Paint/0 Layout 是**真 0**：同一 trace 的控制窗口（鼠标 hover 触发重绘）里
`Paint=8`、`UpdateLayoutTree=4`、`Layout=2`、`presented=4`、`dropped=0`、占空比 12.1%——
渲染事件确实在被采集，只是静态时无事发生。截图：`evidence/gallery-after.png`（2 张 demo 资产、`已完成 · 2 张`）。

---

## 3. T4 范围内的修复尝试与负结果（如实登记，未改产品代码）

针对 A03 的「每帧整屏重建 / IMG 重建」在**产品侧**还能不能进一步收口，本轮做了三点实证：

1. **固定 key（`Each`）**：把画廊的 `assets.map` 换成 `<Each of={assets} key={asset.id}>`（骨架同理）→
   实测 resize 仍重建全部层级节点、`<img>` 仍被重建并重读 **4 次**（`evidence/img-identity-fixattempt.txt`）。
2. **组件化**：再把 tile 抽成组件 `<AssetTile>` → 同样 **4 次**重读（同上）。
3. **定位**：临时插桩计数（`evidence/diag-counters.txt`）显示 resize 使 `renderGrid` **×4**、`renderAsset` **×2**
   （两次 `paneWidthPx` 写入）；且**整个 shell 同现象**——`workspace-shell/sidebar/main/header` 保持节点身份，
   而读了尺寸信号的子树（会话列表 28 行、header 内容、conversation 面板、composer 输入、画廊）全部换新节点
   （`evidence/app-identity.txt`）。

结论：这是 **NSH 运行时 reconciliation 的通用属性**（重跑一个 scope = 重建该 scope 的 DOM 节点；key 与组件边界都不保留 DOM），
**不在本仓**（`web/.nsh/out` 是构建产物；运行时来自 nsh 0.4.0 nightly）。产品侧的可行规避是改布局（tile 宽度交给 flex、
去掉读尺寸信号的 prop 链），属 UI 布局改写，需走项目的 before/after 截图审计与设计评审——**建议另立 change**
（已在 §5 登记），本 change 不做。

因此 T4「性能类」按 trace 证据的**可修部分已修并复测**（T4 条目按此勾选），**框架级部分为仓外**（写明原因，不假勾）。

---

## 4. 未覆盖 / 残余（UNCOVERED）

1. **框架 reconciliation 属性**（§3）：修复点在 nsh 上游；本仓只能规避（另立 change）。
2. **堆毛分配率未复测**：重放 trace 未采到 `UpdateCounters` 序列（类别未生效），故 ④ 的 MB/s 数值缺「After」；
   替代证据 = `GC=0`（8.6 s 内零次 GC，弱于分配率但与 56.2 MB/s 的对照方向一致）。
3. **帧序去重**：本报告不按 `frame_sequence` 去重（findings 做过去重：8 presented / 9 dropped）；
   两侧同口径，比较不受影响。
4. **性能重放是等价复现**，非逐字节同一构建：After 用新构建（`app.37dc0837d1.js`）走真实图像面板；
   Before 的构建（`app.4927a018ca.js`）已随 gitignored 产物消失，无法原样重跑（findings §4.1 已记构建差异）。
5. **A07**（第三方扩展）**/ A08**（流程）不修，仅登记。
6. **无 in-repo 自动化回归守卫**可覆盖「resize 不重建 `<img>`」：`nsh test`（tui/web 两 target 都试过）
   不读取图片源、也不暴露节点身份（`screenshot()` 只有终端 cell 样式）；本轮守卫是浏览器探针脚本（`evidence/*.mjs`，
   依赖外置 playwright-core，故不入产品测试链）。

---

## 5. 建议的后续 change（不在本 change 范围）

- **gallery-flex-layout**：把图像画廊 tile 宽度从「测量信号」改为纯 flex 布局，并从 `ConversationColumn`/`ComposerDock`
  的 prop 链上摘掉尺寸信号（需 UI before/after 审计）——收益：resize 不再重建画廊 DOM、不再重读图片，
  且顺带删掉 `gridWidths`/`tileWidth` 一套反馈机制（"先做减法"）。
- **upstream nsh reconciliation**：向 NSH 上游反馈「scope 重跑重建 DOM」的实测（含本目录探针脚本），
  争取 key/组件级 DOM 复用。

---

## §6 进程与回收（起过必回收）

| 进程/资源 | 端口 | 用途 | 回收证据 |
|---|---|---|---|
| `/tmp/keel-app-demo serve`（多个 pid：31596/38666/…） | 13792 / 13793 / 13794 | 等价重放的临时 demo 实例（同 tenant/同构建/同凭据） | 收尾统一 `kill`，见文末实测输出 |
| headless Chrome（playwright 自起自停） | — | 探针/录像 | 脚本 `browser.close()` |
| PostgreSQL 库 `keel_verify_trace_v1` | — | 临时实例隔离队列（避免与 13760 常驻 worker 抢 run，理由同 demo change §2.0） | 收尾 `drop`（见文末） |
| 13760 常驻实例 | 13760 | 只读对照（未动） | 无重启、无 drain、无写 |
| `/tmp/keel-verify/**`（构建快照、探针、trace） | — | 本轮全部中间物（仓外） | 入库只收 `evidence/` 下的必要产物；其余留在 /tmp |

收尾实测（2026-11-14，记录见 `evidence/cleanup-final.txt`）：

```
$ for p in 13792 13793 13794; do kill $(lsof -nP -iTCP:$p -sTCP:LISTEN -t); done
  killed 42587 / 38666 / 42341
$ for p in 13792 13793 13794; do lsof -nP -iTCP:$p -sTCP:LISTEN | wc -l; done
  0 / 0 / 0
$ ps -p 38666,42341,42587                       # → 进程已不存在
$ go run ./tmp-verify-db drop keel_verify_trace_v1
drop keel_verify_trace_v1: ok
$ go run ./tmp-verify-db list | grep -c -E "verify_demo|verify_trace"
0
$ rm -rf tmp-verify-db                          # 一次性建库工具删除，仓内无残留
```

---

## 证据索引（`evidence/`）

| 文件 | 内容 |
|---|---|
| `metrics-before.txt` | 原 trace 用同一解析器重算的 Before 指标 |
| `metrics-after.txt` | 等价重放的 After 指标（静态窗 + hover 控制窗） |
| `trace-after.sanitized.json.gz` | 重放 trace（已去截图/v8 源码 rundown、临时 UUID 占位化） |
| `trace-replay.txt` | 重放执行日志（含 `演示=false`、`img src` 断言） |
| `gallery-after.png` | 重放画面（2 张 demo 资产、`已完成 · 2 张`） |
| `img-identity-current.txt` / `img-identity-fixattempt.txt` | resize 重建探针：现状 vs key+组件化尝试（均 4 次重读） |
| `app-identity.txt` | shell 与各子树在 resize 下的节点身份对照 |
| `diag-counters.txt` | 插桩计数：resize 使 `renderGrid` ×4 / `renderAsset` ×2 / `paneWidthPx` 写 ×1 |
| `*.mjs` | 全部探针/解析脚本（`trace-replay` / `metrics` / `img-identity(2)` / `app-identity` / `diag-counters`） |
