# Tasks: keel-bot-identity-access

> 顺序：先冻结 Open Decisions，再动 schema/认证；权限与 Bot 模型必须同批落地，不能先上一半。
> Alpha 零兼容：删除旧入口，不写 shim/双读/迁移垫片。每个阶段结束更新本文件并提交证据。

## D0 设计冻结（阻塞全部实现）

- [x] D1：Bot 有效权限 = `Account 权限 ∩ Harness permissions`（已冻结，L11）
- [x] D2：TeamBot 不是 Bot、不做权限主体；它是 Mission + Roster（已冻结，L12）
- [ ] D3：ModelKind 只写类型，还是“类型 + 具体模型默认值”
- [ ] D4：重命名边界（产品面/表/API/CLI/UI vs 连 Go 包名一起改）
- [ ] D5：bootstrap / auth-off / 超管恢复流程
- [ ] D6：旧 sessions/agents/teams 数据清空还是接管
- [ ] D7：Bot 账号登录方式（密码 vs API key）与 API key 归属
- [ ] D8：TeamBot 跨 Account 组装/共享规则
- [ ] 冻结 `specs/identity-access/spec.md` 与权限 key 初始目录；偏离走新 change

## A1 Schema 与领域模型

- [ ] 新表：`accounts` / `roles` / `profiles` / `bots` / `team_bots` /
      `permission_keys`（可选，registry 可代码内）/ `permission_groups` /
      `permission_group_members` / `permission_group_grants` / `permission_grants` /
      `resource_relations` / `auth_sessions` / `auth_events`
- [ ] 旧表退役：`agents` / `agent_teams` / `api_keys`；不保留兼容视图
- [ ] `sessions` 增 `bot_id` / `account_id` / `created_by_account_id`；旧数据按 D6 处理
- [ ] RLS：所有新表 ENABLE/FORCE ROW LEVEL SECURITY + tenant policy；目录级测试
- [ ] 领域包：`Bot` / `TeamBot` / `Account` / `Role` / `Profile` 纯数据 + 校验 + 规范化
- [ ] 架构测试：kernel 不 import access/accounts/bots

## A2 认证

- [ ] Argon2id 密码哈希 + 参数常量 + 单元测试
- [ ] `POST /v1/auth/login|logout|session|password`；签名 cookie（HttpOnly/Secure/SameSite）
- [ ] CSRF 双提交；所有写请求校验
- [ ] 登录限速/锁定/审计（username + IP）
- [ ] `auth_sessions` 持久化；登出/改密/禁用即时吊销
- [ ] 删除 `KEEL_SERVE_PASSWORD` 单口令路径、旧 cookie、旧 Bearer 口令
- [ ] auth 模式 `required|local-only|off`；公网 + off 拒绝启动
- [ ] bootstrap/token/首次改密/超管不可删除/CLI 恢复（按 D5）
- [ ] 旧入口负控：旧 cookie 401、旧 API key 401、旧口令 401

## A3 权限引擎

- [ ] `internal/access`：key registry、scope、grant 并集、`grant ⊆ own`、超管短路
- [ ] ReBAC-lite：`resource_relations` 四关系 + 判定函数；禁止 handler 手写 owner 判断
- [ ] HTTP middleware：路由 → required key；资源关系解析
- [ ] 工具调用：manifest required key + Bot Harness permissions 收窄
- [ ] Bot spawn / session start：`bot.invoke` + 调用者 ∩ Bot 账号
- [ ] CLI 与 HTTP 同一套 key；UI `can()` 接 registry
- [ ] 初始 permission key 目录（粗粒度 + 高危细化）与内置权限组
- [ ] CI 覆盖率门：路由/工具/CLI/UI 动作清单快照，缺 key/未知 key 失败
- [ ] 审计：grant/revoke/deny/超管操作/关系变更；敏感值不落日志

## A4 Bot / TeamBot 产品面

- [ ] 删除产品词汇 `Agent`：API/CLI/UI/文档/表名统一为 Bot
- [ ] `TeamAgent` → `TeamBot`；成员 Bot 作为 captain Harness 工具（按 D2）
- [ ] Bot 级 Profile 绑定；权限实时判定；Role 变更 hook 注入提示
- [ ] Session 属于 Bot → Account；创建时写 owner 关系与 `created_by_account_id`
- [ ] TeamBot 组装/启动/成员/共享（按 D8）
- [ ] 旧 `/v1/agents` `/v1/teams` 404；旧 `keel agent/team` 删除
- [ ] 设置页：账号 / 权限组 / Bot / TeamBot；五态齐全，无权限隐藏 + 403

## A5 迁移与词汇对表

- [ ] 启动时检测旧 schema：拒绝启动并给出重建指令（不自动迁移）
- [ ] `scripts/serve-local.sh` 改 bootstrap/账号登录；`.env` 模板更新
- [ ] `README.md` / `docs/HARNESS.md` / `docs/ARCHITECTURE.md` / `site/docs` 术语对表
- [ ] `PRODUCT-UNIVERSE.md` / `BUSINESS-MODEL.md` 同步 Bot/TeamBot/Account/Role
- [ ] 旧 cookie/key 失效说明；升级即重新初始化账号
- [ ] 删除旧测试/旧 fixture；新增权限矩阵 fixture

## A6 验收

- [ ] 单元/负控：密码、cookie、CSRF、限速、并集、scope、`grant ⊆ own`、跨租户、
      低权限借 Bot、旧入口
- [ ] 集成：登录 → 建 Bot → Session → 工具 → 共享 → 撤销 → 下一工具 403
- [ ] 无人值守：触发器/Webhook 以 Bot 账号运行；`grant ⊆ own` 负控
- [ ] CI 覆盖率门全绿；路由/工具/CLI/UI 清单快照
- [ ] E2E 每步截图 + HTML 报告；人类调用 + 无人值守两条真链路
- [ ] 独立 A/B 复核，P0/P1 清零
- [ ] `openspec validate keel-bot-identity-access --strict`
- [ ] 证据目录 `evidence/`；任务板勾选；归档
