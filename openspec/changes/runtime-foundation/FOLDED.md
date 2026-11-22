# FOLDED — runtime-foundation

> 状态：2026-11-22 Owner 裁决。`runtime-foundation` 不再单独推进；8 项残余折叠进
> `keel-harness-v2` 的 W11 收口门（G4/G10 为主，chaos 落 G2，交付证据落 G11）。
> 本文件不改 `runtime-foundation/tasks.md`：8 条保持未勾；也不把任何实现写成已完成。

## 映射（8 项残余 → W11 门）

| ID | `runtime-foundation/tasks.md` 残余（行） | W11 门 | 收口要验证什么 |
|---|---|---|---|
| RF-1 | 主仓挂 submodule + PROJECT-MAP 登记（L14） | G10 | keel 指针与主仓登记一致；端口/资源台账清理；无残留 |
| RF-2 | 为 loop 补单测（fake stream：文本/工具/压缩/取消）（L25） | G2（兼 G3） | 崩溃恢复路径的 loop 行为有可复跑证据；取消/压缩不重复副作用 |
| RF-3 | loop 的持久任务接线（工具执行/压缩 `resume()`）（L35） | G2 | kill -9/容器重启后非终态 run 自动续跑；子代理继续原任务 |
| RF-4 | 缩略图异步生成（可选）（L53） | G4 | 大图/多会话下 RSS 有界；异步生成不把整图进内存 |
| RF-5 | RLS 可选启用 + 非 owner 角色（L68） | G4 | 租户隔离与配额一起验；越权读取/写入被拒 |
| RF-6 | 每租户配额（并发/带宽/存储）（L69） | G4 | 并发、带宽、存储三配额有界；超限返回明确错误 |
| RF-7 | HTML 报告 + 公网链接（L77） | G11 | HTML 图文 E2E、每步截图、before/after 覆盖表、公网链接 |
| RF-8 | 子代理交叉复核（本项目不使用 Codex）（L78） | G11 | 独立子代理复核；P0/P1 清零；不复用实现方自评 |

## 口径

- **G2**：chaos/恢复证据；`RF-2`/`RF-3` 是 loop 级支撑，不替代 W11 的 kill -9/容器重启 E2E。
- **G4**：RSS/RLS/quota 一起收口；RSS 采样按 `acceptance.md` §0 的 `VmRSS` 协议，目标 ≤300MB、
  上限 ≤500MB；cgroup peak 单列。
- **G10**：清理与登记；`RF-1` 只做指针/台账/登记核对，不重开 runtime-foundation 线。
- **G11**：交付面；`RF-7`/`RF-8` 是报告与独立复核，不得用手写摘要替代。

## 执行约束

- 8 项只在 `keel-harness-v2/tasks.md` 末尾“W11 收口补充（from runtime-foundation）”
  段跟踪；`runtime-foundation` 板不再勾选。
- 任何一项若被判定不属于 Alpha 垂直切片，留在本文件作为 backlog，不阻塞 W11 其他门。
- 不得把本文件当作“已折叠 = 已完成”；折叠只表示归属改变。
