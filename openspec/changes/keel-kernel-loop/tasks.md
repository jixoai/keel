# Tasks: keel-kernel-loop

## K0. 边界与基线

- [x] K0.1 内核包纯度测试：`internal/{loop,agentcore,agenttool,kernel,agent}` 不得 import
      `internal/agentapi|agentext|web`（`go list -deps` 断言，进 CI）
- [x] K0.2 `docs/KERNEL.md`：内核边界（loop/事件/工具/账本）与「产品能力不进内核」清单
- [x] K0.3 R1 allow 预登记：`assistant.message.payload.inputSeqs`（值钉死：seq 列表形状）

## K1. 投递模式

- [x] K1.1 `user.message.payload.delivery`（`queue|guide`，缺省 queue；非法值 400）
- [x] K1.2 runner 接线 `GetSteeringMessages`：只读未消费 guide（seq 升序），PI 三个 drain 点
- [x] K1.3 降级规则：非 streaming / 有 stop / 有附件 → 降级 queue（事件里可见）
- [x] K1.4 单测：工具轮边界注入（全部 sibling results 后、一次一条）、并行工具提前完成不注入、
      stop 期间不注入、降级

## K2. 输入消费事实

- [x] K2.1 `assistant.message.payload.inputSeqs`：runner 记录本次请求呈现的 user 输入 seq
- [x] K2.2 「未答输入」由 inputSeqs 判定（`owedTurn`/`pending_input` 改造），水位降级为诊断
- [x] K2.3 崩溃矩阵：模型流中 kill / 工具轮中 kill / assistant 提交后 Finish 前 kill /
      queue+guide 混排 / 取消与完成竞争；断言不丢不重
- [x] K2.4 迁移兼容：历史事件无 inputSeqs 时回退水位判定（并记诊断），不得重复答

## K3. 运行摘要

- [x] K3.1 `run.finished` kind + worker 收口提交（done/failed/cancelled），字段齐全
- [x] K3.2 转写忽略 `run.finished`（不进模型上下文）；R1/ conformance 不退化
- [x] K3.3 单测：失败/取消也摘要；摘要失败不改变 run 终态

## K4. 回归

- [x] K4.1 既有 11 门全量重跑（race/conformance/R1/E2E/drain/inflight/smoke/chaos×2/perf×2）——16 门全绿（2026-11-09）
- [x] K4.2 内核纯度/边界/性能无退化；文档（KERNEL/CHAT-SURFACE 的 loop 部分）更新
