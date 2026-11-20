# media-session-surface Specification

## ADDED Requirements

### Requirement: Image 会话围绕生成、编辑、多轮编辑

Image surface MUST 提供 composer（prompt/negative/参考图/参数/生成/取消）、按 turn 分组的
timeline（资产/prompt/参数摘要/操作）与多轮编辑：选资产 + 指令生成新资产，MUST 保留
`parentAssetId` 版本链（客户端投影）。

参数 MUST 只渲染 Provider/模型 schema 声明的字段；不支持字段 MUST 禁用并说明，MUST NOT
静默丢弃。本构建的操作范围与真实路径限制见场景。

#### Scenario: 多轮编辑
- **WHEN** 用户生成 3 张图，选中第 2 张并输入“把背景换成夜景”
- **THEN** 生成一个新资产，版本链指向第 2 张；画廊显示编辑历史

#### Scenario: 本构建的操作范围
- **WHEN** 用户查看画廊资产操作或使用参考图
- **THEN** 提供预览、作为参考图（演示路径）、编辑、重新生成；下载/复制资产引用/删除不出现；
  真实生成请求只上报 prompt/negative/size/n，参考图不随真实请求上行

#### Scenario: 真实编辑 URL-only
- **WHEN** 用户对真实资产发起编辑，父资产是 Provider 返回的 http(s) URL
- **THEN** `POST /v1/media/images/edit` 以该 URL 调用；`data:` URL MUST 被 400 拒绝并提示传
  Provider URL；网关 MUST NOT 主动抓取客户端提供的 URL（避免 SSRF）

#### Scenario: 生成失败
- **WHEN** Provider 返回错误
- **THEN** 保留 prompt 与参考图，显示错误与重试；不丢用户输入

### Requirement: 资产引用与 URL-only 编辑

本 change 的图片结果 MUST 以 Provider 返回的 URL 引用进入画廊；画廊 MUST NOT 内联 base64
到事件或设置。

本 change **不实现**资产字节写入 layerfs：字节落盘/上传为后续项，MUST NOT 声称资产已入
layerfs。媒体请求 MUST 走租户边界（`X-Tenant-ID`）；画廊 MUST NOT 跨会话展示资产引用。
资产加载失败 MUST 显示占位与重试。

#### Scenario: 资产加载失败
- **WHEN** 资产引用不可读
- **THEN** 画廊显示占位、错误原因与重试；不影响其它资产

#### Scenario: 拒绝 data URL 编辑
- **WHEN** 客户端把 `data:image/png;base64,...` 作为编辑参考图提交
- **THEN** 服务端 400 拒绝并说明“请传 Provider 的 http(s) URL（或先上传）”；
  不落任何资产，不发起上游请求

### Requirement: 空态与权限

空态 MUST 提供示例 prompt 与“配置 image Provider”入口。

未配置 image Provider 时 surface MUST 进入**明确标注的演示模式**：结果来自本地演示资产、
不调用任何服务、每张资产带「演示」标记；MUST NOT 把演示结果伪装成 Provider 输出。
真实网关配置后，生成失败 MUST 保留输入并显示错误/重试；网关 503 MUST 标记配置态而不是
静默失败。

#### Scenario: 无 image Provider
- **WHEN** 租户没有可用 image Provider
- **THEN** 显示空态与配置 CTA，并明确标注当前为演示面板；
  演示生成不发起网络请求，结果不会被描述为 Provider 输出

#### Scenario: 真实网关未配置
- **WHEN** 真实生成返回 503（网关未配置）
- **THEN** 本轮失败可重试并标记“需要配置”，prompt 与参数保留
