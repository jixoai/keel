# Wave 2 B 轴功能复核与处置（2026-11-19）

对象：冻结快照 `06e3747`（净变更 `2b26a6b..06e3747`）。复核：独立 reviewer 子代理（自跑 12 包
`-race`、PG 集成套件、100k capture 规模门，并亲自复跑 E2E；均 PASS、零残留）。
**结论：8/10，2 个 P1（均为装配/记账问题，非组件缺陷）。**

## P1 与处置

| # | 问题 | 处置 |
|---|---|---|
| P1-1 | 生产装配缺 seam：`serve` 的 bash 桥用空配置（每次 Run 新建临时目录），`fileplugin.Options.Surface/CaptureRoot` 未注入 → 生产下 `write`→`bash cat` 不通、两次 bash 调用间状态不持久；`gbashfs` 全仓无非测试引用 | **W2.seam 切片接线中**：按 session 的 layerfs workspace（非根挂载）→ 同一 workspace 作 `Surface`、capture 回 layerfs+manifest；装配级测试为收口门 |
| P1-2 | 沙箱内 `nohup/jobs/ps` 命令未实现（`jobs` 走上游假表 → 127），但任务板曾按已交付口径勾选 | ①任务板改回未勾并注明已交付/未完成范围；②三命令追加进 W2.seam 范围（读 JobManager；`fg/bg` 按契约不注册）；③装配级测试断言 keel 作业 id、无宿主 PID |

## 非阻塞处置

- **taskId 字面偏离**：B 轴裁定「实质接受、程序须补 spec delta」。已修订 `specs/task-log/spec.md`
  的 requirement 与场景（Harness 先归属初始卡、无法归属 `task_id_required`、未知卡 `task_not_found`），
  并在 tasks 勾选中注明裁决来源。
- `LinkSubagent` 挂在 `keel.tasks` 插件 hook 上（插件停用时子卡不链接）→ 记 backlog：把链接移到
  harness spawn 路径。
- 陈旧注释（`tasklog/service.go:350-351` "until layerfs lands"）与死代码
  （`toolsurface/surface.go` `singleTool`）→ 交 W2.seam 顺手清理。
- `MaxGlobal` 实为按租户计数 → 多租户 pack 落地前重命名/重定义（记 backlog）。
- 其余确认无虚报：配额/ capture upsert / fs.write 栅栏精度 / notify Pusher seam / 测试强度 /
  7 张新表 RLS 覆盖 / DDL 与 `fileplugin.SchemaDDL` 逐字段一致 / gbash 契约遵循（pin、禁 Timeout、
  非根挂载、64KiB 上限、双许可文本）。

## 评分依据

超时转后台取消语义、单次推送 CAS、审批 provenance actor 升级、capture stat 缓存与单事件结算、
内核键空间负控均「声明即实现、实现即有测、测即真断言」；扣分：P1-1 使两条冻结 spec 场景在生产
装配下不成立（-1）、P1-2 记账超实态（-0.5）、taskId 偏离程序未走（-0.5，已补）。
