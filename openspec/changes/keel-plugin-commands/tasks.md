# Tasks: keel-plugin-commands

## 1. 契约与注册表

- [x] 1.1 在 `internal/pluginpkg` 增加 `CommandDecl` 与 `provides.commands` 解析/校验：
  name 文法、插件内唯一、`tool` 属于 `provides.tools`、danger 闭集、idempotent 缺省
  false、args_schema ≤16 KiB、显式 targets 非空；补字段级错误单测。
- [x] 1.2 增加命令发布名 `/<plugin-id>:<name>` 与内置裸名保留表（`compact`、
  `skill:<name>`）；安装/更新校验全局冲突；补 manifest fixture。
- [x] 1.3 在 `internal/harnessext` 建立命令注册表：随插件 enable/disable/uninstall/
  update 在同一 generation 增删；`PENDING`/`FAILED` 暴露 `available=false` 原因；
  不新增执行原语，只保存 `pluginId/tool/decl`。
- [x] 1.4 `GET /v1/sessions/{id}/commands`：`session.read` + 会话归属；聚合内置、插件、
  skills；固定排序、256 条上限与 `truncated`；补 handler 测试（跨租户/缺权/停用/失败）。
- [x] 1.5 安装/更新审批文本加入命令发布名、danger、targets；补审批快照测试。

## 2. 执行路由与内置命令

- [x] 2.1 `POST /v1/sessions/{id}/commands/{name}`：`session.write` + 会话归属；
  body `{args, requestId}`；参数按 `args_schema` 校验（400 字段级）；未知命令 404。
- [x] 2.2 幂等：`requestId` 唯一记录；同 requestId 重放返回首次结果（含失败），不二次
  执行、不二次开 decision；并发同 key 串行化；补并发/重放测试。
- [x] 2.3 idle 检查：有在途 turn/compaction 返回 409 `session_busy`；执行成功/失败都写
  durable `command.invoked`（命令名、来源、actor、tenant/session、requestId、状态、
  耗时、参数 hash+长度、decisionId），不含参数正文。
- [x] 2.4 `/compact`：委托既有 `CompactSession` seam；命令与 `POST /v1/sessions/{id}/compact`
  统一要求 `session.write` + `model.invoke`（本 change 同步收紧 HTTP 路由），
  busy/策略关闭/幂等语义一致；补对照测试。
- [x] 2.5 `/skill:<name>`：委托 skills loader；`session.write` + `skill.read`；缺失 404；
  为下一轮按引用加载；`disable-model-invocation` 不阻止用户命令；补测试。
- [x] 2.6 `/learn` 明确排除：不出现在目录；Composer 既有模板路径保留；补回归测试防止
  误注册。

## 3. 权限、审批、租户

- [x] 3.1 插件命令执行统一走 `Manager.InvokePluginTool`：`plugin.invoke` + 目标工具在
  registry 声明的权限 key + `permissions.tools`；目录 `requiredPermissions` 从 tool
  registry 派生，不使用插件自报。
- [x] 3.2 `danger=destructive` 开 durable decision：202 + decisionId；拒绝 403、超时 408、
  取消 409；未批准不得调用工具；补 decision 全分支测试。
- [x] 3.3 拒绝写审计：`permission.denied` / `command.invoked` refused；detail 不含参数
  正文/密钥；补负控。
- [x] 3.4 租户边界：跨租户 session 404；插件命令只来自当前 tenant 启用的插件；同名
  插件命令不跨租户泄露；补 PG 测试（真实 A/B 租户）。

## 4. 客户端（web/TUI）

- [x] 4.1 Composer：首 token `/` 打开命令菜单（数据来自目录）；选择后按 §7 规则映射
  `args.text` 或展示 schema 表单；执行走 `POST .../commands/{name}`；补纯函数单测。
- [x] 4.2 命令结果行：以 `command.invoked` 呈现，不伪装 user/assistant；`compact` 仍按
  `compaction` 事件呈现；补投影测试。
- [x] 4.3 命令面板 Mod+K 增加“命令”分组，与 Composer 同目录；无会话时提示先选会话；
  选择即执行；补面板测试。
- [x] 4.4 五态：加载/空/错误/无权限/成功；`targets` 不支持显示原因；TUI 与 web 同序、
  同可用性；补 `nsh test --target tui` 用例。
- [x] 4.5 视觉：菜单/表单/结果行使用宿主 kit 与主题令牌，390/768/1024/1440 无横向溢出；
  补截图。

## 5. 测试与 E2E

- [x] 5.1 后端单测：manifest 校验、目录排序/过滤/截断、参数 schema、幂等、busy、
  未知命令、权限矩阵、decision、插件注册/撤销。
- [x] 5.2 后端 PG 集成：真实租户跨边界、插件 enable/disable/FAILED、审计行内容、
  requestId 并发。
- [x] 5.3 内置命令：`/compact` 与 HTTP 端点等价；`/skill:<name>` 下一轮生效、缺失/缺权；
  `/learn` 不在目录且模板仍工作。
- [x] 5.4 真实浏览器 E2E（8 步起）：打开目录 → 执行内置 `/compact` → 安装插件命令 →
  执行成功 → 重放同 requestId → 拒绝 destructive → 停用插件 → 回退；每步截图，
  `UNCOVERED=0`，生成 HTML 报告并挂公网。
  - 真实栈 9 步 / 37 断言 / 19 图；改动文件覆盖表 56 个产品/测试文件 COVERED、
    UNCOVERED=0；EXEMPT 仅 openspec 证据与 `scripts/keel-commands-e2e/` 运行器。
  - 公网：https://local-kzf-keel-live-gh.fde2e.com/commands/report.html （HTTP 200）。
  - 证据：`evidence/ui-e2e/`（steps.json / report.html / shots / residue.txt）、
    `evidence/ui-go-race*.txt`、`evidence/ui-nsh-*`。
- [x] 5.5 TUI E2E：同一目录/顺序/可用性；命令菜单与结果行文本降级正确。

## 6. 验收与归档

- [x] 6.1 `go test -race` 相关包（pluginpkg/harnessext/harnessapi/access/toolsurface/
  web 构建）全绿；`nsh check`、`nsh test --target tui` 全绿。
- [x] 6.2 `openspec validate keel-plugin-commands --strict` 通过；证据目录含输出。
- [ ] 6.3 独立 reviewer（子代理）复核 P0/P1 清零；重点审“命令不是 prompt 模板”与权限派生。
- [ ] 6.4 归档：`openspec archive keel-plugin-commands`，更新 `MAINLINE.md` 与
  `openspec/specs/plugin-commands/spec.md` 的 Purpose。
