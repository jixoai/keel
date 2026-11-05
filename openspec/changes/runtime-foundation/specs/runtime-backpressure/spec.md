# runtime-backpressure

## ADDED Requirements

### Requirement: 网络是首要上限

系统 SHALL 以全局带宽预算约束出站流量（默认 1GbE 的 70–80%）。预算耗尽时，
存储读取 MUST 被自然阻塞（反压回到 PG/CAS），MUST NOT 继续无界拉取。

#### Scenario: 带宽饱和
- **WHEN** 1000 个图片流同时传输
- **THEN** 总出站不超过预算，RSS 稳定，无 OOM

### Requirement: 按尺寸分池

系统 SHALL 按请求尺寸分道：控制/SSE 与小型响应走快车道，大文件/图片走慢车道；
调度 MUST 保证小请求不被大请求长队列阻塞。

#### Scenario: 小请求插队
- **WHEN** 10 个大文件传输占满慢车道
- **THEN** 新的 SSE 控制请求在 100ms 内得到响应

### Requirement: 有界并发

系统 SHALL 使用有界 worker 池；队列满时 MUST 快速失败（429/503 + `Retry-After`），
MUST NOT 无界创建 goroutine。

#### Scenario: 过载
- **WHEN** 并发请求超过池容量
- **THEN** 拒绝部分请求并给出重试建议，RSS 不随队列增长

### Requirement: 慢客户端可恢复

系统 SHALL 检测慢客户端（写超时/进度超时）并取消其传输，记录已发送 offset；
客户端 MUST 能用 Range/offset 从断点续传。

#### Scenario: 慢客户端被取消
- **WHEN** 客户端 30s 内接收 <1KB
- **THEN** 传输被取消、资源释放；客户端重连后从 offset 继续

### Requirement: 重试预算

客户端与服务端 SHALL 遵守重试预算（次数、退避、抖动），避免重连风暴。

#### Scenario: 服务端抖动
- **WHEN** 服务端短暂不可用
- **THEN** 客户端指数退避 + 抖动重连，峰值连接数受控
