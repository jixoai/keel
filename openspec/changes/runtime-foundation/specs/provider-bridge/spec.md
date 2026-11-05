# provider-bridge

## ADDED Requirements

### Requirement: 统一流式契约

系统 SHALL 把各模型供应商（OpenAI Chat Completions、OpenAI Responses、Anthropic、
OpenRouter、Ollama）统一为同一事件流：`start → (text|thinking|toolCall)_start/_delta/_end* → done`。
构建流失败 MUST 返回错误；流开始后的失败 MUST 以终止事件（`stopReason=error/aborted`）结束。

#### Scenario: 供应商错误
- **WHEN** 流式进行中供应商返回错误
- **THEN** 客户端收到终止事件，流关闭，不 panic

### Requirement: 图片双形态

系统 SHALL 支持图片以 URL 与 base64/data URL 两种形态出站；当供应商不支持 URL 时，
MUST 在本仓内流式转换为 base64，MUST NOT 整图驻留内存。

#### Scenario: 不支持 URL 的供应商
- **WHEN** 消息引用 `file_id` 而目标供应商只接受 base64
- **THEN** 本仓流式拉取并编码，峰值内存与图片大小无关

### Requirement: 能力声明

每个模型 SHALL 声明上下文窗口、最大输出、是否支持 thinking/tools/images；
不支持的能力 MUST 显式报错，MUST NOT 静默丢弃。

#### Scenario: 不支持图片
- **WHEN** 向不支持图片的模型发送图片
- **THEN** 返回明确错误，提示模型不可见图片
