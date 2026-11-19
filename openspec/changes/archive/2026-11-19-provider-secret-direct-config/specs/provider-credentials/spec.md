# Spec delta: provider-credentials（provider-secret-direct-config）

## ADDED Requirements

### Requirement: Provider 凭据来源（部署 env 与直接配置双轨）

一个 provider 的凭据 MUST 可以来自两条通道，且 MUST NOT 内联在模型文档、事件、日志或任何响应中：

1. **部署级环境变量**（云端）：文档声明 `access.env: NAME`，进程环境变量提供值；
2. **租户密钥库（直接配置）**：`settings › 密钥` 按名字保存，文档显式 `secret:NAME` 引用，
   或（内置目录 provider 的 `env:NAME` 声明在环境变量未设置时）按**同名**密钥自动解析。

解析优先级 MUST 为：显式 `ref`（`secret:` / `env:`/`${NAME}`）→ 文档声明的 `env` 且变量已设置 →
租户密钥库中的同名 secret → 进程级 legacy key → 失败。可用性投影（executable/selectable/issues）
与 run 的实际凭据解析 MUST 使用同一判定，不得一处说可用另一处失败。
凭据缺失时 MUST fail closed（不发空值），并给出可读 issue。

#### Scenario: 直接配置生效（自托管）
- **GIVEN** 内置 provider 声明 `access.env: S2API_KEY`，进程环境变量未设置，租户密钥库已保存 `S2API_KEY`
- **WHEN** 会话选择该 provider 的模型
- **THEN** 请求使用密钥库中的值；settings 中该 provider 显示为可用

#### Scenario: env 优先（云端）
- **GIVEN** `S2API_KEY` 环境变量已设置，且密钥库中也存在同名密钥
- **WHEN** 解析该 provider 的凭据
- **THEN** 使用环境变量的值（部署级配置优先），密钥库同名值不参与

#### Scenario: 双轨缺失 fail closed
- **GIVEN** 环境变量与密钥库都没有对应凭据
- **WHEN** 尝试使用该 provider
- **THEN** 请求被拒绝且不发送空凭据；settings 以 issue 说明原因（picker 不列出不可用模型）

#### Scenario: 密钥值不回读
- **WHEN** 读取 `GET /v1/settings/secrets` 或任何 provider 视图
- **THEN** 只出现密钥名字与时间，任何响应、日志、事件都不含密钥值
