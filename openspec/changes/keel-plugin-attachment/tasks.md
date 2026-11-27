# Tasks: keel-plugin-attachment

## 1. 契约与清单

- [x] 1.1 在 `docs/MODEL-INPUT-MODALITIES.md` 增补“插件附件面”一节：唯一生命周期、
  元数据投影、预览槽位与后端不读字节的边界；与 `docs/BLOB-LIFECYCLE.md` 交叉引用。
- [x] 1.2 扩展 `internal/harnessext/plugin_extension.go` 的贡献投影：读取并校验
  `part=attachment` 与 `mime[]`（仅当 slot=`keel.ui.message-part`；`mime` 模式限
  `type/subtype` 与 `type/*`，≤32 项、每项 ≤128 字节）；按 `(slot,form,entry)` 合并到
  plugin.yaml 已声明的同一条贡献，不得产生重复条目。
- [x] 1.3 在 `internal/pluginpkg` 的 extensibility 校验中拒绝非法 `mime`（通配位置、
  大小写、空项），错误带 `contributes[i].mime` 字段路径；补字段级单测。
- [x] 1.4 `GET /v1/plugins` 的 contribution 投影带 `part/mime`；安装审批文本展示
  “将认领 `keel.ui.message-part` 的附件贡献 + MIME”；补审批快照测试。
- [x] 1.5 冲突规则：同一插件内同一 MIME 模式不得重复；跨插件不做全局唯一（宿主按声明
  顺序选择），在 `EXTENSIBILITY.md` 写明“先声明先匹配”。

## 2. 输入期校验

- [x] 2.1 在共享提交路径（`internal/httpapi/input_commit.go` 的 `CommitClientInput`）
  解析并校验 `attachments`：数组、≤16 项、每项只允许 `fileId/name/mime/size/width/height`、
  name 1..256、mime ≤128，返回字段级错误；MCP Apps `ui/message` 与 retry 复用同一校验。
- [x] 2.2 `fileId` 归属与完成态：按当前 tenant 查询 blob，要求 `complete=true`；
  不存在/跨租户/未完成统一字段错误，不泄露存在性；补 PG 测试（真实租户 A/B）。
- [x] 2.3 MIME 一致性：以 blob 记录为权威，小写去参数后比较；`image/*` 伪造为其他类型
  或反向伪造均拒绝；补表驱动测试。
- [x] 2.4 大小/配额：单文件超 `MaxUpload` 返回 413；租户存储配额超限返回 429/413 并带
  上限说明；复用 `cmd/keel/edge_quota.go`，不新增配额源。
- [x] 2.5 缺 `fileId` 的既有语义保留：生成显式文本 notice；补回归测试证明整条输入不被
  拒绝、模型仍知道附件存在。

## 3. 宿主投影与预览槽位

- [x] 3.1 定义并实现 `AttachmentPart` 投影（fileId/name/mime/size/kind/尺寸/thumbStatus/
  thumbnailUrl/downloadUrl/delivery.native/reason）；由事件 payload + blob meta + attempt
  解析结果生成，禁止填入字节或 CAS 路径。
  > 后端：`internal/attachment.BuildPart` + `GET /v1/sessions/{id}/attachments/{fileId}/part?seq=`
  > （session.read；name/宽高取已提交 user.message，MIME/size/缩略图取 blob；未完成 409；
  > delivery 明确标记为 attempt 未存储）。3.2-3.6 前端仍归 UI 切片。
- [x] 3.2 在 `web/src/screens/messages.tsx` 的附件行接入 `PluginMessagePartSlot`：
  以 `partKind="attachment"` 与 MIME 选择贡献；保留默认文件名/大小/下载入口。
  > 前端：`PluginAttachmentPreview` 复用 `keel.ui.message-part`；registry
  > `attachmentSelection` 按 **inventory 插件顺序 + contributes 声明顺序** 取第一个 ready 且
  > MIME 匹配的贡献（重注册不移动顺序，补跨插件重同步测试）；无 `partKind` 的普通消息部件
  > 不再被当作附件渲染器。默认事实行始终保留。
- [x] 3.3 宿主默认附件行五态：缩略图 pending/ready/failed/none、无权限、加载失败；
  去掉重复附件行或空白占位。
  > 前端：`web/src/screens/attachment-row.tsx` 实现加载/成功/错误/无权限/空（无附件不渲染
  > 区域）；pending 用非破坏性刷新并有终态；failed/none 一律占位，绝不把原图当缩略图；
  > `file.read` 以 `/meta` 探测（web 与 TUI 同源），403 显示无权限并禁用下载；
  > `uniqueAttachments` 去掉重复行；TUI harness 覆盖重复/空两种渲染。
- [x] 3.4 预览失败回退：贡献抛错/加载失败/超时 → 默认行 + 可解释提示；单个失败不影响
  其他附件与整轮对话；补渲染单测。
  > 前端：`PluginAttachmentPreview` 捕获抛错、装载失败与 4s 超时，失败面为中文
  > “附件预览失败”+原因+重试；默认行不受影响。TUI harness 13 个场景
  > （`web/src/screens/attachment-row.tui.test.tsx`）。
- [x] 3.5 TUI：`chunk` 附件贡献按 TUI 视图渲染；`iframe` 显示“终端不可用（需要图形
  界面）”与打开入口；不出现空白或假成功。
  > 前端：`slots.tui.test.tsx` 真实 App 覆盖 chunk 渲染与 iframe 降级；harness 覆盖
  > iframe 的“在浏览器打开”入口；iframe 收到附件查询上下文（fileId/mime/size/…）。
- [x] 3.6 主题与布局：预览只用宿主令牌（solid-glass/shadcn），390/768/1024/1440 无横向
  溢出；补 UI 走查截图。
  > 真实浏览器 E2E 步骤 9：插件预览在场，四档视口均无横向溢出，附件行在视口内；截图见
  > `evidence/ui/screenshots/09..12-layout-*.png`。

## 4. 租户、权限、审计

- [x] 4.1 预览读取走 principal 鉴权：`/v1/files/{id}` 与 `/thumbnail` 只用 principal 的
  tenant，不信任客户端 `X-Tenant-ID`；匿名/伪造头 401，跨租户 404；补 handler 测试。
- [x] 4.2 安装/启用/停用/卸载附件预览插件写既有 plugin audit；detail 只含 pluginId、
  slot/form、MIME 列表摘要，不含文件内容。
- [x] 4.3 预览经桥调用插件工具时复用 `Manager.InvokePluginTool`：缺 `plugin.invoke`/
  `permissions.tools` 返回类型化拒绝并写 `plugin_audit`；补负控。
- [x] 4.4 日志脱敏：附件解析降级日志含 `fileId/size/mime/reason`，不含 name/正文/字节；
  补日志断言（捕获 slog）。

## 5. 测试与 E2E

- [x] 5.1 后端单测/PG：数量、大小、MIME 伪造、跨租户、未完成、缺 fileId、字段级错误。
- [x] 5.2 provider 派发回归：图片原生、PDF 原生/兜底、音频原生/兜底、视频兜底、
  resolver 缺失；断言文本兜底含 blobId 与 reason。
- [x] 5.3 插件生命周期：安装附件预览插件 → 渲染 → 停用 → 默认行恢复 → 卸载无残留；
  同一 generation 的槽位/订阅撤销。
  > 后端覆盖：安装/启用/停用/卸载的 claims 投影、审计与 generation 撤销；UI 覆盖预览
  > 撤销与默认行恢复；`bridge.test.ts` 证明停用/卸载/重声明会退订并清空监听器
  > （`keel.plugin.unsubscribe`）。
- [x] 5.4 真实浏览器 E2E（7 步起）：上传 → 发送带附件消息 → 原生/兜底 → 安装预览插件 →
  渲染 → 停用 → 回退；每步截图，`UNCOVERED=0`，生成 HTML 报告并挂公网。
  > `scripts/keel-attachment-e2e/` 真实 PG + 双租户服务 + Chrome：9 步 / 116 断言全绿，
  > 12 张截图，`evidence/ui/steps.json` + `report.html`，覆盖表 UNCOVERED=0（EXEMPT 只允许
  > openspec 证据与 E2E 运行器，其余未映射即 UNCOVERED 并失败）。
  > 公网挂载受本 worktree 禁止 push/merge 的约束，合并后按 `fde2e-ops` 发布。
- [x] 5.5 跨租户与无权限 E2E：租户 B 打开租户 A 的附件 URL/缩略图 404；无 `session.read`
  账号看到无权限态，不泄露字节。
  > 步骤 6-8：reader 无 `file.read` 显示无权限态且缩略图/下载 403；outsider 无
  > `session.read` 全部 403，UI 显示“无权限读取会话（缺少 session.read）”而不退回登录页；
  > 租户 B 读 A 的 file/thumbnail/part 均 404，伪造 `X-Tenant-ID` 仍 404。

## 6. 验收与归档

- [x] 6.1 `go test -race` 相关包（httpapi/harnessapi/harnessext/pluginpkg/agent/blob/
  provider）全绿；`nsh check` 与 `nsh test --target tui` 全绿。
  > 后端 `go test -race ./internal/attachment ./internal/httpapi` 全绿
  > （`evidence/go-test-attachment-httpapi-race.txt`；相关包总表见 `go-test-race.txt`）；
  > 前端 `nsh check` 0 错（`evidence/nsh-check.txt`）；`nsh test --target tui` 478/478
  > 全绿（`evidence/nsh-test-tui.txt`）；web 打包见 `evidence/web-build.txt`；
  > 反向变异证据（顺序/占位/退订各一条会变红）见 `evidence/mutation-attachment.txt`。
- [x] 6.2 `openspec validate keel-plugin-attachment --strict` 通过；证据目录含校验输出。
- [ ] 6.3 独立 reviewer（子代理）复核 P0/P1 清零；修复后复跑 6.1/6.4。
- [ ] 6.4 归档：`openspec archive keel-plugin-attachment`，更新 `MAINLINE.md` 与
  `openspec/specs/plugin-attachment/spec.md` 的 Purpose；不保留 draft 状态。
