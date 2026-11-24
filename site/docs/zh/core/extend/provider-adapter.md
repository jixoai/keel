# 写 provider 适配器

**provider 适配器**把 Keel 整形后的请求翻译成某个厂商的线格式，并把回答以单一事件类型流式返回。本页
说明这条接缝、当前的解析流程、新增 provider 实际要做什么，以及附件（图片与文档）如何出站承载。

## 1. 接缝

loop 对模型的一切需求就是一个函数类型（`internal/provider/provider.go:128`）：

```go
type StreamFn func(ctx context.Context, model string, llm LlmContext, cfg StreamConfig) (*AssistantMessageEventStream, error)
```

provider 实现一个接口（`internal/provider/provider_interface.go:65`）：

```go
type Provider interface {
    Name() string                                  // 标识，与 Model.Provider 一致
    Models() []Model                               // 本 provider 能服务的模型
    StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error)
}
```

两种失败模型刻意并存：`StreamCompletion` 只在最早期「无法建立流」时返回 `error`（配置错、无凭据）；
一切运行时失败都以**终止错误事件**的形式走返回的流。`StreamFnFromProvider` 在两种形状之间做直通代理
（`internal/provider/provider_interface.go:79`）。

## 2. 模型如何被解析

`llm` 服务从租户文档构建 `StreamFn`（`internal/servicehost/services.go:127`）：

```go
svc := &llmService{stream: agent.FakeStream(), providerName: "fake", model: "fake"}
if strings.TrimSpace(agentCfg.Model) != "" {
    resolved, _, err := provider.ResolveProvider(agentCfg.Model, agentCfg.BaseURL, agentCfg.Protocol, agentCfg.Provider, lookup)
    svc.stream = provider.StreamFnFromProvider(resolved)
    svc.providerName = resolved.Name()
    svc.model = /* provider 自己的模型 id（models[0].ID） */
}
```

写适配器前值得知道的事：

- **空模型意味着确定性 fake provider**。快速开始与 hermetic 测试之所以可行全靠它——没有凭据也能跑通
  整条路径。
- **`ResolveProvider` 的优先级**（`internal/provider/resolve.go:50`）：显式 `--provider` 名高于一切；
  否则显式 `--protocol` 高于模型 id 启发式；再否则走预设目录、前缀（`ollama/`、`nvidia/`）、模型名推断，
  最后落到参考性的 OpenAI 兼容网关。未知 protocol 是**错误**，绝不静默回退。
- **protocol 是被归一化的，不是自由文本**（`internal/provider/protocol.go:33`）：`openai` 与
  `openai/chat` 折叠为 Chat Completions，`openai/resp_api` 走 Responses，`anthropic` 走 Messages。
  启动横幅打印的是标签，因此显示的线格式与实际所说一致。
- **凭据只是引用**。`agent.apiKeyRef` 与 provider 的 `env`/`headers` 值只接受 `${NAME}`/`env:NAME`/
  `secret:NAME`；字面密钥（包括 `Bearer <token>`）被拒绝且永不入库（`docs/HOST.md:61`）。`llm` 服务
  通过 `LLMOptions.LookupEnv` / `LookupSecret` 解析引用；部署没有密钥存储时 `secret:` 引用
  fail-closed 并指名该密钥，而不是回退到环境变量（`internal/servicehost/host.go:109`）。

配置输入速查：

| 变量 | 含义 |
|---|---|
| `KEEL_MODEL` | 模型 id；为空 = fake provider |
| `KEEL_PROVIDER` | 注册表里的内置 provider 名（优先于 protocol） |
| `KEEL_PROTOCOL` | `openai`（`openai/chat`）、`openai/resp_api`、`anthropic` |
| `KEEL_BASE_URL` | 端点前缀（OpenAI 兼容与 Anthropic 都用它） |
| `KEEL_API_KEY` | 解析出的驱动的凭据 |
| `KEEL_MODELS_CONFIG` | 租户**模型文档**（providers/rules/selection），更新的路径 |

## 3. 配方 A —— 加一条注册表条目

多数「新 provider」需求其实是「同一个协议换个 base URL、key 变量与鉴权方式」。注册表就是一组 spec
（`internal/provider/registry.go:72`）：

```go
var providerRegistry = []ProviderSpec{
    {
        Name:           "example-vendor",
        EnvVars:        []string{"EXAMPLE_API_KEY"},     // 取第一个非空
        DefaultBaseURL: "https://api.example.com/v1",
        Protocol:       ProtocolOpenAI,                  // 或 ProtocolAnthropic / responses
        AuthScheme:     AuthBearer,                      // bearer | x-api-key | aws | azure | special
        ExtraHeaders:   map[string]string{"X-Tenant": "keel"}, // 可选
        BaseURLEnvVars: []string{"EXAMPLE_BASE_URL"},    // 可选，按优先级覆盖
    },
    // …现有条目…
}
```

然后：

1. 该 provider 可按名选中（`--provider example-vendor`）；想支持模型 id 推断就往预设目录补模型。
2. 保持注册表**有序且确定**——`--help` 会枚举它。
3. 跑 provider 测试；解析矩阵（显式 provider > 显式 protocol > 预设 > 前缀 > 推断 > 默认网关）在那里
   有覆盖。

## 4. 配方 B —— 实现一整套协议

如果厂商不说 OpenAI Chat/Responses 也不说 Anthropic Messages，就实现 `Provider`：

```go
type myProvider struct { apiKey, baseURL string; client *http.Client }

func (p *myProvider) Name() string    { return "example-vendor" }
func (p *myProvider) Models() []Model { return []Model{{ID: "example-large", Provider: p.Name()} } }

func (p *myProvider) StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error) {
    stream := provider.NewAssistantMessageEventStream(ctx)   // 或包内既有形状
    go func() {
        defer stream.Close()
        // 1. 从 req.Context（system prompt、消息、工具）整形请求
        // 2. 增量解析厂商的 SSE/JSON 帧
        // 3. 发出 assistant 消息事件（text/thinking/toolCall 增量，最后是消息本体）
        // 4. 任何运行时失败都变成终止错误事件，而不是 return error
    }()
    return stream, nil
}
```

运行时依赖的规则：

- **流式，不要缓冲**。loop 消费增量；一次返回整条消息依然正确，但会失去 token 级体验
  （partial 是旁路，见[事件与协议](/zh/core/events)）。
- **工具调用属于消息本体**，每个调用都要有 `id` 与 `name`——内核在提交时会拒绝无名或重复 id 的
  assistant 载荷（`internal/kernel/validate.go:168`）。
- **失败走流**。`stopReason: error` 变成 attempt failed；runner 按策略重试，且该终止消息不进模型上下文
  （[事件与协议](/zh/core/events) §5）。
- **不要把凭据放进日志或错误串**。审计路径只记 provider/method/target/耗时/状态
  （`docs/SERVICE-GRAPH.md:168`）；HTTP 壳会脱敏。

## 5. 整体替换 `llm` 服务

如果你要改的是解析逻辑本身（例如多区域路由，或需要请求级凭据的 provider），正确形状是**服务替换**而
非打补丁：`llm` 服务提供 `ctx.llm`，你通过 `loadout.Config.Provide` 提供自己的工厂，并升该描述符的
`Version`。这就是[写一个服务描述符](/zh/core/extend/service-descriptor)描述的装配接缝；loop 只看见一个
`StreamFn`。

## 6. 附件：`file_id` > `url` > `base64`

文件/图片协议已经实现，且顺序是固定的。两个形状要分清：

- **账本只存引用**：`ImageRefContent{blobId, mimeType, size…}`（非图片是 `MediaRefContent`）。
  base64 永不进事件表，因此被多轮引用的同一张图只有一条 blob 行、一份字节
  （`internal/agentcore/content.go:190`、`openspec/specs/provider-files/spec.md:5`）。
- **provider 线上携带的是传输形态**：`ImageContent`，`fileId`/`url`/`data` 恰好只填一个
  （`internal/agentcore/content.go:156`）。

解析发生在 worker 里、出站请求之前，按目标声明的能力选择：

1. **`file_id`** —— 上传路径给出的 provider 侧句柄，最便宜：字节只过一次线，句柄过期才再过一次。
   句柄按 `(sha256, provider, workspace)` 缓存，带 TTL 与懒刷新
   （`internal/provider/imagefiles.go:324`）。
2. **`url`** —— 签发的 URL（默认 TTL 10 分钟），让字节完全不进请求体
   （`internal/provider/imagefiles.go:163`）。
3. **`base64`** —— 通用形态，也是兜底：内联路径只分配一份编码后的缓冲，读取任何字节之前先按
   provider 的 `MaxInlineBytes` 设界（`internal/provider/imagefiles.go:299`）。

能力声明挂在**线协议**上，而不是模型名上：图片用 `ImageCapsForAPI`
（`internal/provider/imagecaps.go:82`），文档用 `FileCaps`（`internal/provider/media.go:51`），
两者的 `BestForm()` 都遵循同一顺序（`internal/provider/imagecaps.go:63`、
`internal/provider/media.go:80`）。没有声明能力的驱动按 base64-only 处理，而这是所有厂商都接受的形态。

降级是诚实的、绝不静默：上传/URL 失败会带一条 `Note` 继续降级；超过声明上限且没有更便宜形态可用的
图片以 `ErrImageTooLarge` fail-closed 并指名大小与上限——run 继续但缺这张图，而不是 OOM 或让整条请求
被上游拒绝（`internal/provider/imagefiles.go:42`）。

所以适配器作者要做的事很窄：为协议声明能力、编码三种形态。编码器之上的一切——能力选择、句柄缓存、
有界内联缓冲——都已经在 `internal/provider` 里。

## 7. 测试 provider 适配器

| 门 | 跑什么 |
|---|---|
| Hermetic 单测 | provider 包测试，用录制/线级流，不联网 |
| fake provider 路径 | 不设 `KEEL_MODEL`——无需凭据即可跑 loop、内核与 HTTP 面 |
| 真实模型 E2E | `docs/REAL-MODEL-E2E.md`（§1–§2 配置、§3 脚本用法、§4 断言与证据口径；§7 列已知边界） |
| 脱敏 | 失败路径是 E2E 的一部分：故意用错 key，必须看到 401，**且**日志与响应不泄露密钥 |
| 回归 | 若改动触及默认装配，R1 差分门适用（[测试与验收](/zh/core/extend/testing)） |
