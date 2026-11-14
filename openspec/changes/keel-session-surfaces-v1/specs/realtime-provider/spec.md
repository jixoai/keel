# realtime-provider Specification

## ADDED Requirements

### Requirement: realtime 是独立 Provider 类型

Provider 类型 MUST 增加 `realtime`，并有自己的 composer/timeline。descriptor MUST 声明
modalities（audio/text）、transport（`websocket`/`webrtc`；本轮固定内置 WebSocket）、
能力（VAD/打断/增量转写/工具调用/文本兜底）与音频格式。

realtime MUST NOT 是 LLM surface 上的按钮，MUST NOT 被实现为“点击录音→转写”的 STT 面板
（后者属 `stt` 的 speech surface）。它是持续双向音频会话：麦克风持续上行、模型音频持续下行
并边收边播，VAD/打断属于会话，转写只是伴随事件。

客户端 MUST 在 WS 前 GET `/v1/realtime/status`；`configured:false` 时 MUST 落配置态且
MUST NOT 建连；服务端未配置时 `GET /v1/realtime` MUST 返回 503。

#### Scenario: 选择 realtime
- **WHEN** 客户端选择已配置的 realtime 模型并开始
- **THEN** 会话 surface 为 realtime，按 descriptor 建立实时通道

#### Scenario: 预检未配置
- **WHEN** `GET /v1/realtime/status` 返回 `configured:false`
- **THEN** surface 显示「实时语音服务未配置」与配置入口，不建立 WebSocket；
  不出现伪造的通话、转写或 assistant 消息

### Requirement: 实时会话事件与持久化

客户端与 Provider 之间传输音频帧、transcript delta/final、tool call、interrupt、error。

账本目标契约：MUST 只持久化最终 user/assistant transcript、音频资产引用、tool call/result
与 interrupt 标记；音频 chunk、partial transcript MUST NOT 写成账本事件。

**本轮交付状态**：Web 面板只维护客户端投影（final transcript 行 + 音频播放状态），不写服务端
账本，刷新不恢复；服务端账本写入与回放是暂缓项（tasks (a)）。因此本轮 MUST NOT 声称转写已持久化。

#### Scenario: 一轮语音
- **WHEN** 用户说一句话并收到语音回复
- **THEN** 面板出现 user/assistant 两个 final transcript 行；partial 只存在 UI；
  本轮不写服务端账本（刷新不恢复，文案明确说明）

#### Scenario: 打断
- **WHEN** 用户开口或点打断
- **THEN** 客户端立即停止播放/生成；终态 assistant 行带 `interrupted:true`；
  不产生伪造的完整回复

### Requirement: 权限、安全与降级

realtime MUST 显式请求麦克风权限，录制中 MUST 有持续可见指示，停止 MUST 立即释放。
MUST NOT 后台常听；MUST NOT 把原始音频写入日志。

麦克风不可用/拒绝时 MUST 在同一 surface 内降级为文本输入，并保留会话。

#### Scenario: 无麦克风
- **WHEN** 设备没有麦克风或权限被拒
- **THEN** surface 显示原因与“文本输入”入口；会话不丢失

### Requirement: 重连与不重复

实时通道断线后 MUST 重连，并 MUST 明确说明该连接是否为新的 Provider 会话；已 final 的
本地 transcript MUST NOT 重复，partial MAY 丢弃或替换。

没有 Provider 会话恢复能力时 MUST NOT 声称“按 session/turn 游标续传”或“已续接”。
重连失败 MUST 显示错误与重试，不伪造 assistant 消息。

#### Scenario: 断线重连
- **WHEN** 网络闪断后恢复
- **THEN** 客户端重连并提示「新的 Provider 会话，断线前的转写不会重放」；
  本地已 final 的行不重复；播放状态恢复或明确停止；失败显示错误与重试
