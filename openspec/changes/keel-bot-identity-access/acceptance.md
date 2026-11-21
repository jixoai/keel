# Acceptance: keel-bot-identity-access

> 判定标准：**上线级产品**，不是“接口通了/单测绿”。三轴独立复核、P0/P1 清零、E2E 每步截图 +
> HTML 报告、改动文件 before/after 覆盖 UNCOVERED=0，才可称“完成”。
> 判据正文：`.agents/skills/fde2e-delivery-gates/`；体验判据：`ui-experience-gate`。

## 1. 产品目标（验收的第一性问题）

1. 一个部署可以**没有账号**（本机开发）或有**多账号**；有账号时，每个人只看到自己该看的。
2. **权限是能力**：有 key 就能做；没有就 403；不存在隐藏的“人类专属/Bot 专属”旁路。
3. **Bot 是 AI 员工**：有自己的 Account、Role、ModelKind/Harness；Bot 能做的事与同权限人类一致。
4. **TeamBot 是团队使命**：只提供 Mission + Roster，不拥有权限；队长用工具组织成员。
5. **管家 Bot 场景成立**：一个高权限 Bot 能创建团队、配置权限并跑起来，不需要人类特判。
6. **可追溯**：谁（Account/Bot）、对什么资源、做了什么、结果如何，都能在审计/DAG 里查到。

## 2. 功能验收（逐条可执行）

### 2.1 启动与登录

- [ ] 空库启动 → 无超管时 `/v1` 返回 503/401 并提示 bootstrap；不存在“第一个访问者自动成为超管”。
- [ ] `keel admin bootstrap` 生成一次性 setup token；token 只能使用一次；过期/复用返回 401。
- [ ] 用 token 设置用户名/密码 → 首次登录强制改密 → 进入产品。
- [ ] 用户名不存在与密码错误返回同形 401；连续失败触发限速/锁定；`auth_events` 有记录。
- [ ] 登录 cookie 为 HttpOnly/Secure/SameSite；登出、改密、禁用账号后旧会话立即 401。
- [ ] `auth=local-only` 仅 loopback；绑定 `0.0.0.0` 启动失败；`auth=off` 需要显式不安全开关且公网拒绝。
- [ ] 旧 `KEEL_SERVE_PASSWORD`、旧 cookie、旧 `api_keys.role` 凭证全部 401。

### 2.2 账号、权限组与授权

- [ ] 超管创建 human 账号；账号可属于多个权限组；有效权限是并集；同 key 取最大 scope。
- [ ] 权限 key 只能来自 registry；未知 key 写入失败；CI 覆盖率门对未登记路由/工具/CLI/UI 动作失败。
- [ ] `grant ⊆ own`：A 只有 `session.read/own` 时，给 B 授 `secret.read` 或 `session.read/tenant` 返回 403，grant 不落库。
- [ ] 超管拥有全部 key/scope；跨租户操作、授权、改密、impersonation 全部审计。
- [ ] 最后一个超管不可删除/禁用；本机 CLI 可重置超管密码；公网无重置端点。
- [ ] 禁用账号后：登录失败、现有会话失效、其 Bot 的 API key 失效（或按明确策略拒绝）。

### 2.3 Bot

- [ ] 创建 Bot：Profile（Account+Role）、ModelKind、默认 provider/model、Harness（tools + instruction + permissions）。
- [ ] ModelKind 决定工作方式；默认模型可被调用参数覆盖；Session/Run 快照记录实际模型。
- [ ] Bot 有效权限 = Account 权限 ∩ Harness permissions；Role/Instruction 不产生授权。
- [ ] Bot 列表/详情显示：归属 Account、Profile、模型、工具、权限、状态、最近活动；有加载/空/错/权限不足/成功五态。
- [ ] Bot 归档后：新 Session 拒绝；已有 Session 保留可读；引用它的 TeamBot 标记 invalid 或明确降级。
- [ ] Bot API key：签发时只显示一次；只能以绑定 Bot 身份行动；错误 Bot 403；吊销后 401；last_used_at 更新。
- [ ] 人类调用 Bot 需要 `bot.invoke`；有权限时 Bot 以自己的权限执行；没有则 403；调用审计含 caller/callee/result。

### 2.4 TeamBot

- [ ] 创建 TeamBot：mission 非空；roster 至少一个 captain；成员引用唯一；revision/content_hash 稳定。
- [ ] TeamBot **没有** Account/Profile/权限；不能给它授权；权限判定永远落在成员 Bot 上。
- [ ] roster 可引用同/跨 Account 的 Bot；能否放入/调用由 `bot.invoke` 决定；无专属跨账号规则。
- [ ] 启动 TeamBot：队长 Bot 创建/调用成员；成员各自以自己的 Account/Harness 权限执行；TeamBot 不获得成员权限。
- [ ] TeamBot 运行 DAG 可查看：Mission、Bot/Session 节点、create/fork/invoke 边、状态、成本/用量、失败原因。
- [ ] 成员 Bot 被调用时是独立 Session（归属成员 Bot），父 Session 只收结果摘要；链路可追溯。
- [ ] 跨账号成员被撤销 `bot.invoke` 后，下一次调用 403；正在跑的成员按其自身权限/生命周期收敛。

### 2.5 Session 归属与共享

- [ ] 新 Session 必属于一个 Bot → Account；`created_by_account_id` 记录发起人。
- [ ] 默认只有 Bot 的 Account 可见；同租户其他账号无关系时 403。
- [ ] owner 可给 viewer/editor 关系；撤销后下一次读取/写入 403。
- [ ] 团队 Mission 注入成员上下文；成员 Session 记录 team_bot_id/mission 快照。
- [ ] 删除 Bot 不删除历史 Session；Bot 归档后 UI 明确显示“已归档，只读”。

### 2.6 管家 Bot（关键验收场景）

- [ ] 预置一个管家 Bot，持有 `bot.create`、`team_bot.manage`、`permission.manage`、`account.read`。
- [ ] 用户对它说“帮我组建客服团队并跑起来”；它创建成员 Bot、TeamBot（Mission+Roster）、权限组并启动。
- [ ] 全程无 Bot 专属代码路径；每一步都受 `grant ⊆ own` 约束；越权步骤 403 并解释缺什么权限。
- [ ] 运行后在 DAG/审计中能看到：谁创建了哪个 Bot、哪个 TeamBot、授了哪些权限、成员跑了什么、花了多少。

### 2.7 零兼容

- [ ] `/v1/agents`、`/v1/teams`、旧 CLI `keel agent/team` 返回 404/未知命令，无重定向/别名。
- [ ] 旧 `agents`/`agent_teams`/旧 `api_keys` schema 启动即拒绝，输出重建指令；不写旧格式读取/迁移代码。
- [ ] 旧数据库清空重建；不保留兼容视图、双读、迁移垫片。
- [ ] 文档/UI/错误文案不再出现面向用户的 `Agent`/`TeamAgent`（PI 技术包例外仅在架构文档说明）。

## 3. UI/UX 验收（产品级）

- [ ] 信息架构：登录/引导 → 账号 → 权限组 → Bot → TeamBot → TeamRun/DAG → 审计；每页有目的陈述与主操作。
- [ ] 五态齐全：加载（骨架/可取消）、空（解释+下一步）、错（人话+重试+错误码）、权限不足（缺什么+如何获取）、成功（主次信息+下一步）。
- [ ] 渐进披露：高频操作在首屏；低频进 Menu/Dialog/Sheet；不把裸 JSON/裸表丢给用户。
- [ ] 权限感知：无权限的导航/按钮隐藏或禁用并解释；直接访问 403 页面；不是白屏。
- [ ] 四档 viewport：375/390/768/1440；移动端可滚动可达，关键操作不被遮挡；四档截图。
- [ ] 设计语言：shadcn 官方组件 + solid-glass + 主题令牌 + lucide 单色；禁手写仿制；无未定义令牌。
- [ ] 文案：人话、单一语言；不出现 fixture/test/内部 id/长哈希。
- [ ] 对比度：正文 ≥4.5:1；图标/边框 ≥3:1；焦点环可见且全局单一定义。
- [ ] 触控目标 ≥44px；单行标签不折行；容器按内容收缩。
- [ ] 实时性：账号/权限/Bot 状态变化在 2s 内反映（SSE/事件），不要求手动刷新。
- [ ] OpenDesign 产出：设计方向 + 信息架构 + 关键页面原型 + 五态矩阵；评审选定后才实现。

## 4. 三轴对抗审查（独立子代理，实现方不得兼任）

### A 安全红队（P0/P1）

- [ ] 跨租户读取账号/Bot/TeamBot/Session/权限/审计 → 全部 403/404。
- [ ] 低权限账号调用高权限 Bot：无 `bot.invoke` → 403；有 → 允许，且审计；不能借 Bot 改权限。
- [ ] 伪造 `X-Tenant-ID`/`X-Account-ID`/`X-Bot-ID` → 被服务端覆盖/拒绝。
- [ ] CSRF：无 token 的写请求 403；跨站 cookie 不生效。
- [ ] 会话固定/重放：旧 cookie、登出后 cookie、改密前 cookie → 401。
- [ ] Bot key：错误 Bot、吊销 key、跨租户 key → 401/403；key 不出现在日志/响应。
- [ ] `grant ⊆ own` 绕过：直接 API、组嵌套、scope 提升、超管提升 → 全部拒绝。
- [ ] ReBAC 绕过：无关系读/写、伪造 relation、删除后缓存仍放行 → 全部拒绝。
- [ ] 旧路由/旧 schema/旧凭证 → 404/401/启动拒绝。
- [ ] 登录限速/锁定可被绕过（X-Forwarded-For 伪造等）→ 不能绕过。

### B 功能与链路红队（P0/P1）

- [ ] 真实角色旅程：超管 bootstrap → 建组 → 建账号 → 建 Bot → 发 key → 建 TeamBot → 启动 → 成员产出 → 队长汇总 → 审计/DAG 可见。
- [ ] 每个按钮/表单真实有效；“toast 成功但数据没变”判 P0。
- [ ] 五态在真实数据下可复现；空态有引导；权限不足有解释。
- [ ] 上游产出下游可见：Bot 创建 → TeamBot roster 可选；权限组变更 → 账号有效权限变化；Session 共享 → 对方可见。
- [ ] 实时性：权限撤销/账号禁用/Bot 归档在 2s 内反映到 UI 与下一次调用。
- [ ] 管家 Bot 场景完整跑通。
- [ ] 审计/DAG 与真实操作逐条对得上；不丢边、不重复、不伪造。

### C 视觉与设备红队（P0/P1）

- [ ] 375/390/768/1440 四档截图；页面可滚动、无横向溢出、关键操作可达。
- [ ] 五态视觉均有设计；不是“只有成功态”。
- [ ] 材质/对比度/字号阶梯/间距档位/触控目标/焦点环达标。
- [ ] 单行标签不折行；表格/卡片不出现半个字形；溢出有交代。
- [ ] 权限不足页不是白屏；有缺什么/如何获取。
- [ ] 视觉层级：主操作唯一、次操作可辨；不是“所有按钮一样重”。

## 5. E2E 报告（脚本生成，禁手写）

步骤至少覆盖：

1. 空库启动 → bootstrap token → 设超管 → 强制改密 → 登录。
2. 建权限组（含 `session.read/own`、`bot.invoke`）→ 建 human 账号 → 加入组。
3. 建 Bot（Profile/ModelKind/默认模型/Harness）→ 查看详情。
4. 签发 Bot key → 以 Bot 身份调用工具 → 错误 Bot 403 → 吊销后 401。
5. 建 TeamBot（Mission/Roster）→ 跨账号成员（有/无 `bot.invoke` 对照）→ 启动。
6. 管家 Bot：一句话建团队 + 配权限 + 启动；DAG/审计展示。
7. Session 默认私有 → 分享 viewer → 对方可见 → 撤销 → 403。
8. 旧路由/旧 CLI/旧 cookie/旧 key 负控。
9. 四档 viewport + 五态截图。
10. 审计导出/查询：谁、何时、对什么、结果。

产物：

- `steps.json` → `report.html`（统一脚本，禁手写）。
- 每步截图；失败带错误原文与截图。
- 改动文件 before/after 覆盖表，UNCOVERED=0；无视觉面按三类豁免逐行登记。
- 报告公网链接（禁 localhost）；索引写入 `.agents/docs/E2E-REPORTS.md`。

## 6. 工程门

- [ ] `go build ./...`、`go vet ./...`。
- [ ] `go test -p 1 -count=1 ./...`（仅导出 `KEEL_TEST_DSN`，独立测试库）。
- [ ] `scripts/conformance.sh`。
- [ ] `nsh check`、`nsh test --target tui`、`nsh build --target web`。
- [ ] `openspec validate keel-bot-identity-access --strict`。
- [ ] 权限覆盖率门（路由/工具/CLI/UI 动作清单快照）。
- [ ] 架构测试：kernel 不 import access/accounts/bots。
- [ ] 无孤儿进程/端口/临时 DB；测试结束回收并留证。

## 7. 完成定义（DoD）

只有同时满足才可勾“完成”：

1. 本文件 §2–§6 全部通过；P0/P1 = 0。
2. 三轴由三个独立子代理执行，实现方未兼任；修复后对应轴全量重跑。
3. E2E 报告公网可开；每步截图；改动文件覆盖 UNCOVERED=0。
4. `openspec validate --strict` 通过；任务板全部勾选；证据落盘。
5. 独立 A/B 复核评分 ≥8/10（或等价），无未关闭 P0/P1。
6. 合入 main 用 rebase/squash，无 merge commit；子仓先推、主仓指针后跟。
7. 部署到 alpha 后由 fresh 子代理复验关键旅程；报告与链接可开。
