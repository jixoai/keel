# Design artifacts: keel-bot-identity-access

> 状态：**方向待选定**。旧 UI 不是基线；本目录是 OpenDesign 产出与评审包。
> 实现前必须完成：独立视觉评审 → Owner/评审选定方向 → 再开 L5。

## 内容

| 文件 | 用途 |
|---|---|
| `PRODUCT-DESIGN.md` | 产品定位、人物/故事、IA、页面规格、五态、文案、响应式、设计语言 |
| `DIRECTIONS.md` | 2–3 个真正不同的方向 + seed challenger、品类地板、推荐与取舍 |
| `REVIEW.md` | 待拍板点（≤5）、与 acceptance 的对应、独立评审状态 |
| `opendesign/` | OpenDesign 设计板（`index.html`）、`DESIGN.md`、artifact 元数据；仓库内副本，外部工作区为 `.../Open Design/.../projects/keel-bot-identity-access/` |

## 方向摘要

- **A 控制台**：左栏 + 列表/详情 + 页签；baseline，与旧设置页同构，不作为最终方案。
- **B 账簿（推荐）**：顶部章节带 + 页边 spine + 纸面带；身份/记录心智；drill-in。
- **C 任务简报**：待办 + 运行 + 名册横条；挑战者，其“待我处理”被 B 吸收。
- **D 索引表单**：seed challenger，只作 divergence probe，不进入实现。

## 打开方式

- 仓库副本：直接用浏览器打开 `design/opendesign/index.html`（静态单文件）。
- OpenDesign 工作区：`~/Library/Application Support/Open Design/namespaces/release-stable/data/projects/keel-bot-identity-access/index.html`。

## 纪律

- 设计板使用 representative mock 数据，标注“设计样例”；生产 UI 不得用假数据。
- 组件必须映射回 shadcn 官方件 + solid-glass + 主题令牌 + lucide 单色；禁手写仿制。
- 方向未选定前不进入 UI 实现。
