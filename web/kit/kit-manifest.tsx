// The gallery's manifest: one entry per item of the kit spec (proposal §1's
// component list and §2's layout blocks), the group it is rendered in, and the
// demo each state passes.
//
// Two consumers read this file:
//   - KitGallery.tsx renders exactly these entries;
//   - web/kit/gen-registry.mjs parses the `name: "<Name>"` and `aliases: [...]`
//     lines here as the coverage list (keep one entry per block, and do not
//     rename those two keys). A name with no export in web/src/ui/kit renders
//     as a "missing: <name>" card and fails the gate.
//
// Recipes follow the kit's own shapes — shadcn/ui composition, never invented
// data props: composite pieces get their sub-components through `compose`, which
// KitGallery resolves against the same generated registry (a sub-component that
// has not landed renders as "missing child: <Name>" instead of breaking the
// build). The kit's `harness-*.tsx` files are the usage reference.
import { Text, View } from "nsh";

export type KitGroupId = "base" | "form" | "container" | "overlay" | "layout" | "block";

export type KitStateId = "default" | "disabled" | "error" | "empty";

export type KitState = { id: KitStateId; props?: Record<string, unknown> };

/** A component the gallery renders dynamically (see the generated registry). */
export type KitNode = (props: any) => any;

/** Sub-components for a composite demo, resolved by name at render time. */
export type KitCompose = (need: (name: string) => KitNode) => Record<string, unknown>;

export type KitEntry = {
  /** The export the gallery looks for in web/src/ui/kit. */
  name: string;
  /** Other exports that satisfy this line (e.g. the compound group of Resizable). */
  aliases?: string[];
  group: KitGroupId;
  /** Props every state shares (the demo content). */
  base?: Record<string, unknown>;
  /** Composition for composite pieces: the sub-components as props. */
  compose?: KitCompose;
  /** A full-row card: the layout blocks decide by their own width, so a narrow
   * card would hide their real (wide) form behind an auto-collapse. */
  wide?: boolean;
  /** The states rendered for this entry; the default is default/disabled/error. */
  states?: KitState[];
  /** One line under the title: what this entry is for. */
  note?: string;
};

export const KIT_GROUPS: ReadonlyArray<{ id: KitGroupId; label: string }> = [
  { id: "base", label: "C1 · 基础件" },
  { id: "form", label: "C2 · 表单件" },
  { id: "container", label: "C3 · 容器件" },
  { id: "overlay", label: "C4 · 浮层与导航" },
  { id: "layout", label: "C5 · 布局原语" },
  { id: "block", label: "L · 布局块（zcode 参数）" },
];

const text = (value: string) => <Text>{` ${value} `}</Text>;

/** default / disabled / error — the three states every kit component owes. */
const TRIO: KitState[] = [
  { id: "default" },
  { id: "disabled", props: { disabled: true } },
  { id: "error", props: { error: "示例错误", invalid: true } },
];

/** Display pieces have no disabled state: the second cell is empty. */
const defaultThenEmpty = (empty: Record<string, unknown>): KitState[] => [
  { id: "default" },
  { id: "empty", props: empty },
];

/** Window-layer pieces (NSH presents them over the whole window): the gallery
 * keeps them closed and still registers their three state cells. */
const closedStates = (extra: Record<string, unknown>): KitState[] => [
  { id: "default" },
  { id: "disabled", props: { open: false, ...extra } },
  { id: "error", props: { open: false, ...extra } },
];

export const KIT_MANIFEST: KitEntry[] = [
  // C1 · 基础件
  {
    name: "Button",
    group: "base",
    base: { label: "按钮" },
    states: TRIO,
  },
  {
    name: "Badge",
    group: "base",
    base: { label: "徽标" },
    states: [
      { id: "default" },
      { id: "disabled", props: { onPress: () => {}, disabled: true } },
      { id: "empty", props: { label: "", children: null } },
    ],
  },
  {
    name: "Separator",
    group: "base",
    base: { label: "分隔" },
    states: defaultThenEmpty({ label: "", children: null }),
  },
  {
    name: "Kbd",
    group: "base",
    base: { keys: ["⌘", "K"] },
    states: defaultThenEmpty({ keys: [] }),
  },
  {
    name: "Spinner",
    group: "base",
    base: { size: 14 },
    states: defaultThenEmpty({ size: 8 }),
  },
  {
    name: "Avatar",
    group: "base",
    base: { name: "Keel" },
    states: defaultThenEmpty({ name: "" }),
  },
  {
    name: "Skeleton",
    group: "base",
    base: { width: 160, height: 14, lines: 2 },
    states: defaultThenEmpty({ width: 0, height: 0 }),
  },
  {
    name: "Progress",
    group: "base",
    base: { value: 0.4 },
    states: [
      { id: "default" },
      { id: "error", props: { value: 1, tone: "danger" } },
      { id: "empty", props: { value: 0 } },
    ],
    note: "proposal §1 列了 Progress，tasks C1–C5 没列它：kit 现在没有这个导出（缺件由门禁暴露，等裁决）",
  },

  // C2 · 表单件
  {
    name: "Input",
    group: "form",
    base: { placeholder: "示例输入", defaultValue: "示例" },
    states: TRIO,
  },
  {
    name: "Textarea",
    group: "form",
    base: { placeholder: "示例文本", defaultValue: "示例文本", rows: 3 },
    states: TRIO,
  },
  {
    name: "Label",
    group: "form",
    base: { label: "示例标签", required: true },
    states: TRIO,
  },
  {
    name: "Field",
    group: "form",
    base: { label: "字段", hint: "辅助说明", children: text("示例控件") },
    states: [
      { id: "default" },
      { id: "disabled", props: { disabled: true } },
      { id: "error", props: { error: "必填字段" } },
    ],
  },
  {
    name: "Switch",
    group: "form",
    base: { label: "开关" },
    states: TRIO,
  },
  {
    name: "Checkbox",
    group: "form",
    base: { label: "复选" },
    states: TRIO,
  },
  {
    name: "RadioGroup",
    group: "form",
    base: {
      options: [{ value: "a", label: "选项 A" }, { value: "b", label: "选项 B" }],
      value: "a",
      accessibilityLabel: "选项",
    },
    states: TRIO,
  },
  {
    name: "Select",
    group: "form",
    base: {
      items: [{ value: "a", title: "选项 A" }, { value: "b", title: "选项 B" }],
      defaultValue: "a",
      placeholder: "请选择",
    },
    states: TRIO,
    note: "shadcn 形态：items 是数据，弹层由 nsh/base 呈现",
  },
  {
    name: "Command",
    group: "form",
    base: {
      items: [{ id: "open", label: "打开" }, { id: "close", label: "关闭" }],
      placeholder: "输入命令…",
    },
    states: TRIO,
  },
  {
    name: "Combobox",
    group: "form",
    base: {
      items: [{ value: "a", title: "选项 A" }, { value: "b", title: "选项 B" }],
      value: ["a"],
      multiple: true,
      placeholder: "搜索…",
    },
    states: TRIO,
  },

  // C3 · 容器件
  {
    name: "Card",
    group: "container",
    compose: (need) => {
      const Header = need("CardHeader");
      const Content = need("CardContent");
      const Footer = need("CardFooter");
      const Button = need("Button");
      return {
        children: (
          <>
            <Header title="卡片标题" description="说明行" />
            <Content>{text("卡片内容")}</Content>
            <Footer>
              <Button label="确认" />
            </Footer>
          </>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
  },
  {
    name: "ScrollArea",
    group: "container",
    base: { maxHeight: 96, children: text("滚动内容") },
    states: defaultThenEmpty({ children: null }),
  },
  {
    name: "Collapsible",
    group: "container",
    base: { open: true, content: text("折叠内容") },
    compose: (need) => {
      const Trigger = need("CollapsibleTrigger");
      return { children: <Trigger label="折叠标题" expanded={true} /> };
    },
    states: [
      { id: "default" },
      { id: "disabled", props: { open: true } },
      { id: "empty", props: { open: false } },
    ],
    note: "empty = 收起态（关闭时内容不占高）；静态 open=true 起始时 native Collapsible 不渲染 content（0→1 展开路径由 kit 单测覆盖）",
  },
  {
    name: "Accordion",
    group: "container",
    base: { value: ["a"], multiple: true },
    compose: (need) => {
      const Item = need("AccordionItem");
      return {
        children: (
          <>
            <Item value="a" title="问题 A">
              {text("答案 A")}
            </Item>
            <Item value="b" title="问题 B">
              {text("答案 B")}
            </Item>
          </>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
  },
  {
    name: "Tabs",
    group: "container",
    base: { accessibilityLabel: "标签" },
    compose: (need) => {
      const Tab = need("Tab");
      const Content = need("TabsContent");
      return {
        children: (
          <View>
            <>
              <Tab label="标签 A" selected={true} />
              <Tab label="标签 B" />
              <Content value="a" current="a">
                {text("标签内容")}
              </Content>
            </>
          </View>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
    note: "Tabs 是 tab 列表，TabsContent 是面板（shadcn 形态）",
  },
  {
    name: "Table",
    group: "container",
    compose: (need) => {
      const Table = need("Table");
      const HeaderRow = need("TableHeaderRow");
      const Th = need("Th");
      const Body = need("TableBody");
      const BodyRow = need("TableBodyRow");
      const Td = need("Td");
      return {
        children: (
          <Table accessibilityLabel="运行记录">
            <>
              <HeaderRow>
                <>
                  <Th>{text("会话")}</Th>
                  <Th>{text("状态")}</Th>
                </>
              </HeaderRow>
              <Body>
                <BodyRow>
                  <>
                    <Td>{text("发布周报")}</Td>
                    <Td>{text("完成")}</Td>
                  </>
                </BodyRow>
              </Body>
            </>
          </Table>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
    note: "tasks 把它放在 C3 容器组",
  },

  // C4 · 浮层与导航
  {
    name: "Dialog",
    group: "overlay",
    base: { open: false, title: "对话框", description: "说明文字", children: text("对话框内容") },
    states: closedStates({}),
    note: "窗口层浮层（NSH dialog）：gallery 只渲染关闭态，自动打开会遮住整页",
  },
  {
    name: "AlertDialog",
    group: "overlay",
    base: {
      open: false,
      title: "删除这个会话？",
      description: "删除后无法恢复",
      confirmLabel: "删除",
      cancelLabel: "取消",
      destructive: true,
    },
    states: closedStates({}),
    note: "窗口层浮层：gallery 只渲染关闭态",
  },
  {
    name: "Sheet",
    group: "overlay",
    base: { open: false, side: "right", title: "筛选", children: text("抽屉内容") },
    states: closedStates({}),
    note: "窗口层浮层：gallery 只渲染关闭态",
  },
  {
    name: "Popover",
    group: "overlay",
    base: { trigger: text("气泡触发"), children: text("气泡内容") },
    states: defaultThenEmpty({ children: null }),
    note: "组合形态：trigger + 内容子元素",
  },
  {
    name: "Tooltip",
    group: "overlay",
    base: { label: "保存并继续" },
    states: [
      { id: "default" },
      { id: "disabled", props: { disabled: true } },
      { id: "empty", props: { label: "", children: text("") } },
    ],
    note: "内容走 label / 子元素（不是 content）；浮层默认隐藏，gallery 不自动 open（会浮到别的卡片上）",
  },
  {
    name: "DropdownMenu",
    group: "overlay",
    base: { trigger: text("操作菜单") },
    compose: (need) => {
      const Label = need("DropdownMenuLabel");
      const Item = need("DropdownMenuItem");
      const Separator = need("DropdownMenuSeparator");
      return {
        children: (
          <>
            <Label label="会话" />
            <Item label="重命名" />
            <Separator />
            <Item label="删除" danger />
          </>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
    note: "组合形态：trigger + Item / Label / Separator",
  },
  {
    name: "ContextMenu",
    group: "overlay",
    base: { trigger: text("右键区域") },
    compose: (need) => {
      const Item = need("ContextMenuItem");
      return { menu: <Item label="复制路径" /> };
    },
    states: defaultThenEmpty({ menu: null }),
    note: "组合形态：trigger + menu 子元素",
  },
  {
    name: "Toast",
    group: "overlay",
    base: { title: "已保存", description: "模型配置", tone: "success" },
    states: [
      { id: "default" },
      { id: "error", props: { tone: "danger", title: "保存失败", description: "网络不可达" } },
      { id: "empty", props: { title: "", description: "", children: null } },
    ],
    note: "内容走 title / description（不是 message）",
  },
  {
    name: "Empty",
    group: "overlay",
    compose: (need) => {
      const Media = need("EmptyMedia");
      const Title = need("EmptyTitle");
      const Description = need("EmptyDescription");
      const Content = need("EmptyContent");
      const Button = need("Button");
      return {
        children: (
          <>
            <Media icon="message" tone="neutral" />
            <Title label="还没有消息" />
            <Description label="发送一条消息，Keel 会在这里开始工作。" />
            <Content>
              <Button label="新建会话" />
            </Content>
          </>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
  },
  {
    name: "Breadcrumb",
    group: "overlay",
    compose: (need) => {
      const List = need("BreadcrumbList");
      const Item = need("BreadcrumbItem");
      const Link = need("BreadcrumbLink");
      const Page = need("BreadcrumbPage");
      const Separator = need("BreadcrumbSeparator");
      return {
        children: (
          <List>
            <>
              <Item>
                <Link label="首页" />
              </Item>
              <Separator />
              <Item>
                <Page label="设置" />
              </Item>
            </>
          </List>
        ),
      };
    },
    states: defaultThenEmpty({ children: null }),
    note: "层级组合：List / Item / Link / Separator / Page",
  },

  // C5 · 布局原语
  {
    name: "Resizable",
    aliases: ["ResizablePanelGroup"],
    group: "layout",
    base: { axis: "horizontal", handleAccessibilityLabel: "调整面板宽度" },
    compose: (need) => {
      const Panel = need("ResizablePanel");
      return {
        children: (
          <>
            <Panel size={140}>{text("左面板")}</Panel>
            <Panel>{text("右面板")}</Panel>
          </>
        ),
      };
    },
    states: [
      { id: "default" },
      { id: "disabled", props: { disabled: true } },
      { id: "empty", props: { children: null } },
    ],
    note: "shadcn 形态：PanelGroup + Panel（handle 4px band / 16px 键盘步长在组件内）",
  },
  {
    name: "Sidebar",
    group: "layout",
    base: { collapsed: false, children: text("侧栏内容") },
    states: [
      { id: "default" },
      { id: "empty", props: { collapsed: true, children: null } },
      { id: "error", props: { collapsed: false, children: null } },
    ],
    note: "empty = 折叠成 4px 条（264 / 50% / 16px 步长在组件内）",
  },
  {
    name: "SidePane",
    group: "layout",
    base: { open: true, children: text("右面板 45%") },
    states: [
      { id: "default" },
      { id: "empty", props: { open: false, children: null } },
      { id: "error", props: { open: true, children: null } },
    ],
    note: "45% / min 240 / max 65%；独立渲染时宽度按窗口折算",
  },

  // L · 布局块
  {
    name: "WorkspaceShell",
    wide: true,
    group: "block",
    base: {
      header: text("标题栏 h-12"),
      sidebar: text("侧栏 264"),
      main: text("主区 min 320"),
      sidePane: text("右栏 45%"),
      defaultSidePaneOpen: true,
    },
    states: [
      { id: "default" },
      { id: "empty", props: { sidebar: null, sidePane: null } },
      { id: "error", props: { defaultSidebarCollapsed: true } },
    ],
    note: "参数在块内（264/50%/16px/4px 折叠/767 断点/自动折叠阈值），gallery 不传壳宽",
  },
  {
    name: "SettingsShell",
    wide: true,
    group: "block",
    base: {
      sections: [
        { id: "general", label: "通用", icon: "settings" },
        { id: "model", label: "模型", icon: "spark" },
      ],
      breadcrumb: ["设置", "通用"],
      children: text("内容列 max-w-4xl"),
    },
    states: [
      { id: "default" },
      { id: "empty", props: { children: null } },
      { id: "error", props: { section: "model" } },
    ],
    note: "rail 68 → lg 268，内容卡 + h-12 面包屑，分区记忆在块内",
  },
  {
    name: "ConversationColumn",
    group: "block",
    base: { children: text("对话列（2xl / 4xl / 6xl 档）") },
    states: [
      { id: "default" },
      { id: "empty", props: { draft: true } },
      { id: "error", props: { children: null } },
    ],
    note: "empty = 草稿/空态（max-w-2xl，无左移）",
  },
  {
    name: "ConversationStatusRail",
    group: "block",
    base: { variant: "inline", children: text("状态栏（≥1280 宽 320）") },
    states: [
      { id: "default" },
      { id: "empty", props: { variant: "mini", label: "状态", children: null } },
      { id: "error", props: { variant: "inline", children: null } },
    ],
  },
  {
    name: "ComposerDock",
    group: "block",
    base: { children: text("输入区（sticky dock / rounded-2xl / p-3 / focus ring）") },
    states: [
      { id: "default" },
      { id: "empty", props: { draft: true } },
      { id: "error", props: { children: null } },
    ],
  },
];

/** Every public name the gate expects (the entry's own name plus its aliases). */
export const KIT_NAMES: string[] = KIT_MANIFEST.flatMap((entry) => [entry.name, ...(entry.aliases ?? [])]);
