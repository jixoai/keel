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
- [ ] 性能类：按 trace 证据修
      → 未做。A01（静止画面主线程 91.8%、8 个长任务）与 A03（4 轮整屏重建 4,704 节点/轮）的
        根因在 NSH 运行时 pump 与响应式依赖收集器（`nsh.*.js` / `bootstrap.js`），本 change
        范围是 trace 取证与泄露收口；性能修复需单独 change 并按 T5 的同一路径重放验证。

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
- [ ] 重放路径的 trace 对比（修复前/后）
      → 未做：需要**新的 trace**。原 trace 是性能面板导出（无 Network/Console 面，见 T1），
        既无法验证 R2 的响应体脱敏，也无法验证 A02 的新构建画面；按 `leak-audit.md` R6 的
        取证口径（DevTools 勾选 Console + Network 后录制）重录一次，才能做修复前后对比。
- [ ] 报告：findings + 修复 + UNCOVERED
      → 未做：待上面那条新 trace 到位后与对比结果一并出（`findings.md` 与 `leak-audit.md`
        已是本报告的取证与审计两半，缺的是修复后对比这一半）
