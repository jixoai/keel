# S4 端到端走查发现（2026-11-14，owner: MainAgent）

服务以 `KEEL_DEMO_PROVIDER=1` 起在 13760，本机同时配置真实 vendor 凭据（`KEEL_BASE_URL`/`KEEL_API_KEY`），
公网入口 `https://local-gaubee-keel-app.fde2e.com`。

## 已修复（每项都有回归测试）

| # | 缺陷 | 严重度 | 证据 | 修复 |
|---|---|---|---|---|
| 1 | 图像/语音面板**不把选中模型传给网关** → 选 `demo-image` 却走 vendor 真实计费出图 | 高（花钱 + 错误 provider） | 请求体无 `model` 字段 | `selectedImageModel()` / `selectedSpeechModel()` 从 `store.sessionSelection()` 解析并写入请求；image 8/8、speech 11/11 |
| 2 | `demo-decision` 在 `/v1/models` 里 `selectable:true` 但 `surfaceKind=generic` → 选中进占位页死胡同 | 中 | `GET /v1/models` | `internal/modelcfg/resolver.go` 加 `sessionSelectableFamily()`；decision `selectable:false` + issue，仍留在列表供设置页可见；modelcfg/servicehost/agentapi/httpapi 全绿 |
| 3 | TTS 面板头部写死「未配置」，卡片同屏显示「已配置」 | 中（界面说谎） | 同一屏两处相反文案 | 头部 badge 改读 `ttsState()`；speech 11/11 含一致性用例 |
| 4 | 选中 Demo STT 后仍停在 TTS 工作台 | 高（进错面板） | 芯片=Demo STT，面板=语音合成 | mode 同步改为 `effect()`（原为函数体一次性赋值）；speech 11/11 含切换用例 |
| 5 | **焦点环完全不渲染**（`boxShadow: none`），键盘用户不可见 | P0 可访问性 | 聚焦后 `computedStyle.boxShadow === "none"` | `focusRing()`/`invalidRing()` 从 `boxShadow` 改为 `outline`（2px, `alpha(foreground,0.65)`）；实测 `outline: solid 2px rgba(212,212,212,0.65)` |
| 6 | 图像画廊 `onLayout` 写回信号又决定自身子项宽度 → 布局反馈环 | P1 性能（trace A01） | trace：静止时主线程 91.8%、2.22fps | 0.5px 阈值，无变化不写 |

## 端到端结果（真实链路）

| 场景 | 结果 | 证据 |
|---|---|---|
| Demo LLM（浏览器） | 3 轮完成，22/26/25ms，账本 `responseModel=demo-llm, provider=demo` | 会话事件流 |
| Demo Image（浏览器） | `已完成 · 2 张`，两张 `<img src>` = `/v1/demo/assets/square.png?i=0\|1` | DOM 查询 |
| Demo TTS（浏览器） | 结果区 `demo-tts · 明天下午三点评审 · 播放` | DOM 查询 |
| Demo Realtime（浏览器） | `就绪 · 麦克风开启 · 持续收音 · 00:05 · 已连接`；文字通道一轮 → 用户句 + 助手 `已完成` | 截图 + DOM |
| Demo STT（浏览器） | `语音转写 · 麦克风 → 文字 · 待命 · 共 0 条 · 按下说话` | DOM |
| 390×844 | `scrollWidth === clientWidth === 390`，无横向溢出 | DOM |
| 焦点环 | `outline: solid 2px rgba(212,212,212,0.65)`（深色底 ≈5.5:1） | computedStyle |

## 未覆盖 / 已知限制（如实登记）

1. **真实麦克风音频上行**：浏览器无法自动授予麦克风，实时会话验证走的是同一 WS 会话的文字通道 + 服务端 demo provider；麦克风授权超时 → 降级文字（有可见状态，不伪造）。
2. **STT 走平台识别而非服务端 `/transcriptions`**：客户端 `transcribeAudio` 已就绪，待音频采集桥接后切换。
3. **会话模型选择不持久化**：`sessionSelection` 为内存态，刷新后回落租户默认（store 注释即如此设计）。这是可用性缺口，建议下个 change 落库。
4. **video 无 provider**：`/v1/models` 无 video 模型，`surfaceKind=video` 的占位面仍无法在 UI 到达（单测覆盖）。

## 追加（截图子代理独立走查 + 主线复核）

### 已修复
| # | 缺陷 | 修复 |
|---|---|---|
| 7 | **SSE 间歇 404 → 徽标「未连接」、队列不排空**（4 个会话中 3 次复现）。`stream()` 把非可重试的 404 当终态 `closed`，而 store 没有重订阅路径，整段会话失去实时更新，只能靠轮询兜底 | 404 属于幂等 GET 的瞬时失败（新建会话瞬间、代理先答），改为**有界重试 3 次**（`notFoundRetries`），成功即清零；回归测试 `src/data/stream-retry.test.tsx`（fixture 先 404 两次再 200，断言「到过 open 且从不 closed」） |
| 8 | `HttpEventSource.stream()` 直接调用全局 `fetch`，绕过 `KeelHttp` 注入的 transport（可测性缺陷：stub 永不生效） | 改为 `this.http.transport()`，并暴露 `KeelHttp.transport()` |
| 9 | `sidebar.tsx` 收起条（唯一回到展开态的入口）与 `composer-dock.tsx`「回到底部」缺焦点环（UI 代理因文件额度未做） | 两处各加 `focusStyle={focusRing()}` |

### 仍未处理（如实登记，非本次范围）
- 实时面板报「麦克风未授权」而语音转写页显示「录音中」，且实时面板重复渲染两枚同名徽标 + 同一句降级说明两遍（P2 文案/口径冲突）。
- `/v1/media/capabilities` 的 `realtime` 仍为 `false`（由 `/v1/realtime/status` 负责，语义分工正确，但 tasks.md 措辞需同步）。
- demo 图像资产 32×32 放大糊化、同批两张相同（演示资产本身的保真度，非缺陷）。
- 图像面板在服务端 demo 内容下仍写「真实生成 / Provider 输出」（客户端不该知道是 demo，但文案可更中性）。
- Tab 序列有一次落在无 `aria-label`/`outline: none` 的元素上（P3）。

## 麦克风不是能力缺口，是权限（Owner 2026-11-14 裁决）

> 「麦克风只是一个权限问题，你完全可以左脚踩右脚，比如用 tts 生成的声音当素材测试 stt。」

据此把「无麦克风」从**未覆盖项**变成**产品能力**：

1. **STT 工作台新增「文件」输入**（第三种与语音/文字并列的方式）：`Picker.pickFiles` 选一个音频
   → `POST /v1/media/audio/transcriptions` → 转写成为账本行（source=`file`，标注「音频文件」）。
   不需要麦克风授权，任何浏览器都能走；这也是 mic 采集桥接落地后要复用的同一条端点。
2. **TTS 的音频携带它「说」的句子**：合成时把文本写进 RIFF `LIST/INFO/ICMT` chunk（标准元数据位，
   不改变 PCM，任何 WAV 读取器都会忽略）。转写端点读回该 chunk 作为转写结果。
   这样 TTS → STT 是一次**真实往返**，而不是两个常量碰巧相等——音频自己带着答案走完链路。
3. **其它音频仍走固定兜底文本**：端点不会声称理解它没读过的内容，字节数仍然进转写（可复现）。

实测（真实服务，demo provider，无麦克风）：

```
POST /v1/media/audio/speech {"text":"明天下午三点在A会议室评审"}
→ 200, 25222 bytes, RIFF/WAVE 16k mono

POST /v1/media/audio/transcriptions (同一条 25222 字节)
→ {"text":"明天下午三点在A会议室评审（已接收 25222 字节音频）","model":"demo-stt"}
```

回归测试：`TestSpeechToTranscriptionRoundTrip`（含字节计数断言）、
`TestTranscriptionOfForeignAudioUsesTheFallback`（外部音频不得冒充）、
`speech.test.tsx` 的文件输入用例。
