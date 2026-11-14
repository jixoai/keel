# ui-craft Specification

## ADDED Requirements

### Requirement: 界面细节改进不改变行为

改进线 MUST 只修改呈现层（样式、文案、可访问性属性、焦点顺序），MUST NOT 改变接口、
数据流、组件契约或既有验收。任何一条改动 MUST 能用「问题 → 证据 → 改动 → 回归」四段
说明。

#### Scenario: 提交一条 UI 改进
- **WHEN** 一条改动被标记为完成
- **THEN** 记录含问题、证据（截图或测试）、改动文件与回归结果
- **AND** 主线既有测试全部保持通过

### Requirement: 可访问性是硬门

交互元素 MUST 可通过键盘到达并有可见焦点；控件 MUST 有可读名称；文本对比度 MUST 满足
WCAG 2.2 AA；错误与空态 MUST 说明下一步。

#### Scenario: 键盘走查
- **WHEN** 用户只用 Tab/Shift+Tab/Enter/Space 操作任一页面
- **THEN** 每个可交互元素都能到达、激活，且焦点始终可见
