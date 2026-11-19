# Wave 2 最终 A/B 复核处置（2026-11-20）

对象：冻结 `b68cd81`（jobcap `53f87d3`/`cd73a31` + aclack `fcbce3a`）。两轴独立 reviewer，
均自跑 build/race/真库 DSN 复现。**A 轴 8.5/10，B 轴 9.3/10；两轴 P0=0、P1=0。**

## A 轴结论

六个审查面全部通过：kernel 系统通道无客户端触达面（`System: true` 提交点全仓 5 处均为进程内）、
jobId 唯一写入点不可伪造、双通道去重键隔离、jobqueue 无身份泄漏、notify capability 语义收敛
（错 token 404 / 有效 token 410 无 oracle）、spawn selection 零副作用、MCP image/arguments 无绕过、
证据零虚报。P2×7 均不阻塞。

## B 轴结论

jobcap 声明-实现-测试逐条对应（真库测试先释放父 lease 再启动作业，非 tautology）；aclack 四项
全部落地且真断言；记账 43/146 与任务板精确一致，新增三项未勾主动暴露；supersede 注记经树哈希
比对忠实。P2×5（两条测试缺口 + 三条卫生）。

## P2 处置

| 来源 | 问题 | 处置 |
|---|---|---|
| B P2-1 | system lane capture 失败路径无测试 | **已修（4dad05b，合入 71424ed）**：`TestBackgroundJobCaptureFailureCarriesJobID` 断言 jobId、owner/job 失败键不合并 |
| B P2-2 | kernel 不强制 system capture 的 jobId | **已修（4dad05b）**：`validateSystemCapturePayload` 在 `commitOnce` 拒绝缺/空/非字符串 jobId；owner 通道不受影响 |
| B P2-3 | 空 jobId 卫门无单测 | **已修（4dad05b）**：`TestSystemCaptureRequiresJobID` |
| B P2-4 | 复核 DSN 遗留 `keel_agent_test_*` 库；residue 检查不扫该模式 | **backlog**：不在本轮删（归属不确定）；residue 脚本补扫模式 |
| B P2-5 | fallback 注释「once per process」措辞 | **已修（4dad05b）**：「once per provider instance」 |
| A P2-1 | 跨 lane dedupe 只靠键值构造（`job:` 前缀），kernel 键空间不按 lane | **backlog（结构性）**：`Lookup` 无 lane 参数，加 `sys:` 前缀需改查询契约；当前仅本会话自伤的恶意 toolCallId 碰撞面，无跨租户/提权 |
| A P2-2 | 失败键 owner/job 值域分离但不完全对称 | **backlog**：注释级对齐 |
| A P2-3 | 三态推送仅单进程成立（多实例窗口） | **backlog**：多实例部署前 store 层复查 status |
| A P2-4 | `mcp.call` object 强制只在 toolsurface seam | **backlog**：接口注释声明契约归属 |
| A P2-5 | MCP image 上限不约束远程 URL 的 provider 侧拉取 | 文档已注明「provider 自限」；保持 |
| A P2-6 | OpenURL/TrackDelivery 错误优先级互反 | **backlog**：均 410，无客户端可见差异 |
| A P2-7 | README supersede 引用 squash 前提交 id | **已修（4dad05b）**：补「squash 前切片提交；合并树为 53f87d3」 |

## 结论

Wave 2 收口条件满足：P0/P1 清零；P2 中与正确性直接相关的（失败路径、kernel 强制归因）已修，
其余为结构性/多实例/文档级，已挂 `docs/RESIDUAL-PLAN.md`。所有证据可复跑，DSN 零泄漏。
