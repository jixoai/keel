# P6.4 最终交叉复核（2026-11-20）

对象：`5518646`（代码）+ P6V 视觉修复 `f54d464`；证据见 `ACCEPTANCE.md` 与各 `evidence/p6/*`。

## Reviewer（独立只读）

- 复跑：`go build`/`go vet`/六个指定包、`conformance.sh` 1077/0/1、`nsh check`、
  `nsh test --target tui` 174/174（快照时点）、`nsh build`/`package`、openspec strict——全部 EXIT 0。
- G0–G5 逐项核验：G2 kill 点/seq/绑定、G3 六族/13 series/审计、G5 1000 会话/64 run/200fps/
  cgroup 416.5MiB/15 kill/38 重连、browser/degrade 断言矩阵、cmd/e2e core 8/8×2——与
  ACCEPTANCE 一致；仅 4 处口径措辞已修正（11 步、54/57 行、5009ms、最终 conformance 输出已归档）。
- 新修复复核：A13 双位置声明条件注入、A1 domain 投影、登录能力刷新、`mcpAppToolInput` 解包、
  `frameLoadVerdict` 首载分类均正确，自导航/budget/display-mode 行为未弱化。
- 开放项裁决：P3.6 NSH allow、A5 第二 origin、A7 teardown、MRTR env、5 个 Non-Goal 均为
  「已接受偏离/登记后续」，不构成 P0/P1；tasks 42/50 真实。
- 结论：**P0=0 / P1=0 / P2=6（文档口径与测试注释）/ 9/10 / pass / archive ready**。

## Vision（独立截图判读）

- 首轮：fail（P1 全屏未占满、iframe 不增高、1970 时间）。
- P6V 修复（`f54d464`：Dialog 100%+fill、iframe flex、epoch 时间隐藏、降级隐藏全屏按钮）后
  复跑 `browser/final2` + `degrade/final2`，复审：全屏四边贴齐、iframe 撑满、1970 消失、
  降级无全屏按钮，无新增 P0/P1。
- 结论：**P0=0 / P1=0 / P2=0 / pass**。

## 归档结论

P6.4 通过。剩余开放项仅为已登记的上游依赖/Non-Goal；P6.6 归档与主仓指针按交付流程执行。
