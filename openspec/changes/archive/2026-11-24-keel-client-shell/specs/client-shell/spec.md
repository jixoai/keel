## ADDED Requirements

### Requirement: Keel 深链路由

桌面客户端 MUST 声明 `keel` scheme，并解析：

- `keel://session/<uuid>`：打开该会话；
- `keel://session/<uuid>/decision/<id>`：打开该会话并提示审批位置；
- `keel://settings`：打开设置页；
- 其它或非法链接 MUST NOT 导航，并 MUST 给出可读提示。

链接来自任意应用，MUST 视为不可信输入；会话 id MUST 通过 UUID 校验后才传给 store。
单实例第二次启动 MUST 聚焦已有窗口，不创建第二个进程。

#### Scenario: 深链打开会话
- **WHEN** 系统把 `keel://session/<uuid>` 交给客户端
- **THEN** 已有窗口被聚焦并打开该会话

#### Scenario: 非法链接
- **WHEN** 系统交给客户端 `keel://session/not-a-uuid`
- **THEN** 不打开任何会话，界面给出可读提示
