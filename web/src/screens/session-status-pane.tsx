// The workspace's right pane: the open session's running facts (change
// keel-harness-shell, C6 — the pane that `web/App.tsx` used to stand in for
// with a bare status capsule).
//
// What it shows, and where each fact comes from:
//
//   · the run state       `conversationState(store.conversation())` — the same
//                         vocabulary the header's badge uses, one word;
//   · the model           the session's own selection, else the tenant default
//                         (`selectionDisplay`: the display name, never the raw
//                         wire identity);
//   · the reasoning level that selection's level, in the pickers' words;
//   · the usage           the open view's run summaries added up;
//   · the tool calls      every work segment's tool calls, counted;
//   · the plugins         the assembled extension surface, once it is loaded
//                         (the pane asks for it when it opens).
//
// A row without a fact is not drawn: the pane never prints a placeholder for
// something it cannot read. With no session open it is an honest empty state,
// never an empty panel.
//
// NSH components run once: every reactive read sits in a getter the JSX calls
// (or in an `<If>`), never in a body-level local.
import { Clipboard, cleanup, effect, Text, View } from "nsh";
import { store } from "../data/store";
import { formatTokens, reasoningLabel, selectionDisplay } from "../data/wire";
import {
  Alert,
  Badge,
  Button,
  IconButton,
  ScrollArea,
  Skeleton,
  color,
  font,
  lineHeight,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";
import {
  conversationState,
  queueDetail,
  runStateLabel,
  runStateTone,
  type RunState,
  type RunTone,
} from "../ui/run-state";
import { exportAvailable, exportSummary, sessionQueryAccess } from "../data/session-query";
import {
  deliverablesAccess,
  deliverableSize,
  shortDeliverableHash,
  type DeliverablePreview,
} from "../data/deliverables";
import type { DeliverableView } from "../data/contract";

const stateBadge: Record<RunTone, BadgeVariant> = {
  neutral: "secondary",
  brand: "info",
  warning: "warning",
  danger: "destructive",
};

/** The header's state: a 202 queue admission wins over the conversation
 * projection, which has no queue fact and would otherwise say 就绪 while the
 * run waits in line. */
const headerState = (): RunState => (store.activeQueue() ? "queued" : conversationState(store.conversation()));
const headerStateLabel = (): string => {
  const queue = store.activeQueue();
  return queue ? queueDetail(queue) : runStateLabel[headerState()];
};

/** The connection in the reader's words. */
const connectionLabel = (status: string): string => {
  switch (status) {
    case "open":
      return "服务已连接";
    case "connecting":
      return "正在连接";
    case "reconnecting":
      return "连接中断，正在续接";
    default:
      return "未连接";
  }
};

/** One fact: its name above its value. The name is the quiet line, but it is
 * still read at 1× on a wide screen — 12px is the UI floor (10px CJK ink
 * smears), while the value keeps its 1.5 line height because it wraps. */
function StatusFact(props: { label: string; value: string }) {
  return (
    <View style={{ gap: space.xxs }}>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.label}</Text>
      <Text
        style={{
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.normal),
          color: color.foreground,
          userSelect: "text",
        }}
      >
        {props.value}
      </Text>
    </View>
  );
}

/** The open session's model: the session's own input fact wins over the tenant
 * default, the way the header's chip reads it. */
const effectiveSelection = () => store.sessionSelection() ?? store.tenantSelection();

/** Tokens in/out, added across the open view's runs; `null` while no run has
 * reported usage. */
const usageTotals = () => {
  const turns = store.conversation()?.turns ?? [];
  let tokensIn = 0;
  let tokensOut = 0;
  let runs = 0;
  for (const turn of turns) {
    const summary = turn.runSummary;
    if (!summary) continue;
    tokensIn += summary.tokensIn;
    tokensOut += summary.tokensOut;
    runs += 1;
  }
  return runs > 0 ? { tokensIn, tokensOut } : null;
};

/** Tool calls across every work segment of the open view. */
const toolCallCount = () => {
  const turns = store.conversation()?.turns ?? [];
  let count = 0;
  for (const turn of turns) {
    for (const segment of turn.workSegments) count += segment.toolCalls.length;
  }
  return count;
};

/** One manifest row: the path, the size/hash the manifest records, and a press
 * that opens the row's bounded preview. The preview link is the server's own
 * `/file-preview` URL (the row carries it), never a client-composed path. */
function DeliverableRow(props: { row: DeliverableView; onOpen: () => void }) {
  const row = () => props.row;
  return (
    <View
      testID={`deliverable-row-${row().path}`}
      role="button"
      focusable
      onPress={props.onOpen}
      accessibilityLabel={`预览交付物 ${row().path}`}
      hoverStyle={{ backgroundColor: color.hover }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        minHeight: 36,
        paddingVertical: space.xxs,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <Text
        style={{
          flex: 1,
          minWidth: 0,
          fontFamily: font.mono,
          fontSize: typeScale.key,
          color: color.foreground,
          numberOfLines: 1,
          textOverflow: "ellipsis",
        }}
      >
        {row().path}
      </Text>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{deliverableSize(row().size)}</Text>
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
        {shortDeliverableHash(row().hash)}
      </Text>
    </View>
  );
}

/** The open preview: the manifest's current bytes, whether they still match the
 * row's hash, and the server's preview link (copyable). */
function DeliverablePreviewCard(props: { preview: DeliverablePreview; onClose: () => void }) {
  const preview = () => props.preview;
  const matchLabel = () =>
    preview().matches === null ? "未能核对引用" : preview().matches ? "与引用一致" : "文件已变更，显示当前内容";
  return (
    <View
      testID="deliverable-preview"
      style={{
        flexDirection: "column",
        gap: space.xs,
        padding: space.md,
        borderRadius: 8,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.card,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {preview().row.path}
        </Text>
        <Button variant="ghost" size="sm" label="关闭" accessibilityLabel="关闭交付物预览" onPress={props.onClose} />
      </View>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        {deliverableSize(preview().view.size)} · {shortDeliverableHash(preview().view.hash)} · {matchLabel()}
      </Text>
      <Text
        testID="deliverable-preview-content"
        style={{
          fontFamily: font.mono,
          fontSize: typeScale.key,
          lineHeight: Math.round(typeScale.key * lineHeight.normal),
          color: color.foreground,
          userSelect: "text",
          numberOfLines: 40,
          textOverflow: "ellipsis",
        }}
      >
        {preview().view.content}
      </Text>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Button
          testID="deliverable-copy-link"
          variant="outline"
          size="sm"
          icon="copy"
          label="复制链接"
          onPress={() => {
            void Clipboard.writeText(preview().row.previewUrl).then(
              () => store.flash({ tone: "success", text: "已复制交付物链接。" }),
              () => store.flash({ tone: "danger", text: "这个平台不能写入剪贴板。" }),
            );
          }}
        />
        <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale["2xs"], color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {preview().row.previewUrl}
        </Text>
      </View>
    </View>
  );
}

/** The workspace-changes / deliverables section: the session manifest's rows,
 * five states, and one bounded preview at a time. */
function DeliverablesSection() {
  const rows = () => deliverablesAccess.rows();
  const phase = () => deliverablesAccess.phase();
  return (
    <View testID="deliverables-section" style={{ flexDirection: "column", gap: space.sm }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Text style={{ flex: 1, fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
          交付物 · 工作区变更
        </Text>
        <If when={rows().length > 0}>
          <Text testID="deliverables-count" style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
            {rows().length} 个文件
          </Text>
        </If>
        <Button
          testID="deliverables-refresh"
          variant="ghost"
          size="sm"
          icon="refresh"
          label="刷新交付物"
          accessibilityLabel="刷新交付物"
          onPress={() => {
            const sessionId = store.activeId();
            if (sessionId) void deliverablesAccess.refresh(sessionId);
          }}
        />
      </View>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        这些是会话文件清单里的写入；路径、大小与哈希都来自清单本身。
      </Text>
      <If when={phase() === "loading"}>
        <Skeleton testID="deliverables-loading" lines={3} accessibilityLabel="正在加载交付物" />
      </If>
      <If when={phase() === "permission"}>
        <Alert
          testID="deliverables-permission"
          variant="destructive"
          icon="lock"
          title="权限不足"
          description={deliverablesAccess.error()?.message ?? ""}
        />
      </If>
      <If when={phase() === "error"}>
        <Alert
          testID="deliverables-error"
          variant="destructive"
          icon="alert"
          title="交付物读取失败"
          description={deliverablesAccess.error()?.message ?? ""}
          action={
            <Button
              variant="outline"
              size="sm"
              label="重试"
              onPress={() => {
                const sessionId = store.activeId();
                if (sessionId) void deliverablesAccess.refresh(sessionId);
              }}
            />
          }
        />
      </If>
      <If when={phase() === "empty"}>
        <Text testID="deliverables-empty" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          这个会话还没有交付物。
        </Text>
      </If>
      <If when={phase() === "success"}>
        <View testID="deliverables-list" style={{ flexDirection: "column" }}>
          <Each of={rows()} key={(row) => row.path}>
            {(row) => <DeliverableRow row={row()} onOpen={() => void deliverablesAccess.openPreview(row())} />}
          </Each>
        </View>
      </If>
      <If when={deliverablesAccess.hasMore()}>
        <Button
          testID="deliverables-more"
          variant="outline"
          size="sm"
          label="继续加载"
          onPress={() => void deliverablesAccess.more()}
        />
      </If>
      <If when={deliverablesAccess.previewLoading() !== null}>
        <Text testID="deliverable-preview-loading" style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          正在读取文件预览…
        </Text>
      </If>
      <If when={deliverablesAccess.previewError() !== null}>
        <Alert
          testID="deliverable-preview-error"
          variant="destructive"
          icon="alert"
          title="预览失败"
          description={deliverablesAccess.previewError()?.message ?? ""}
        />
      </If>
      <If when={deliverablesAccess.preview() !== null}>
        <DeliverablePreviewCard
          preview={deliverablesAccess.preview()!}
          onClose={() => deliverablesAccess.closePreview()}
        />
      </If>
    </View>
  );
}

export function SessionStatusPane() {
  // The plugin surface is part of this pane's facts; ask for it when the pane
  // opens (the store's own guard keeps this to one request while it is in
  // flight, and the settings page shares the same load).
  effect(() => {
    if (store.mode() !== "http") return;
    const timer = setTimeout(() => void store.loadPlugins(), 0);
    cleanup(() => clearTimeout(timer));
  });
  // The workspace-changes projection follows the open session. The read is a
  // bounded first page; the section's 继续加载 asks for the next one.
  effect(() => {
    const sessionId = store.activeId();
    const mode = store.mode();
    if (mode !== "http" || !sessionId || !deliverablesAccess.available()) return;
    const timer = setTimeout(() => deliverablesAccess.ensure(sessionId), 0);
    cleanup(() => clearTimeout(timer));
  });

  return (
    <View
      testID="session-status-pane"
      style={{ flex: 1, minHeight: 0, flexDirection: "column" }}
    >
      <View
        style={{
          flexShrink: 0,
          gap: space.xs,
          paddingHorizontal: space.xl,
          paddingVertical: space.lg,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <Text
            style={{
              flex: 1,
              fontSize: typeScale.base,
              fontWeight: weight.semibold,
              color: color.foreground,
            }}
          >
            会话状态
          </Text>
          <If when={!!store.activeId() && exportAvailable()}>
            <IconButton
              testID="session-export"
              icon="download"
              label="导出会话"
              onPress={async () => {
                const sessionId = store.activeId();
                if (!sessionId) return;
                try {
                  const { view, jsonl } = await sessionQueryAccess.exportSession(sessionId);
                  if (jsonl) await Clipboard.writeText(jsonl);
                  store.flash({ tone: view.messages?.length ? "success" : "info", text: exportSummary(view) });
                } catch (error) {
                  const message = error instanceof Error ? error.message : String(error);
                  store.flash({ tone: "danger", text: `导出失败：${message}` });
                }
              }}
            />
          </If>
          <If when={!!store.activeId()}>
            <Badge
              testID="session-status-state"
              variant={stateBadge[runStateTone[headerState()]]}
              label={headerStateLabel()}
            />
          </If>
        </View>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          这些事实来自当前会话的运行记录。
        </Text>
      </View>

      <ScrollArea
        style={{ flex: 1, minHeight: 0 }}
        contentStyle={{ flexDirection: "column", padding: space.xl, gap: space.xl }}
        accessibilityLabel="会话状态详情"
      >
        <If
          when={!!store.activeId()}
          else={
            <View style={{ gap: space.xs }}>
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
                还没有打开会话
              </Text>
              <Text
                style={{
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.mutedForeground,
                }}
              >
                打开一个会话后，这里显示它的模型、用量与工具调用。
              </Text>
            </View>
          }
        >
          <If when={!!effectiveSelection()}>
            <StatusFact label="模型" value={selectionDisplay(effectiveSelection())} />
          </If>
          <If when={!!effectiveSelection()?.reasoningLevel}>
            <StatusFact
              label="思考档"
              value={reasoningLabel(effectiveSelection()?.reasoningLevel)}
            />
          </If>
          <StatusFact
            label="连接"
            value={connectionLabel(store.connection().status)}
          />
          <If when={usageTotals() !== null}>
            <StatusFact
              label="用量"
              value={`输入 ${formatTokens(usageTotals()?.tokensIn ?? 0)} · 输出 ${formatTokens(
                usageTotals()?.tokensOut ?? 0,
              )}`}
            />
          </If>
          <If when={toolCallCount() > 0}>
            <StatusFact label="工具调用" value={`${toolCallCount()} 次`} />
          </If>
          <If when={!!store.plugins()}>
            <StatusFact
              label="插件"
              value={`${(store.plugins()?.plugins ?? []).length} 个插件 · ${
                (store.plugins()?.tools ?? []).length
              } 个工具`}
            />
          </If>
        </If>
        <If when={!!store.activeId() && deliverablesAccess.available()}>
          <DeliverablesSection />
        </If>
      </ScrollArea>
    </View>
  );
}
