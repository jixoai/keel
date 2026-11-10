# 写 provider 适配器

**provider 适配器**把 Keel 整形后的请求翻译成某个厂商的线格式，并把回答以单一事件类型流式返回。本页
说明这条接缝、当前的解析流程、新增 provider 实际要做什么，以及文件/图片协议里**没有实现**的部分。

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

## 6. **没有**实现的部分：文件/图片协议

provider 目前仍按 PI/pigo 的方式接收图片：**内联 base64**。内容类型只有 `data` 与 `mimeType`，没有
blob 引用（`internal/agentcore/content.go:65`）。仓库自己的调研与计划对这个缺口写得很明确
（`docs/FILE-PROTOCOL.md:46`）：

- 平台 blob 层已经存在（`POST /v1/files` → `fileId` + `url`、流式分块读取、SHA-256 去重——见
  [存储与运行](/zh/core/storage)）。
- **尚不存在**的是：按 provider 的能力位（`SupportsImageURL`、`SupportsFileID`）与
  `ResolveImage(blobID)` 的形态选择（`file_id` > `url` > `base64`）。在 `internal/` 里 grep 这两个标识符
  没有任何实现。
- 计划顺序是 Gemini Files API → OpenAI Responses `file_id` → Anthropic URL → Anthropic Files `file_id`
  → DashScope/OpenRouter URL（`docs/FILE-PROTOCOL.md:62`）。

也就是说：想让适配器避免重复上传图片字节，得先把这部分做出来；不要把它写成可用能力。

## 7. 测试 provider 适配器

| 门 | 跑什么 |
|---|---|
| Hermetic 单测 | provider 包测试，用录制/线级流，不联网 |
| fake provider 路径 | 不设 `KEEL_MODEL`——无需凭据即可跑 loop、内核与 HTTP 面 |
| 真实模型 E2E | `docs/REAL-MODEL-E2E.md`（§1–§2 配置、§3 脚本用法、§4 断言与证据口径；§7 列已知边界） |
| 脱敏 | 失败路径是 E2E 的一部分：故意用错 key，必须看到 401，**且**日志与响应不泄露密钥 |
| 回归 | 若改动触及默认装配，R1 差分门适用（[测试与验收](/zh/core/extend/testing)） |
