# Tasks: keel-session-surfaces-v1

> 每个任务都必须有可执行验收；surface 面板可并行开发，但共享 registry/contract 先冻结。
> 状态截至 2026-11-13（第二轮规格同步）：S0/S2/S3/S5/S6 已交付；S1 保留原 LLM 实现未物理搬迁；
> S4 标准模式与跨 surface 门禁完成，captain 判定接缝已就绪、web 团队界面未实现；三项暂缓见文末。
> 实现说明：会话 `surfaceKind` 由客户端按模型/captain 的 descriptor 解析（`/v1/models` 已投影
> `providerType`/`surfaceKind`）；本 change 未新增 session 数据库列，选型随输入携带。

## S0 契约与 skeleton

- [x] 冻结 `SurfaceDescriptor` / `surfaceKind` / composer+timeline 契约
- [x] 冻结会话 `surface_kind` 推导：llm/realtime/image/video/tts/stt
- [x] 冻结跨 surface 换模型策略：同 surface 可换，跨 surface 必须新建会话
- [x] 冻结 realtime 事件映射：final transcript + audio asset ref；partial 不入账
- [x] web 建立 `src/surfaces/` skeleton 与 registry，ConversationScreen 只做 surface 装载
- [x] `video` / `generic` 不落文本聊天：新增 `surfaces/unsupported/` 占位面（label + reason +
  唯一「打开模型设置」，演示预览态额外「返回对话」）；`Conversation` 分派为穷尽式，
  `video`/`generic` 装载占位面
- [x] registry 新增 `resolveCaptainSurface`（队长模型决定主会话 surface，无队长回退 `llm`）与
  `previewSwitchReason`（演示 preview 不得顶替已开会话的其它 surface）
- **验收**：registry 单测 13/13（6 类 provider + unknown fallback + captain/preview 门，
  `web/src/surfaces/registry.test.ts`）；`nsh check` 通过（构建记录见 S5）

## S1 LLM surface 回归

- [~] 现有 `messages.tsx`/`composer.tsx` 收编为 `surfaces/llm/`
  - 实际保留在 `src/screens/`（未物理搬迁，避免在并行面板开发中搬动 2000 行回归基线）；
    `Conversation` 通过 registry 装载 LLM 分支；物理收编留待面板稳定后单独重构。
- [x] 保留 queue/guide、停止、审批、工具组、thinking、compaction、附件
- [~] ConversationScreen 不再直接 import 消息/输入组件
  - 顶层 `Conversation` 只做 surface 装载；LLM 分支内部仍 import 现成消息/输入组件（见上一条）。
- **验收**：`nsh test --target tui` 全绿（53/53）；现有 web fixture 演示不回归

## S2 Image surface

- [~] Composer：prompt/negative/参考图/参数（仅 schema 字段）/生成/取消
  - 参考图选择/复用/移除只在演示路径完整生效；真实生成请求当前只上报 prompt/negative/size/n，
    `/v1/media/images/generate` 没有参考图字段。缺：真实参考图上传/引用接线。
- [x] Timeline：画廊按 turn 分组、缩略图/大图、参数摘要、复用/编辑/重生成
  - 未实现「下载 / 复制资产引用 / 删除」：当前资产是演示本地图或 Provider URL，尚无资产引用
    与字节落盘语义；当前提供预览、复用为参考图、编辑、重新生成。
- [x] 多轮编辑：选图 + 指令 → 新资产 `parentAssetId`；版本链可回溯
  - 版本链是客户端投影；真实编辑仅接受父资产的 Provider http(s) URL，演示资产不能作真实编辑输入。
- [x] 状态：空/生成中/成功/失败/取消；失败保留输入
  - 配额/权限态由真实 Provider 接线后补齐；当前为空态配置 CTA + 演示失败开关；真实网关 503
    会标记配置态并保留输入。
- **验收**：fixture 下生成 3 张图、选第 2 张编辑生成新版本、失败重试不丢 prompt；
  `nsh check` 通过；390px 无横向溢出（4/4）

## S3 Realtime surface

- [x] Composer：麦克风授权、录音指示、静音、打断、文本兜底
  - 持续双向：开始后麦克风流持续上行、模型音频持续下行并边收边播，回合由服务端 VAD 结束，
    用户可随时打断；不做按住说话，也不把「录音→转写」当输入机制（后者是 STT/speech surface）。
- [x] Transcript timeline：user/assistant final transcript、音频占位、partial 状态、错误
  - 音频回放按钮显式禁用；无真实音频资产，不伪造播放。
- [~] 连接：transport 由 Provider descriptor 决定；连接中/重连/关闭状态可见
  - 已实现 `/v1/realtime` WebSocket 桥、重连与状态机；`web/host.js` 仍用内置 WebSocket provider，
    未按插件 descriptor 选择 WebRTC/WS（`webrtc` 未实现）。
- [~] 事件：final transcript 成行 + audio asset ref（目标入账）；partial 不写账本；打断标记 interrupted
  - 本构建只做客户端投影：final transcript 成为 UI 行、partial 不落行；不写服务端账本，
    刷新不恢复（暂缓项 (a)）。
- [x] 安全：停止即释放麦克风；无后台常听；无原始音频日志
- **验收**：fixture 模拟 2 轮语音、打断、断线重连；转写不重复；拒绝麦克风自动降级文本；
  `nsh test --target tui` 通过（realtime.test.tsx 12 条 + components.test.tsx 2 条）

## S4 AgentTeam / 标准模式集成

- [x] 标准模式模型选择器显示 provider type + 目标 surface；选 realtime/image 明确进入对应 surface
  - `/v1/models` 已投影 `providerType`/`surfaceKind`（`internal/agentapi/models_test.go:315`）；
    真实模型与「面板演示」条目共用同一 surface 判定。
- [x] 跨 surface 换模型弹「新建会话」确认，不能直接替换
  - 演示模型（demo-realtime/image/stt/tts）同样走该门禁；空会话可直接预览。
- [~] AgentTeam：captain 默认模型决定主 surface；成员卡显示各自 surface 标记
  - 已实现 `resolveCaptainSurface`（`web/src/surfaces/registry.ts`）并由 registry 单测 pin；
    缺 web 团队界面与 captain chip 接线，依赖 `keel-agent-team-v1` 的 Team/Agent HTTP API，
    即暂缓项 (c)。成员卡 surface 标记随团队界面一并实现。
- [ ] 成员结果进入 captain 时间线的规则固定，不与 captain transcript 混用
  - 未实现，依赖团队消息/任务卡数据结构与 captain 时间线接线（`keel-agent-team-v1`）。
- **验收**：标准模式选 realtime 进入语音；跨 surface 切换被阻止（已验）；
  captain 判定接缝单测通过（已验）；captain realtime 的团队界面（未验，未实现）

## S5 实时运行与反馈

- [x] `nsh package --target web` 构建
- [x] `keel serve` 13760 重启，local 域名可访问
- [x] 登录后能选 llm/realtime/image demo 模型并看到对应 surface
  - 另提供 speech（demo-stt/demo-tts）与 video/generic 占位面可走查。
- [x] 失败/降级状态明确标注 demo，不伪造真实模型结果
- **验收**：`https://local-gaubee-keel-app.fde2e.com` 登录后 llm/realtime/image 三套主 surface
  可见（另可切 speech 工作台与 video/generic 占位面）；构建日志与 serve 日志附在报告

## S6 验收报告

- [x] 覆盖 S1-S5 的 E2E 截图（桌面 + 390px）
- [x] 记录 realtime 打断/重连、image 多轮编辑、跨 surface 阻止的证据
  - realtime 打断/重连由 TUI 测试覆盖并写入报告；image 多轮编辑有桌面/390 截图；跨 surface 由 registry 单测与 picker 确认卡覆盖。
- [x] 记录未覆盖：真实 WebRTC Provider、真实厂商 realtime、音频编码兼容矩阵
- 报告：`fde2e/.agents/reports/keel/2026-11-13-session-surfaces/report.html`

## 暂缓项（2026-11-13 显式登记）

1. **realtime 转写账本持久化**（刷新后不恢复）
   - 现状：final transcript 与音频状态只存在于 `web/src/surfaces/realtime/index.tsx` 的模块级投影；
     `internal/httpapi/realtime.go` 只做 WebSocket 双向代理，不写 kernel 事件账本。
   - 前置条件：服务端把 final transcript 写为 `user.message`/`assistant.message` 并附 audio asset ref；
     提供 `session.resume` 回放或等价分页加载；音频资产存储/引用方案落地。
2. **真实厂商 realtime/TTS/STT 凭据**（当前 token plan 不提供）
   - 现状：`/v1/realtime` 需 `KEEL_REALTIME_WS_URL` + `KEEL_REALTIME_API_KEY`（或
     `KEEL_REALTIME_PROVIDER`）才 `configured:true`；未配置时 `/v1/realtime/status` 返回
     `configured:false`，客户端预检后落配置态、不开 WS。TTS 合成按钮无 Provider 时禁用并给配置 CTA；
     STT 走宿主 `Speech` 能力，不是厂商 STT Provider。
   - 前置条件：可用的 realtime/TTS/STT 厂商凭据与配额；`KEEL_REALTIME_*` 接线；TTS/STT Provider
     网关（`internal/mediagw` 当前只有 image/decision/chat）。
3. **AgentTeam captain 的 web 团队界面**（仅推导接缝已就绪）
   - 现状：`resolveCaptainSurface` + `surfaceFact` 已实现并由 `web/src/surfaces/registry.test.ts`
     pin；web 无团队界面，picker 只在标准模式工作；成员卡 surface 标记与「成员结果进入 captain
     时间线」规则未接线。
   - 前置条件：`keel-agent-team-v1` 的 Team/Agent HTTP API 与会话拓扑；web 团队视图；captain chip
     调用 `resolveCaptainSurface`；成员任务卡/团队消息数据结构。
