# Tasks: provider-secret-direct-config

## 已完成（2026-11-14，部署侧与现状实证）

- [x] 启用租户密钥库：`KEEL_SECRET_KEY`（32-byte base64）写入 gitignored `.env.local`，
      `serve-local.sh` 透传；重启后 `GET /v1/settings/secrets` → `available: true`
- [x] 直接配置端到端实证（显式 `secret:` 引用）：本地假上游校验 Bearer 头命中，
      会话回复 `DIRECT-CONFIG-OK`，`run.finished=done`；测试 provider/密钥/会话/进程全部回收
      （转录：`evidence/direct-config-e2e.md`，UI：`evidence/01-settings-secrets.png`）
- [x] `serve-local.sh` 支持 `*_KEY` 变量透传（env 通道对内置 provider 可用）

## 待实现（等运行注入线收口后落码，避免同文件并发写）

- [ ] `internal/servicehost/models.go`：`credential()` 与 `credentialIssue()` 的 env 未设置分支
      先查租户密钥库同名 secret（`lookupSecret`），再回退 `legacyKey`；两函数共享同一顺序
- [ ] 测试（`internal/servicehost/models_test.go`）：
      - env 未设置 + 同名 secret 已存 → 解析用 secret 值且 settings 显示 executable
      - env 已设置 + 同名 secret 已存 → env 胜（云端行为不变）
      - 两者都无 → legacy key 行为照旧（向后兼容）
      - 密钥库不可用 + `secret:` 引用 → 仍 fail closed 并给可读 issue
- [ ] 真库 E2E：内置形状 provider（`env: S2API_KEY`）+ 密钥库存 `S2API_KEY` + 本地假上游
      → 会话成功、假上游断言 Bearer 命中；清理并按纪律回收
- [ ] 设置页 Provider 对话框 hint 补一句（内置 provider 的 env/same-name-secret 关系）
- [ ] 勾选本 tasks 并提交（随该批）
