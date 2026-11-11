# Acceptance Criteria（验收标准）

本文是 `keel-harness-v2` 的收口门。判据全部可执行、可复跑、可被负控推翻；证据必须是**产物**
（报告/截图/命令输出/曲线），不接受自评文字。评审按项目纪律**只用子代理交叉复核，不启动 Codex**。

## 0. 口径

- **验收单元**：`proposal.md`、`design.md`、`tasks.md`、9 份 spec（含 `plugin-system` 的 MODIFIED delta）、`plugin-inventory.md`、
  `plugin-boundaries.md`、`state-machines.md`、本文件、`adr/0001-layerfs-rust-grpc.md` 逐条对表；
  未覆盖项必须显式登记并说明为什么不可验收。
- **预算（硬）**：后端整体（各 group worker + layerfs + 控制面）**目标 ≤300MB，上限 ≤500MB**。
  - **RSS**：各后端进程 `/proc/<pid>/status:VmRSS` 定时采样（≤10s）求和；`VmRSS` 不含 page cache。
  - **cgroup peak**：同时单列 `memory.peak`（含 page cache），仅用于与 v1 基线对比。
  - **作用域**：预算指全机后端总和；MUST 另报每 group 的分解。
  - **基线**：v1 为 cgroup peak **523.8–639.1MB**；RSS 基线 MUST 在本 change 实测补登，
    MUST NOT 用 cgroup peak 与 RSS 直接比较。
- **前端可分离**：NSH 客户端可跑在另一台机器 / 手机浏览器；surface VM 配额单独断言
  （默认单 surface 32–64MB，关闭即卸），不计入后端预算，但不得回退。
- **负控**：每道门必须给出**能把它判失败**的构造，否则该门不成立。
- **原生 target**：由 NSH 多平台构建直接产出；本 change 不另起构建/签名产品线。

## E0 既有门零回归

- **判据**：`scripts/acceptance-all.sh` 全绿；R1 差分 `unexpected=0`，allowlist 每条附理由；
  新事件 kind / payload 变更后 MUST 重新 pin 基线并更新 `cmd/r1diff` 场景。
- **证据**：命令输出 + allowlist diff + 基线 pin 记录。
- **负控**：故意改一个事件字段 → R1 必须报 unexpected>0。

## G0 契约门

- **判据**：内核包不得 import 产品层；插件不得访问同层内部状态；无第二套插件运行时；
  **模型可见工具集精确等于 11 个内建工具 ∪ 已审批 `provides.tools`**；迁移表「删除」列在代码中
  `grep` 零命中（测试夹具白名单除外）。
- **证据**：`go test ./internal/architecture/...`、工具清单快照、迁移 grep、依赖图；
  RSS 门由 `scripts/acceptance-rss.sh` 并入 `acceptance-all.sh` 执行。
- **负控**：加一行 `import placement`、注册 `spawn_subagent`、或让插件直读另一插件状态 → 失败。

## G1 状态机门

- **判据**：`state-machines.md` 每台状态机有**转移矩阵测试**：合法转移通过，**非法转移被拒且
  留下拒绝事实**；`A5` 的每个 kind 在 `kindRegistry` 有 class/key 单测；静态检查证明不存在
  "仅内存态"（每个状态指到权威事实，脚本输出状态→事实映射表）。
- **证据**：每台状态机测试清单 + 拒绝事实样例 + kindRegistry 单测输出。
- **负控**：终态会话 resume、跨 epoch 双写、无 `taskId` 调用、超额放置、compaction 中提交工具。

## G2 恢复门（默认自愈）

- **判据**：`kill -9` 与容器重启后，**全部非终态 run 自动续跑，零人工干预**；中断的子代理
  **继续原任务**（不是新开替代者）；恢复次数/原因/所用配置可查；恢复不重复已提交副作用；
  `resume.mode=never` 的会话**不**自动恢复并列为待恢复。
- **证据**：六格崩溃矩阵（模型流/工具/追加/压缩/capture/SSE）+ 容器重启 E2E + seq 连续性 +
  幂等断言。
- **负控**：同一 `requestId` 重复 resume → 只有一次 attempt。

## G3 幂等与冲突门

- **判据**：有副作用工具重试只一次效果；文件写前置条件不命中 `conflict`；webhook 重投不产生
  第二次会话；`approve` 重复作答 409；provider 额度耗尽后停止对该 provider 重试。
- **证据**：账本片段 + 计数断言。
- **负控**：同一条 webhook 投两次、同一审批答案提交两次 → 计数不增。

## G4 内存门

- **判据**：按 §0 的测量协议，后端 RSS **≤500MB（上限）/ ≤300MB（目标）**，覆盖：单机组
  `pack` × N 会话 × M 并发 run、layerfs 1GB 流式读写、`npm install` 十万文件 capture、
  1000 会话空闲态、打开/关闭重型面板；**组 OOM 不扩散**；前端 surface VM 配额执行且关闭回落。
- **证据**：RSS 曲线 + cgroup peak + 全机/每 group 分解 + 面板前后对比。
- **负控**：写入超配额 blob → `quota_exceeded`；关闭面板后内存不回落 → 失败。

## G5 任务门

- **判据**：每次工具调用有 `taskId`；任务树跨会话可拼；fork 原点 `eventSeq` 精确；资源只记
  `(path, hash)`；**`write` 后 `bash` 立即可读**；**十万文件 capture 只产生一条批事件**。
- **证据**：任务面板截图 + 事件片段 + blob 引用计数 + 大目录 capture 事件计数。
- **负控**：无 `taskId` 调用、fork 后写父文件再读子会话、十万文件产生 >1 条 capture 事件。

## G6 插件门

- **判据**：安装/启停/卸载**零残留**；缺依赖 `PENDING` 且原因可见；HMR 失败回滚且不打断 run；
  `extensibility.json` 缺失的官方插件发布被拒；**`catalog.json` 为唯一机读真源**，
  `COVERAGE.md` 每行映射到 `keelPluginId`，`plugin-inventory.md` 覆盖所有 `keelPluginId`，
  `status`/`wave` 一致。
- **证据**：注册表 diff + generation 序列 + catalog 对账脚本输出。
- **负控**：卸载同时注册工具与 UI 贡献的插件；缺文档的包发布。

## G7 UI 与槽位门

- **判据**：槽位由插件声明（Harness 无内置清单）；贡献只用槽位允许的形态；主题令牌来自宿主
  （无硬编码色值/阴影）；`iframe` 显式标注非原生主题；**TUI 中 `package`/`iframe` 显式不可用、
  `chunk` 正常渲染**；**至少一个原生 target 渲染 chunk**；桥按能力裁剪，越权 `ERR_ACCESS_DENIED`；
  系统级推送在原生 target 用系统通知，web target 无 service worker 时显式降级（登记缺口）。
- **证据**：web + **`macos`（`nsh package --target macos`）** + TUI（`nsh test --target tui`）
  三组截图/日志；越权审计；主题切换前后截图。
- **负控**：给只允许 `chunk` 的槽位投 `iframe`；越权桥调用。

## G8 创造模式门

- **判据**：**AI 现场写出一个 Native 插件**（chunk 槽位贡献），从聚合目录出发 → 生成包 →
  校验 → 审批安装 → 在 **web 与 `macos` 原生 target** 渲染 → 卸载无残留；给出 TUI 渲染或
  显式降级证据；缺文档/非法形态/硬编码令牌的包被拒且理由字段级可读。
- **证据**：AI 会话记录 + 生成的包 + 三端截图 + 卸载 diff。
- **负控**：硬编码主题色、贡献到不存在的槽位、缺 `extensibility.json` → 安装前被拒。

## G9 有界门

- **判据**：工具输出有界（head/tail + blob 引用，200MB 输出不整段进内存/账本）；并发槽位满时
  明确排队（带 ETA）或 429（带 `retry_after`）；每组后台作业有上限，超限拒绝新作业且不 kill
  已有；**十万文件 capture：账本事件数 ≤ 1 + 显式写次数，RSS 不随文件数线性增长**。
- **证据**：大输出 RSS 曲线 + 429/排队样例 + 作业上限 + 大目录事件计数。
- **负控**：连续提交超过上限的作业 → 被拒且既有作业存活。

## G10 资源回收门

- **判据**：任何测试/走查结束后**零孤儿**：无残留进程（sidecar、外部运行时、后台作业、
  layerfs）、无残留监听端口、无残留 VM；SIGKILL 后 sweep 能回收或明确标记 `lost`；端口在
  `.agents/docs/PORT.md` 登记。
- **证据**：`ps`/`lsof` 前后对照 + 端口台账核对。
- **负控**：sidecar 脱离进程组 → sweep 回收或标 lost。

## G11 交付面门

- **判据**：E2E 报告为 **HTML 图文文档**（每步截图 + 做了什么/预期/实际断言/关键日志），落
  `.agents/reports/<group>/<fileName>/` 并随主仓入库；**每个改动过的文件**在审计清单里有对应
  页面/状态（视觉文件 before/after 成对），`UNCOVERED=0`；子代理交叉复核；layerfs 的 ADR
  证据门（1000 并发流 RSS/p99/GC 停顿、tree 去重收益、fork O(1)）齐备或已回退。
- **证据**：`report.html` + 覆盖表 + 复核结论 + ADR 证据。
- **负控**：抽查未截图改动文件 → UNCOVERED。

## 附：门的依赖顺序

```
E0 既有门 → G0 契约 → G1 状态机 → { G2 恢复, G3 幂等, G5 任务 }
  → { G4 内存, G6 插件, G7 UI, G9 有界 } → G8 创造模式 → G10 资源回收 → G11 交付面
```

前置门未绿时，后续门的结果不作为验收依据。
