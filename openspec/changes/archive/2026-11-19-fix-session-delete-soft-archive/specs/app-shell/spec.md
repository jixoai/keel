# Spec delta: app-shell（fix-session-delete-soft-archive）

## MODIFIED Requirements

### Requirement: 会话列表

会话行 MUST 显示标题（未命名＝「新会话 · 相对时间」）、预览、时间与状态（等待确认/失败/运行中），
MUST NOT 出现裸 session id、MUST NOT 出现任何演示/演示数据入口；支持搜索、新建、删除。
删除 MUST 为软删除（归档）：删除后会话从列表消失，服务端 MUST 保留其事件账本与运行记录
（计量/审计真值不可抹）；删除对不存在或跨租户的会话 MUST 404；重复删除 MUST 幂等成功。

#### Scenario: 无演示入口
- **WHEN** 在任意页面全文检索「演示 / demo / fixture / 切到演示」
- **THEN** 产品 UI 无命中（fixture 仅 `?fixture=1` 开发态可用，且不渲染任何入口）

#### Scenario: 删除会话（软删除）
- **WHEN** 客户端对某会话发起 DELETE
- **THEN** 返回成功；`GET /v1/sessions` 不再包含该会话；该会话的事件仍可按 id 读取（账本保留）

#### Scenario: 跨租户删除
- **WHEN** 用不属于当前租户的会话 id 发起 DELETE
- **THEN** 404；该会话在它自己的租户列表不受影响

#### Scenario: 重复删除幂等
- **WHEN** 对同一会话连续 DELETE 两次
- **THEN** 两次都成功（第二次不报错、不改动任何账本行）
