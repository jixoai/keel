# 审批与问句

不会发问的智能体只能猜。因此 Harness 有一个一等原语——**decision**——应对一切需要人回答的
事情：工具审批、澄清问句、选项抉择。

设计规则与其他部分一致：原语住在产品层，账本的词汇表不增长。Decision **不是**内核事件
kind（`internal/kernel/validate.go:36`）；它是投影到既有 tasks 记账表上的 durable 行，
带答案日志（`internal/agentext/doc.go`）。

## 1. 原语

```
open ──▶ （等待中）──┬─▶ answered     （选项和/或自由文本；答案即终局）
                     ├─▶ timed_out    （有界；默认 5m，上限 24h）
                     └─▶ cancelled    （会话被取消）
```

- 状态：`open`、`answered`、`timed_out`、`cancelled`
  （`internal/agentext/decisions.go:28`）。
- 答案是选项 id、自由文本，或两者——只有声明了 `allowFreeText` 的 decision 才接受自由文本
  （`internal/agentext/decisions.go:49`、`:309`）。
- 超时按 decision 设界：open 时的 `timeoutMs`，默认 5 分钟，上限 24 小时
  （`internal/agentext/decisions.go:162`、`:163`）。
- 每会话对进程内记账设界（512 条 decision）；durable 行本身无上限，但一个打开数千条
  decision 的会话是 bug（`internal/agentext/decisions.go:167`）。

## 2. 身份与幂等

两把键，都是 durable，处于不同层级：

| 键 | 作用域 | 含义 |
| --- | --- | --- |
| `requestId`（开题者的键） | 问题 | 同一键开两次返回同一条 decision（`DecisionIDFor`，`internal/agentext/decisions.go:222`） |
| `decisionId`（确定性 UUID） | 答案 | 作答幂等：同一答案重放为 no-op；*不同*答案是冲突，绝不覆盖 |

答案语义，与实现逐字一致（`internal/agentext/decisions.go:309`）：

- 已是 `answered` 且答案**相同** → 返回该行，`deduped=true`；
- 已是 `answered` 但答案**不同** → `ErrDecisionConflict`（HTTP 409）；
- 已是 `timed_out`/`cancelled` → `ErrDecisionClosed`（HTTP 409）；
- 未知选项 id，或不允许时使用自由文本 → `ErrInvalidDecision`（HTTP 400）。

由于 decision id 是确定性的，网络故障后重发作答的客户端会收敛，而不是把对话分叉。

## 3. 一条问句的生命周期

```
run（模型或工具策略需要人来拍板）
  │  DecisionBroker.Open {requestId, kind, prompt, options, allowFreeText, timeoutMs}
  │    ├─ durable 行落库（tasks 记账表）                       decisions.go:233
  │    ├─ 发出 hook：decision.opened                          hooks.go:25
  │    └─ 排定超时（有界、可取消）                            decisions.go:542
  ▼
客户端看到问题
  │  GET  /v1/sessions/{id}/decisions            （列表）
  │  GET  /v1/sessions/{id}/decisions/{decisionId}
  │  POST /v1/sessions/{id}/decisions/{decisionId}  {optionId?|text?}
  │    └─ 首个答案胜出；hook：decision.answered              decisions.go:606
  ▼
等待者恢复
  │  DecisionBroker.Wait 返回终态 decision                    decisions.go:374
  ▼
run 继续（或结算）——答案重新成为事实
```

取消会话会一次取消其全部未决 decision——`CancelSession`
（`internal/agentext/decisions.go:437`）——被停止的 run 绝不会留下无人可答的问题。

broker 也是答案日志的唯一居所：行自带有序事件（`DecisionEvent`
`internal/agentext/decisions.go:78`），运维因此无需触碰账本就能重建「何时问、何时答」。

## 4. 抵达客户端

Decision 以两种等价方式暴露给客户端：

1. **HTTP**——`/v1/sessions/{id}/decisions` 路由，由产品 API 与其他 `/v1` 路由挂在一起
   （`internal/agentapi/api.go:173`）。wire 类型是 `Decision` 文档（id、kind、prompt、
   options、recommendation、status、answer、时间戳）。
2. **投影**——UI 消费 `decision` 事件。宿主可以把决策 API 的事件喂进去，或从 SSE 邻近通道
   合成同形状事件；投影只认识事件形状，从不访问该 API（`docs/CHAT-SURFACE.md` §3）。

投影把未作答的 decision 当作**中断**：带未决 decision 结束的 run 不是「完成」；AG-UI 客户端
收到 `RUN_FINISHED {outcome: {type: "interrupt", interrupts: [...]}}`，其中
`interrupt.id = decisionId`（`docs/CHAT-SURFACE.md` §7）。人的答案随后开启新的 run——问题
永不阻塞 worker 线程。

若部署没有装配 decision broker，路由返回 503，且 `GET /v1/capabilities` 报告
`"decisions": false`，客户端据此隐藏该区域而不是瞎猜（`internal/agentapi/api.go:261`）。

## 5. Decision 不是什么

- **不是内核事件。** 内核从不知道审批的语义；它只会看到被提交的下一条
  `user.message`/`assistant.message`（`internal/agentext/doc.go`）。
- **不是 hook。** Hook 观察；decision 询问并阻塞。Hook 总线携带
  `decision.opened`/`decision.answered`，作为原语的*通知*（`internal/agentext/hooks.go:25`），
  而不是机制本身。
- **不是 UI 状态。** UI 可以乐观渲染，但 durable 行才是答案的权威；刷新会从 API 重新读取。

## 6. 规划中：面向插件的审批

规划中的插件标准新增一个消费者：插件 UI 可以通过宿主桥（`keel.plugin.*`）**请求审批**；且
插件安装/启用本身必须经过 durable 审批才会生效
（`openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`「权限与沙箱」）。两者均标注
为**规划中**——当前检出既无插件桥，也无插件安装路径。
