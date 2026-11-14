# Tasks: keel-trace-triage-v1

> 状态核对基线：2026-11-14，逐条按当前工作区代码取证。取证落点写在各条后面。

## T1 取证（trace → 异常清单）

- [x] 解析 trace：console error/warning 全量（含时间、来源、消息摘要）
      → **该 trace 未录 console 面**（无 console/blink.console/Log category；`ConsoleMessage`/`LogEntry` 均 0），已登记「未出现」并注明录制范围限制
- [x] 失败请求：非 2xx/3xx、被取消、被 CSP 拦截、重复请求
      → **该 trace 未录 loading/network 面**（`Resource*` 均 0 条），此类**无法判定**；CSP=0、取消=0 已核
- [x] 运行时异常：长任务（>200ms）、强制布局、内存/网络瀑布异常
- [x] 形成 `findings.md`（脱敏，带 trace 时间戳定位）
      → 8 条异常（P0=0 / P1=2 A01·A02 / P2=6 A03–A08），每条带 t0 时间戳、代码位置与状态

## T2 泄露审计

- [x] 扫描 Authorization / Bearer / cookie / api[_-]?key / sk- / token= / password
- [x] 判定泄露面：网络请求头、URL、控制台、DOM 文本、localStorage/sessionStorage
- [x] 每条命中：类型 + 位置 + 是否含原文（只写「含」不写值）
      → 命中数为 0；详见 `leak-audit.md`（含阴性证据与代码对表）

## T3 代码对表

- [x] 每条异常映射到具体文件/行
- [x] 区分「产品缺陷 / 开发构建特有 / 第三方」
      → 见 `findings.md` §4（含已排除候选与构建身份差异：trace=`app.4927a018ca.js`，当前另有 `app.f2d54b247f.js`/`app.dda8e4d1c7.js`）

## T4 修复

- [x] 泄露类：R1 凭据引用错误不再回显用户输入（modelprovider/validate.go）；R2 `writeErr` 统一经 `logsafe.Redact`；R3 登记为内部只读路径不改
      → R1：`internal/modelprovider/validate.go` 的 `validateCredentialRef` 不再 `%q` 回显，只写
        「the value is not a credential reference; use ${NAME}, env:NAME or secret:NAME — a literal
        credential is never stored」；
        R2：`internal/httpapi/server.go:1092` `writeJSON(w, status, {"error": logsafe.Redact(err.Error())})`；
        R3：`writeErr` 的泛化面已由 R2 的出口脱敏收口，无需再拆错误构造点。
        **同族残留（已核查，本轮不改）**：`internal/agentapi/api.go:260` 的 `writeErr` 仍是裸
        `err.Error()`，且该包**确实挂在 live 路由上**（`cmd/keel/serve.go:502` 构造，:324 经
        `httpapi.Config{Product: product.api}` 交给主 HTTP server，`api.go:189/196` 注册 settings
        与 plugin 路由）。逐条核查它承载的错误文本：`settings.go:608` 只回显 secret **名字**
        （`%q` 的是 name，值从不落错误）、`api.go:217/226` 只回显 tenant/session id 的解析
        失败串，其余错误来自 store/plugin 层（不含本服务凭据）。当前无实害，故不改；若将来该包
        新增「回显上游响应」类错误，必须同步套 `logsafe.Redact`。
        **口径修正**：`leak-audit.md` §2.3 第 3 条称 `ModelProviderWriteRequest` 无调用点，与
        现状一致；但该文档未登记 agentapi 已挂载——以本条为准。
- [x] 功能类：按复现步骤修
      → A02（界面全程「演示」形态）的根因是客户端内置 demo，已由 change
        `keel-demo-provider-v1` 整体清除：`web/src/surfaces/{image,realtime,speech}/**` 与
        `web/src/screens/{model-picker,conversation}.tsx` 的 demo 分支/徽标/种子全部删除，
        `web/assets/surfaces/**` 目录已不存在（`rg -ni "演示|demo"` 在这些文件里只剩解释性注释）。
        A02 的验收（新构建重放确认）归 T5 的 trace 对比。
- [x] 性能类：按 trace 证据修
      → 2026-11-14 收尾（详见 `triage-report.md`，证据 `evidence/`）：
        ① **产品侧可修部分已修并复测**——A01/A03 的触发点（图像画廊 `onLayout`→`gridWidths`
        布局反馈环）已在 `d68555d` 用 0.5px 等值短路修掉；本 change 用同一路径的**等价重放**
        （新构建 + 服务端 demo provider）复测：静态窗 8.6 s 内主线程占空比 **67.7%→1.2%**、
        `RunTask ≥200ms` **8→0**、掉帧 **10→0**、GC **8 次/165 ms→0**、Paint **440→0**、
        节点重建 **4704→0**、图片 `load` 派发 **168→0**（`evidence/metrics-{before,after}.txt`）；
        A02 亦由新构建重放确认（面板无「演示」、`img src` 只有 `/v1/demo/assets/*`）。
        ② **框架侧（NSH 运行时 pump/collector）不改**：属仓外构建产物，且重放中不再被自激驱动
        （长任务 0）；同步复核了 T5 的 5 条重放判据（第④条毛分配率本轮未采到，如实登记）。
        ③ **新发现（范围外，已登记不假勾）**：读尺寸信号的 scope 在窗口 resize 时整棵重建 DOM
        （会话列表/header/composer/画廊 `<img>` 全部换新节点、图片重读 4 次）——先试
        `Each` 稳定 key + 组件化均无改善（实测证据在案），定位为框架 reconciliation 属性；
        产品侧规避（画廊宽度改 flex、摘尺寸信号 prop 链）属 UI 布局改写，
        需走 before/after 审计流程 → `triage-report.md` §5 建议另立 change，本 change 不改产品代码

## T5 验收

- [x] 回归测试（单元/TUI/Go）覆盖每条修复
      → R1：`internal/modelprovider/provider_test.go:TestInvalidCredentialRefDoesNotEchoValue`
        （断言错误文本不含字面凭据）；
        R2：`internal/httpapi/error_redact_test.go:TestWriteErrRedactsCredentials`（Bearer /
        `"api_key":"…"` / `?access_token=` 三种形态都不出现在响应体）；
        A02 侧：`web/src/surfaces/{image,realtime,speech}/*.test.tsx` 与
        `web/src/screens/settings-capabilities.test.tsx` 保持通过；
        门禁实跑（2026-11-14）：`go test ./internal/... ./cmd/...` 全绿；
        `nsh check` exit 0；`nsh test --target tui` 84/84 通过
- [x] 重放路径的 trace 对比（修复前/后）
      → 2026-11-14 收尾：**新录一份等价重放的 trace 并做同口径对比**（`triage-report.md` §2，
        证据 `evidence/`）。方法：临时 `KEEL_DEMO_PROVIDER=1` 实例（同构建/tenant/凭据）+ 浏览器
        走真实图像面板路径（登录→选 Demo Image→生成 2 张→静置 6 s 录像→hover 正向对照），
        用同一解析器（`evidence/metrics.mjs`）重算原 trace 与重放 trace 的同组指标：
        判据① `RunTask ≥200ms` 8→**0**；② 占空比 67.7%→**1.2%**（<30% 目标）；③ `STATE_DROPPED`
        10→**0**（控制窗 presented 4/0）；④ 毛分配率本轮未采到（重放未含 `UpdateCounters`，如实登记），
        替代证据 GC **8 次/165 ms → 0 次**；⑤ 演示字样/资产：面板文本 `演示=false`、`img src` 仅
        `/v1/demo/assets/*`（A02 的新构建路径确认）。
        **口径说明**：原 trace 无 Network/Console 面（T1 已记），故 R2 的响应体脱敏仍由单测
        （`error_redact_test.go`）承担，重放不追这条——这是方法学边界，非未做。
        **未执行并写明原因**：`leak-audit.md` R6 建议的「DevTools 勾选 Console + Network 重录」未做——
        该 trace 的用途是性能对比，console/network 面按本项目口径由单测（`error_redact_test.go`）与
        `leak-audit.md` 的代码侧审计承担；如需 DevTools 口径的对照 trace，重录脚本已随 `evidence/` 入档
- [x] 报告：findings + 修复 + UNCOVERED
      → `triage-report.md`（本次产出）：A01–A08 逐条「已修/未修/证据/归属」表、Before/After 判据对表、
        本轮修复尝试与负结果（key/组件化对 resize 重建无效的诊断）、UNCOVERED 6 条
        （框架 reconciliation 属性、毛分配率未采、帧序未去重、等价复现非同一构建、A07/A08 不修、
        resize 无仓内自动化守卫）、后续 change 建议、进程/端口/库回收记录
