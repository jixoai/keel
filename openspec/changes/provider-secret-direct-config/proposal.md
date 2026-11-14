# Proposal: provider-secret-direct-config

> Owner 指令（2026-11-14）：「这种系统尽量别完全只靠 env，env 是对的，适合云端部署的时候使用，
> 但直接配置也是要支持的。」

## 现状（2026-11-14 实测，非推断）

- 平台**已具备**两条凭据通道：
  1. **env 引用**：`access: {type:"api-key", env:"S2API_KEY"}` → 进程环境变量（云端部署形态）；
  2. **租户密钥库**：`secret:NAME` → `settings › 密钥` 保存的 AES-256-GCM 密文
     （`PUT /v1/settings/secrets/{name}`，值只写不回读）。
  设置页 Provider 对话框的「凭据引用」字段已明示三种写法（`${NAME}` / `env:NAME` / `secret:NAME`），
  内置目录之外的 provider 本来就是**直接在 UI 配置**的（不是 env-only）。
- 本部署的断点：`KEEL_SECRET_KEY`（密钥库主密钥）未配置 → `settings › 密钥` 显示不可用，
  `secret:` 引用 fail closed。**已修复**：主密钥写入 gitignored 的 `.env.local`
  （`serve-local.sh` 透传 `KEEL_*`），重启后 `/v1/settings/secrets` → `available: true`。
- **直接配置端到端已实证**：密钥库存值 → provider `access.ref = secret:DIRECT_FX_KEY` →
  真实会话 → 本地假上游校验 `Authorization: Bearer <库存值>` 命中（match:true），
  回复 `DIRECT-CONFIG-OK`，`run.finished status=done`（转录见 `evidence/direct-config-e2e.md`）。
- **仍存在的折叠差**：内置目录的 provider 声明的是 `env:NAME`；env 未设置时当前逻辑
  **直接回退进程级 legacy key**（`KEEL_API_KEY`，magpie 网关的钥匙）→ 发到别家网关 → 401/EOF。
  用户即使在「密钥」里存了同名密钥也不会被使用——「直接配置」对内置 provider 不生效。

## What Changes

- **解析优先级（新增同名回退）**：显式 `ref` → 文档声明的 `env` 且变量已设置 →
  **租户密钥库中的同名 secret** → 进程级 legacy key → fail closed。
  即：云端设 env 时行为不变；自托管用户在「密钥」里存 `S2API_KEY` 即直接生效，无需再改 provider 文档。
- 可用性投影（`executable`/`selectable`/issues）与 attempt 使用**同一判定函数**
  （`credentialIssue` 与 `credential` 共享逻辑，仓库既有约束）。
- 密钥库不可用（无 `KEEL_SECRET_KEY`）时：同名回退跳过、`secret:` 引用照旧 fail closed
  并给出可读原因；绝不把空值当凭据发送。
- 设置页文案补一句：内置 provider 优先用环境变量；未设置时自动使用「密钥」里同名密钥。

## Non-Goals

- 凭据内联进模型文档（永远拒绝；文档/事件/日志零凭据）。
- 密钥轮换/多版本（现有写入即覆盖已够用）。
- 云端 KMS 托管（KEEL_SECRET_KEY 是部署主密钥，业务凭据直接配置在租户密钥库）。
