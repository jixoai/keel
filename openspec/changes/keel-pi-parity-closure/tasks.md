# Tasks: keel-pi-parity-closure

- [x] 1.1 hooks：租户命令钩子构建 registry，接进 `agent.Runner`；`cmd/keel` 依赖图包含 `internal/hooks`。
- [x] 1.2 hooks：before_agent_start 改 system prompt、context 改消息、tool_call deny/modify、tool_result patch。
- [x] 1.3 hooks：session_before_compact 可取消；超时/失败策略不挂 run；审计日志无正文/密钥。
- [x] 1.4 hooks：产品级测试（deny 阻止工具、modify 生效、timeout 不挂、compact deny）。
- [x] 2.1 telemetry：run 下挂 turn/tool/compaction 子 span；`message_*` span event。
- [ ] 2.2 metrics：turns/tool/context 指标，固定基数。
- [ ] 3.1 OTLP adapter（默认关），无 endpoint 零连接。
- [ ] 4.1 真实 MCP filesystem server stdio E2E，evidence 入库。
- [ ] 5.1 `go test -race` 相关包、main CI、openspec validate --strict；归档。
