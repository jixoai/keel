# mcp

## ADDED Requirements

### Requirement: 传输与 PI 一致

MCP 客户端 SHALL 支持 stdio 与 Streamable HTTP；MUST 拒绝 legacy SSE（与 PI 一致）；
SHALL 提供 PipeTransport 与内存测试传输。连接 MUST 有超时、健康检查与重连退避。

#### Scenario: server 崩溃
- **WHEN** MCP server 进程退出
- **THEN** 当前调用返回错误，客户端按退避重连，run 不被拖死

### Requirement: 工具命名与内容适配

MCP 工具 SHALL 以 `mcp__<server>__<tool>` 暴露：非法字符替换、冲突加 hash 后缀、
64 字符上限（对齐 `createMcpToolName`）。输入 schema MUST 规范化为 object +
properties；输出 MUST 经 `toLlmContent` 转换并裁剪到 20KB（`limitMcpContent`）。

#### Scenario: 两个 server 同名工具
- **WHEN** 两个 server 都提供 `read`
- **THEN** 两个工具都可用且名称不冲突

### Requirement: 租户配置

MCP server 配置 SHALL 按租户存储（transport/command/url/env/allowed-tools/timeout）；
env 中的密钥 MUST 加密存储且日志脱敏。

#### Scenario: 凭据不落日志
- **WHEN** 配置了带 token 的远程 server
- **THEN** 日志与错误信息中不出现 token

### Requirement: 权限

破坏性工具（写/删除/执行）MUST 默认需要 hook 或审批；只读工具可按租户白名单自动放行。

#### Scenario: 写文件
- **WHEN** 模型调用 MCP 的写文件工具且无审批
- **THEN** 调用被拒绝并记录审计

### Requirement: 兼容

一个真实 MCP server（filesystem 或 git）SHALL 无需改 Keel 代码即可接入并列出工具。

#### Scenario: 标准 server
- **WHEN** 按 MCP 规范启动 filesystem server
- **THEN** `tools/list` 的结果出现在 Keel 工具注册表中
