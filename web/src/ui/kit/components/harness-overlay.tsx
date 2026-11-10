// TUI harness · C4 overlays & navigation — Dialog / AlertDialog / Sheet with
// app-owned `open` signals, Popover / Tooltip / DropdownMenu / ContextMenu,
// a Toast viewport fed by a signal list, and the Empty / Breadcrumb surfaces.
import { mount, signal, Text, View } from "nsh";
import { typeScale } from "../tokens";
import {
  AlertDialog,
  Button,
  ContextMenu,
  ContextMenuItem,
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
  Dialog,
  DropdownMenu,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Popover,
  Sheet,
  ToastViewport,
  Tooltip,
} from "../index";

const dialogOpen = signal(false);
const alertOpen = signal(false);
const sheetOpen = signal(false);
const popoverOpen = signal(false);
const menuOpen = signal(false);
const toasts = signal<{ id: string; title: string; description?: string; tone?: "success" | "danger" }[]>([
  { id: "t1", title: "已保存", description: "模型配置", tone: "success" },
]);
const lastAction = signal("未操作");

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", gap: 8, padding: 8 }}>
      <View style={{ flexDirection: "row", gap: 8 }}>
        <Button testID="open-dialog" label="打开对话框" onPress={() => dialogOpen.set(true)} />
        <Button testID="open-alert" label="打开确认框" onPress={() => alertOpen.set(true)} />
        <Button testID="open-sheet" label="打开抽屉" onPress={() => sheetOpen.set(true)} />
      </View>

      <Button
        testID="tooltip-target"
        variant="outline"
        label="带提示的按钮"
        tooltip={<Tooltip label="保存并继续" />}
      />

      {/* A controlled tooltip: the app opens it, so the terminal host shows the
          hint without a pointer to rest on the trigger. */}
      <View
        testID="tooltip-open-target"
        tooltip={<Tooltip label="常显提示" open={true} />}
        style={{ padding: 6, borderWidth: 1 }}
      >
        <Text style={{ fontSize: typeScale.sm }}>常显提示目标</Text>
      </View>

      <Popover
        trigger={<Button testID="popover-trigger" label="气泡" />}
        open={popoverOpen()}
        onOpenChange={(event) => popoverOpen.set(event.open)}
        accessibilityLabel="气泡说明"
      >
        <Text style={{ fontSize: typeScale.sm }}>气泡内容</Text>
        <Button testID="popover-close" variant="ghost" label="关闭气泡" onPress={() => popoverOpen.set(false)} />
      </Popover>

      <DropdownMenu
        trigger={<Button testID="menu-trigger" label="操作菜单" />}
        open={menuOpen()}
        onOpenChange={(event) => menuOpen.set(event.open)}
        accessibilityLabel="操作菜单"
      >
        <DropdownMenuLabel label="会话" />
        <DropdownMenuItem
          testID="menu-item-a"
          label="重命名"
          onSelect={() => lastAction.set("已选 重命名")}
        />
        <DropdownMenuSeparator />
        <DropdownMenuItem
          testID="menu-item-danger"
          label="删除"
          danger
          onSelect={() => lastAction.set("已选 删除")}
        />
      </DropdownMenu>

      <ContextMenu
        trigger={
          <View testID="context-area" style={{ padding: 6, borderWidth: 1 }}>
            <Text style={{ fontSize: typeScale.sm }}>右键区域</Text>
          </View>
        }
        menu={<ContextMenuItem testID="context-item" label="复制路径" onSelect={() => lastAction.set("已选 复制路径")} />}
      />

      <Empty accessibilityLabel="空态" testID="empty-state">
        <EmptyMedia icon="message" tone="neutral" />
        <EmptyTitle label="还没有消息" />
        <EmptyDescription label="发送一条消息，Keel 会在这里开始工作。" />
        <EmptyContent>
          <Button testID="empty-action" label="新建会话" />
        </EmptyContent>
      </Empty>

      <Breadcrumb accessibilityLabel="面包屑">
        <BreadcrumbList>
          <BreadcrumbItem>
            <BreadcrumbLink
              testID="crumb-home"
              label="首页"
              onPress={() => lastAction.set("已选 首页")}
            />
          </BreadcrumbItem>
          <BreadcrumbSeparator />
          <BreadcrumbItem>
            <BreadcrumbPage label="设置" />
          </BreadcrumbItem>
        </BreadcrumbList>
      </Breadcrumb>

      <Text testID="overlay-readout" style={{ fontSize: typeScale.sm }}>
        {lastAction()}
      </Text>
      <Text testID="toast-readout" style={{ fontSize: typeScale.sm }}>
        {`提示数 ${toasts().length}`}
      </Text>

      <Dialog
        open={dialogOpen()}
        testID="dialog-plan"
        title="重命名会话"
        description="改名后列表会立即更新"
        onOpenChange={(event) => {
          if (!event.open) dialogOpen.set(false);
        }}
        footer={<Button testID="dialog-cancel" variant="outline" label="取消" onPress={() => dialogOpen.set(false)} />}
      >
        <Text style={{ fontSize: typeScale.sm }}>对话框内容</Text>
      </Dialog>

      <AlertDialog
        open={alertOpen()}
        testID="alert-delete"
        title="删除这个会话？"
        description="删除后无法恢复"
        confirmLabel="删除"
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          lastAction.set("已确认删除");
          alertOpen.set(false);
        }}
        onCancel={() => alertOpen.set(false)}
      />

      <Sheet
        open={sheetOpen()}
        testID="sheet-filters"
        side="right"
        title="筛选"
        onOpenChange={(event) => {
          if (!event.open) sheetOpen.set(false);
        }}
      >
        <Text style={{ fontSize: typeScale.sm }}>抽屉内容</Text>
      </Sheet>

      <ToastViewport
        testID="toast-viewport"
        toasts={toasts()}
        onDismiss={(id) => toasts.set(toasts().filter((item) => item.id !== id))}
      />
    </View>
  );
}

mount(Harness);
