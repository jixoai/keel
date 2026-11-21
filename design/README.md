# Design artifacts: keel-bot-identity-access

> 状态：**方向已定（B 账簿 + A 索引/DAG + C 待办）；两轮视觉评审 7.5 → 8.2/10，P0 0 条；第二轮 6 项硬门已修；待 Owner 确认与第三轮复评**。旧 UI 不是基线。

## 内容

| 文件 | 用途 |
|---|---|
| `PRODUCT-DESIGN.md` | 产品定位、人物/故事、IA、页面规格、五态、文案、响应式、设计语言 |
| `DIRECTIONS.md` | 2–3 个真正不同的方向 + seed challenger、品类地板、推荐与取舍 |
| `REVIEW.md` | 待拍板点（≤5）、与 acceptance 的对应、独立评审状态、本轮修复与仍存问题 |
| `VISUAL-REVIEW.md` | 第一轮独立视觉评审（7.5/10，YES WITH FIXES） |
| `VISUAL-REVIEW-2.md` | 第二轮独立视觉评审（8.2/10，P0 0 条，6 项硬门） |
| `DECISION-2026-11-22.md` | MainAgent 在 Owner 授权下的方向决定 |
| `opendesign/` | OpenDesign 设计板（`index.html`）、`DESIGN.md`、artifact 元数据；仓库内副本，外部工作区为 `.../Open Design/.../projects/keel-bot-identity-access/` |

## 方向摘要

- **A 控制台**：左栏 + 列表/详情 + 页签；baseline，与旧设置页同构，不作为最终方案。
- **B 账簿（推荐）**：顶部章节带 + 页边 spine + 纸面带；身份 1440 280 索引 / 1024 全宽表；Bot 四列能力表；运行泳道 + DAG 第二视图；05 记录页；移动端 44px + 停止确认。
- **C 任务简报**：待办 + 运行 + 名册横条；挑战者，其“待我处理”被 B 吸收。
- **D 索引表单**：seed challenger，只作 divergence probe，不进入实现。

## 打开方式

- 仓库副本：直接用浏览器打开 `design/opendesign/index.html`（静态单文件）。
- OpenDesign 工作区：`~/Library/Application Support/Open Design/namespaces/release-stable/data/projects/keel-bot-identity-access/index.html`。

## 纪律

- 设计板使用 representative mock 数据，标注“设计样例”；生产 UI 不得用假数据。
- 组件必须映射回 shadcn 官方件 + solid-glass + 主题令牌 + lucide 单色；禁手写仿制。
- 方向未选定前不进入 UI 实现。
