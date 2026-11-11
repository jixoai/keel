# Plugin Architecture（插件种类、装配、seam 与热重载）

本 spec 定义 Keel Harness v2 的插件分类、装配模型、能力 seam 与生命周期。DSH 是参照，
不是事实源：我们借它的**装配与 seam 思想**，不借它的 Cordis/typert 运行时与
「一能力一工具」形态。完整插件清单见 `plugin-inventory.md`。

## ADDED Requirements

### Requirement: 插件 kind 与装载边界

插件 MUST 声明 `kind`，取值只能是 `kernel` / `harness` / `provider` / `ui` / `service`
（边界表见 `plugin-inventory.md` §2）。Harness MUST 按 kind 选择装载、隔离与回收策略；
kernel 插件 MUST NOT 直接读 DB/网络/文件（只能用注入物）；provider 崩溃 MUST NOT 拖垮
Harness 进程；ui 插件 MUST NOT 写入真值。

#### Scenario: provider 崩溃隔离
- **WHEN** 一个 provider 插件进程崩溃
- **THEN** Harness 仍可用，相关工具返回 `unavailable`，会话可继续或按其策略失败，不产生孤儿进程

#### Scenario: kernel 插件越界即失败
- **WHEN** 一个 kernel 插件尝试 import 产品层包或直接打开数据库连接
- **THEN** 架构测试失败，插件不得装载

### Requirement: 装配模型

装配 MUST 由 **base bundle + mode bundle + preset + 用户配置** 四层叠加；覆盖 MUST 按
row id 进行、last write wins；激活 MUST 由服务可用性驱动，MUST NOT 依赖手写的启动顺序。
用户配置（`config.toml`）MUST 可轮转，轮转 MUST 生成新版本并以账本事件序号为锚点生效；
一次 run MUST 固定使用一个版本。

#### Scenario: mode bundle 覆盖 base
- **WHEN** `headless` 装配面不启用 UI 插件而启用同一套工具
- **THEN** 只覆盖对应 row，base 的其余插件不变，启动顺序不需要人工维护

#### Scenario: preset 只改行为
- **WHEN** 一个 preset 关闭 `bash` 并保留 `read`/`write`/`edit`
- **THEN** 该会话的工具清单按 preset 生效，base 与 bundle 定义不被改写

#### Scenario: 配置轮转后新 run 生效
- **WHEN** 用户改写 `config.toml` 并触发轮转
- **THEN** 正在进行中的 run 保持旧版本；下一次 run 使用新版本，并记录锚点事件序号

### Requirement: 能力 seam 可替换

`fs`、`shell`、`subprocess`、`exec-surface`、`terminal` MUST 以 provider seam 暴露，
MUST 能在不改上层工具的前提下替换实现（本地 / 远端 / SSH）。每个 seam MUST 声明能力矩阵；
不支持的组合 MUST 在启动前以类型化错误拒绝，MUST NOT 静默降级。

#### Scenario: 换远端执行面
- **WHEN** 把本地 exec-surface 换成远端适配器
- **THEN** `bash`、`read`、`write`、`edit` 的模型可见契约不变，只有延迟与错误语义变化

#### Scenario: seam 能力不足
- **WHEN** 远端执行面不支持某个文件操作语义
- **THEN** 启动前返回 `unsupported_capability`，不进入运行期才失败

### Requirement: 插件粒度不等于工具粒度

插件 MUST 能只向聚合工具注册 action，MUST NOT 强制为每个能力新增模型可见工具名。
只有安装期显式声明 `provides.tools` 的插件才扩展工具面（见 `tool-surface`）。

#### Scenario: web 搜索不新增核心工具
- **WHEN** 安装一个搜索 provider
- **THEN** 它可以发布一个显式工具（安装期 schema 变更），也可以只作为 `mcp`/`bash` 的后端，不改变内建工具清单

#### Scenario: 一个能力组拆多个插件
- **WHEN** 一个 DSH 能力组（如 shell）在 Keel 拆成 `keel.shell` + `keel.jobs` + `keel.terminal`
- **THEN** 模型看到的仍是 `bash` 与作业动作，插件边界对模型不可见

### Requirement: 插件的扩展面声明

插件清单 MUST 能声明三件事：**提供的槽位**（`provides.slots`）、**投出的贡献**
（`contributes`：目标槽位 + 形态 + 入口）、**可扩展面文档**（`extensibility.json` 机读 +
`EXTENSIBILITY.md` 人读，声明配置项、可挂 hook、可替换 provider、示例与限制）。贡献 MUST 只
针对已声明的槽位与槽位允许的形态；装配期 MUST 校验并 fail-closed。**Harness MUST NOT 内置任何
具体槽位**：槽位由插件声明，机制与形态见 `plugin-ui-extension`。

#### Scenario: 贡献到不存在的槽位
- **WHEN** 一个插件的 `contributes` 指向未声明的槽位或槽位不允许的形态
- **THEN** 装配前拒绝安装并给出字段级原因，界面不出现半成品入口

#### Scenario: 缺可扩展面文档
- **WHEN** 安装一个没有 `extensibility.json` 的插件
- **THEN** 它可用但被标记「不可被创造模式利用」；官方插件发布门禁直接拒绝该包

### Requirement: 可逆副作用与热重载

插件装载 MUST 注册可逆副作用（工具、hook、触发器、UI、服务）；卸载 MUST 反序撤销且无残留。
HMR MUST 只重载插件代码，MUST NOT 重启会话或丢弃进行中的 run；重载失败 MUST 回滚到上一个
generation，并把失败暴露为可查询状态。

#### Scenario: 卸载无残留
- **WHEN** 停用一个提供工具与 hook 的插件
- **THEN** 其工具、hook、触发器、UI 注册全部撤销，模型面与面板不再出现它

#### Scenario: 热重载不打断 run
- **WHEN** 在会话运行中热重载一个非内核插件
- **THEN** 当前 run 继续完成；下一次工具调用使用新版本；失败则回滚并记录

### Requirement: 单一插件运行时

本项目 MUST NOT 引入 Cordis、typert 或第二套 DI/插件运行时；等价语义 MUST 由
`agentext` + `servicegraph` 承担。插件契约 MUST 只有一个真源（本仓的插件清单与生命周期）。

#### Scenario: 新运行时提案被拒
- **WHEN** 一个 change 提议引入外部插件框架来表达依赖注入/可逆副作用
- **THEN** 拒绝，并要求用现有 `agentext`/`servicegraph` 表达

### Requirement: 插件清单与可见性

Harness MUST 提供已安装插件、包清单、generation、依赖状态、健康状态的查询；UI MUST 能
看到每个插件提供的工具/服务/依赖。安装、启用、升级 MUST 走 durable 审批（沿用 v1）。

#### Scenario: 依赖缺失可见
- **WHEN** 一个插件依赖另一个未启用的插件
- **THEN** 它处于 `PENDING` 且原因可见，不静默降级

#### Scenario: 安装即出现
- **WHEN** 安装并通过审批
- **THEN** 清单、工具面（若声明）、面板入口在同一 generation 内可见
