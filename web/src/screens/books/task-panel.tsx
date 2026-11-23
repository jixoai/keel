// The task panel — change keel-harness-v2, W4.
//
// 任务图 / 事件流 / 资源预览 as one read-only projection of the run surface. The
// panel hangs off the run page beside its inspector: it is a section of the
// run's own paper, not a sixth chapter, and it adds nothing to the top band
// (A4.4f). Its facts come from `GET /v1/sessions/{id}/task-panel` and, for one
// opened reference, `GET /v1/sessions/{id}/file-preview`; the fold lives in
// `data/tasks.ts`.
//
// What it never does: write task state, invent a card, invent a path, or claim
// a resource matches. A reference the file layer no longer holds says 文件已变化;
// an event kind this build does not know is printed with its kind and sequence.
import { Text, View } from "nsh";
import {
  Badge,
  Button,
  IconButton,
  ScrollArea,
  Sheet,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../ui/kit";
import { Icon } from "../../ui/icons";
import {
  cardOriginLabel,
  cardStatusLabel,
  cardTone,
  eventKindLabel,
  eventLines,
  fileReferences,
  forkBranches,
  panelCounts,
  panelGaps,
  panelPhase,
  previewSummary,
  resourceChanged,
  resourceCount,
  shortHash,
  taskPanelAccess,
  taskTree,
  type TaskEventLine,
  type TaskPanelPhase,
  type TaskTreeRow,
} from "../../data/tasks";
import { capabilityOf } from "../../data/permission-catalog";
import { formatWhen, Hairline, PageEmpty, PageError, PageLoading, PagePermission, StateNote, isMobile, touchStyle } from "./common";

const STATUS_VARIANT: Record<ReturnType<typeof cardTone>, BadgeVariant> = {
  info: "info",
  success: "success",
  destructive: "destructive",
  secondary: "secondary",
  outline: "outline",
};

const cardBadge = (status: string | undefined) => (
  <Badge variant={STATUS_VARIANT[cardTone(status)]} label={cardStatusLabel(status)} />
);

const SHORT_ID = (id: string | undefined): string => (id ? id.slice(0, 8) : "—");

/** The section switch: 图 / 事件 / 资源 — the same Segmented grammar the run
 * page uses for its own views. */
function PanelSegmented() {
  const items: Array<{ id: "graph" | "events" | "resources"; label: string }> = [
    { id: "graph", label: "任务图" },
    { id: "events", label: "事件流" },
    { id: "resources", label: "资源预览" },
  ];
  const counts = () => {
    const panel = taskPanelAccess.panel();
    return panel ? panelCounts(panel) : { cards: 0, members: 0, attachments: 0, events: 0 };
  };
  // The resources section counts what it prints: live attachments plus the
  // manifest's own attributed rows (a manifest-only file is a resource too).
  const resources = () => {
    const panel = taskPanelAccess.panel();
    return panel ? resourceCount(panel, taskPanelAccess.files()) : 0;
  };
  return (
    <View
      testID="task-panel-section"
      accessibilityLabel="任务面板视图"
      style={{
        flexDirection: "row",
        gap: 2,
        padding: 2,
        borderRadius: radius.sm,
        backgroundColor: color.muted,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <Each of={items} key={(item) => item.id}>
        {(item) => {
          const active = () => taskPanelAccess.section() === item().id;
          const count = () =>
            item().id === "events"
              ? counts().events
              : item().id === "resources"
                ? resources()
                : counts().cards + counts().members;
          return (
            <View
              testID={`task-panel-section-${item().id}`}
              role="button"
              focusable
              onPress={() => taskPanelAccess.setSection(item().id)}
              accessibilityLabel={`${item().label}，${count()} 条`}
              accessibilityState={{ selected: active() }}
              hoverStyle={active() ? undefined : { backgroundColor: color.hover }}
              style={{
                flex: 1,
                minWidth: 0,
                minHeight: isMobile() ? 44 : 28,
                alignItems: "center",
                justifyContent: "center",
                borderRadius: radius.xs,
                backgroundColor: active() ? color.selected : "transparent",
              }}
            >
              <Text style={{ fontSize: typeScale.sm, color: active() ? color.foreground : color.mutedForeground, numberOfLines: 1 }}>
                {item().label}
              </Text>
            </View>
          );
        }}
      </Each>
    </View>
  );
}

/** The panel's head: what it is, the counts and the refresh. */
function PanelHead(props: { sessionId: string }) {
  const panel = () => taskPanelAccess.panel();
  const counts = () => (panel() ? panelCounts(panel()!) : null);
  const loading = () => taskPanelAccess.loading();
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minWidth: 0 }}>
        <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          任务面板
        </Text>
        <If when={counts() !== null}>
          <Text testID="task-panel-counts" style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
            {counts()!.cards} 卡 · {counts()!.members} 成员 · {counts()!.events} 事件
          </Text>
        </If>
        <IconButton
          testID="task-panel-refresh"
          icon="refresh"
          label="刷新任务面板"
          disabled={loading()}
          onPress={() => void taskPanelAccess.refresh(props.sessionId)}
        />
      </View>
      <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
        任务卡、事件流与资源引用都是这次会话账本的投影；面板只读，不写任务状态。
      </Text>
    </View>
  );
}

/** The five states. `loading`/`empty`/`error`/`permission` are the panel's own
 * faces; `success` renders the sections below. */
export function TaskPanelBody(props: { sessionId: string }) {
  const panel = () => taskPanelAccess.panel();
  const error = () => taskPanelAccess.error();
  const phase = (): TaskPanelPhase =>
    panelPhase({ loading: taskPanelAccess.loading(), error: error(), panel: panel() });
  return (
    <View testID="task-panel" style={{ flexDirection: "column", gap: space.md }}>
      <PanelHead sessionId={props.sessionId} />
      <If when={phase() === "loading"}>
        <PageLoading rows={4} testID="task-panel-loading" />
      </If>
      <If when={phase() === "permission"}>
        <PagePermission
          testID="task-panel-permission"
          capability={capabilityOf(error()?.requiredKey ?? "session.read").displayName}
          capabilityKey={error()?.requiredKey ?? "session.read"}
        />
      </If>
      <If when={phase() === "error"}>
        <PageError
          testID="task-panel-error"
          title="没能读到这次会话的任务记忆"
          message={error()?.message ?? ""}
          requestId={error()?.requestId}
          onRetry={() => taskPanelAccess.retry(props.sessionId)}
          retrying={taskPanelAccess.loading()}
        />
      </If>
      <If when={phase() === "empty"}>
        <PageEmpty
          testID="task-panel-empty"
          icon="layers"
          title="这次会话还没有任务卡"
          description="会话起步会有一张初始卡；工具调用、子代理和分叉都会在它下面追加卡与事件。现在还什么都没读到。"
          primaryLabel="刷新"
          onPrimary={() => void taskPanelAccess.refresh(props.sessionId)}
          primaryTestID="task-panel-empty-refresh"
        />
      </If>
      <If when={phase() === "success"}>
        <PanelSegmented />
        <If when={taskPanelAccess.section() === "graph"}>
          <TaskGraph panel={panel()!} />
        </If>
        <If when={taskPanelAccess.section() === "events"}>
          <TaskEventStream panel={panel()!} />
        </If>
        <If when={taskPanelAccess.section() === "resources"}>
          <TaskResources panel={panel()!} sessionId={props.sessionId} />
        </If>
        <PanelGaps panel={panel()!} />
      </If>
    </View>
  );
}

// ---- the graph ------------------------------------------------------------

function TaskGraph(props: { panel: ReturnType<typeof taskPanelAccess.panel> extends infer T ? NonNullable<T> : never }) {
  const rows = () => taskTree(props.panel);
  const branches = () => forkBranches(props.panel);
  return (
    <View testID="task-panel-graph" style={{ flexDirection: "column", gap: space.sm }}>
      <If when={branches().length > 0}>
        <View style={{ flexDirection: "column", gap: space.xxs, padding: space.sm, borderRadius: radius.md, backgroundColor: color.hoverFaint, borderWidth: 1, borderColor: color.border }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>分叉原点</Text>
          <Each of={branches()} key={(branch) => branch.taskId}>
            {(branch) => (
              <Text testID={`task-panel-fork-${branch().taskId}`} style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {branch().title} · seq {branch().originSeq}
              </Text>
            )}
          </Each>
        </View>
      </If>
      <View style={{ flexDirection: "column", gap: 0 }}>
        <Each of={rows()} key={(row) => row.card.taskId}>
          {(row) => <TreeRow row={row()} />}
        </Each>
      </View>
    </View>
  );
}

function TreeRow(props: { row: TaskTreeRow }) {
  const card = () => props.row.card;
  const selected = () => taskPanelAccess.selectedTaskId() === card().taskId;
  const openEvents = () => {
    taskPanelAccess.select(card().taskId);
    taskPanelAccess.setSection("events");
  };
  return (
    <View
      testID={`task-panel-card-${card().taskId}`}
      role="button"
      focusable
      onPress={openEvents}
      accessibilityLabel={`查看任务卡 ${card().title} 的事件`}
      accessibilityState={{ selected: selected() }}
      hoverStyle={{ backgroundColor: color.hover }}
      style={{
        flexDirection: isMobile() ? "column" : "row",
        alignItems: isMobile() ? "stretch" : "center",
        gap: space.sm,
        minHeight: isMobile() ? 52 : 44,
        paddingVertical: space.xs,
        paddingLeft: space.xs + props.row.depth * 14,
        paddingRight: space.xs,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
        backgroundColor: selected() ? color.selected : undefined,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, minWidth: 0, flex: isMobile() ? undefined : 1 }}>
        <If when={!props.row.member && props.row.depth === 0 && !props.row.externalParent}>
          <View style={{ width: 6, height: 6, borderRadius: radius.pill, backgroundColor: color.foreground }} />
        </If>
        <If when={props.row.depth > 0}>
          <Icon name={props.row.edge === "分叉" ? "layers" : "arrow-down"} size={11} tint={color.mutedForeground} />
        </If>
        <Text
          style={{
            flex: isMobile() ? 1 : undefined,
            minWidth: 0,
            fontSize: typeScale.sm,
            fontWeight: weight.medium,
            color: color.foreground,
            numberOfLines: 1,
            textOverflow: "ellipsis",
          }}
        >
          {card().title || "未命名卡"}
        </Text>
        {cardBadge(card().status)}
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, flexShrink: 0 }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1 }}>
          {props.row.edge}
        </Text>
        <If when={props.row.member || props.row.externalParent}>
          <Badge variant="outline" label="另一会话" />
        </If>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
          {SHORT_ID(card().taskId)}
        </Text>
      </View>
    </View>
  );
}

// ---- the event stream -----------------------------------------------------

function TaskEventStream(props: { panel: NonNullable<ReturnType<typeof taskPanelAccess.panel>> }) {
  const selected = () => taskPanelAccess.selectedTaskId();
  const lines = () => {
    const all = eventLines(props.panel);
    if (!selected()) return all;
    return all.filter((line) => line.event.taskId === selected());
  };
  const filterName = () => {
    const id = selected();
    if (!id) return null;
    const card = [...props.panel.cards ?? [], ...props.panel.members ?? [], ...props.panel.ancestors ?? []].find(
      (entry) => entry.taskId === id,
    );
    return card?.title ?? id;
  };
  return (
    <View testID="task-panel-events" style={{ flexDirection: "column", gap: space.xs }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minHeight: 28 }}>
        <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {filterName() ? `只看 ${filterName()}` : "这次会话的全部任务事件"}
        </Text>
        <If when={selected() !== null}>
          <Button
            testID="task-panel-events-all"
            variant="link"
            size="sm"
            label="看全部"
            accessibilityLabel="看全部事件"
            onPress={() => taskPanelAccess.select(null)}
          />
        </If>
      </View>
      <If
        when={lines().length > 0}
        else={
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这张卡还没有可读的事件。
          </Text>
        }
      >
        <View style={{ flexDirection: "column", gap: 0 }}>
          <Each of={lines()} key={(line) => `${line.event.seq}:${line.event.taskId}`}>
            {(line) => <EventRow line={line()} />}
          </Each>
        </View>
      </If>
    </View>
  );
}

function EventRow(props: { line: TaskEventLine }) {
  const event = () => props.line.event;
  const tone = () =>
    event().kind === "task.closed"
      ? "destructive"
      : event().kind === "task.note"
        ? "info"
        : event().kind === "task.attached" || event().kind === "task.detached"
          ? "secondary"
          : event().unparsed
            ? "warning"
            : "outline";
  return (
    <View
      testID={`task-panel-event-${event().seq}`}
      style={{
        flexDirection: "column",
        gap: 2,
        paddingVertical: space.xs,
        paddingHorizontal: space.xs,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minWidth: 0 }}>
        <Text style={{ width: 44, flexShrink: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
          {event().seq}
        </Text>
        <Badge variant={tone() as BadgeVariant} label={eventKindLabel(event().kind)} />
        <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.line.cardTitle}
          <If when={!props.line.cardKnown}>
            <Text style={{ color: color.mutedForeground }}>（另一会话的卡）</Text>
          </If>
        </Text>
        <If when={event().at !== undefined}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{formatWhen(event().at)}</Text>
        </If>
      </View>
      <If when={props.line.detail !== ""}>
        <Text
          style={{
            paddingLeft: 44 + space.sm,
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
            color: color.mutedForeground,
          }}
        >
          {props.line.detail}
        </Text>
      </If>
    </View>
  );
}

// ---- resources ------------------------------------------------------------

function TaskResources(props: { panel: NonNullable<ReturnType<typeof taskPanelAccess.panel>>; sessionId: string }) {
  const attachments = () => props.panel.attachments ?? [];
  const titleOf = (taskId: string): string =>
    [...props.panel.cards ?? [], ...props.panel.members ?? []].find((card) => card.taskId === taskId)?.title ?? taskId;
  return (
    <View testID="task-panel-resources" style={{ flexDirection: "column", gap: space.sm }}>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        资源只记录 (path, hash)；字节留在文件层，打开时按会话清单核对。
      </Text>
      <If
        when={attachments().length > 0}
        else={
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这张会话的卡还没有挂载资源。
          </Text>
        }
      >
        <View style={{ flexDirection: "column", gap: 0 }}>
          <Each of={attachments()} key={(attachment) => `${attachment.taskId}:${attachment.path}`}>
            {(attachment) => (
              <View
                testID={`task-panel-resource-${attachment().path}`}
                role="button"
                focusable
                onPress={() => void taskPanelAccess.openResource(props.sessionId, attachment())}
                accessibilityLabel={`打开资源 ${attachment().path}`}
                hoverStyle={{ backgroundColor: color.hover }}
                style={{
                  flexDirection: "row",
                  alignItems: "center",
                  gap: space.sm,
                  minHeight: 44,
                  paddingHorizontal: space.xs,
                  borderBottomWidth: 1,
                  borderBottomColor: color.border,
                }}
              >
                <Icon name="file" size={12} tint={color.mutedForeground} />
                <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {attachment().path}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1 }}>
                  {titleOf(attachment().taskId)}
                </Text>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
                  {shortHash(attachment().hash)}
                </Text>
              </View>
            )}
          </Each>
        </View>
      </If>
      <If when={taskPanelAccess.previewLoading() !== null}>
        <StateNote tone="info" icon="clock" text="正在读取文件预览…" testID="task-panel-preview-loading" />
      </If>
      <If when={taskPanelAccess.previewError() !== null}>
        <StateNote tone="danger" icon="alert" text={taskPanelAccess.previewError()?.message ?? ""} testID="task-panel-preview-error">
          <Button
            variant="outline"
            size="sm"
            label="关闭"
            accessibilityLabel="关闭预览错误"
            onPress={() => taskPanelAccess.closePreview()}
            style={touchStyle()}
          />
        </StateNote>
      </If>
      <If when={taskPanelAccess.filesError() !== null}>
        <StateNote tone="danger" icon="alert" text={`没能读到会话的文件清单：${taskPanelAccess.filesError()?.message ?? ""}`} testID="task-panel-files-error" />
      </If>
      <If when={taskPanelAccess.filesError() === null && taskPanelAccess.filesAvailable()}>
        <SessionFileAttribution panel={props.panel} sessionId={props.sessionId} />
      </If>
      <If when={taskPanelAccess.preview() !== null}>
        <ResourcePreviewCard />
      </If>
    </View>
  );
}

/** The files the manifest attributes to a card of this session (`lastTaskId`)
 * that the attachment group above does not already carry. It is the tool/fs
 * reference half of the panel: a write records its card on the manifest row,
 * so this group names what each card wrote without reading the file's bytes. A
 * file attributed to a card outside this session's tree is not listed. */
function SessionFileAttribution(props: { panel: NonNullable<ReturnType<typeof taskPanelAccess.panel>>; sessionId: string }) {
  const files = () => fileReferences(props.panel, taskPanelAccess.files());
  return (
    <View testID="task-panel-files" style={{ flexDirection: "column", gap: 0 }}>
      <Text style={{ paddingTop: space.sm, fontSize: typeScale.sm, color: color.mutedForeground }}>
        文件清单 · 按卡归属（lastTaskId）
      </Text>
      <If
        when={files().length > 0}
        else={
          <Text testID="task-panel-files-empty" style={{ paddingVertical: space.xs, fontSize: typeScale.sm, color: color.mutedForeground }}>
            文件清单里没有额外归属到本会话卡的路径。
          </Text>
        }
      >
        <Each of={files()} key={(file) => `${file.taskId}:${file.path}`}>
          {(file) => (
            <View
              testID={`task-panel-file-${file().path}`}
              role="button"
              focusable
              onPress={() =>
                void taskPanelAccess.openResource(props.sessionId, {
                  taskId: file().taskId,
                  path: file().path,
                  // The manifest's own row carries the content hash; the
                  // preview compares it instead of assuming a match. A row
                  // without one stays without a comparison.
                  hash: file().hash ?? "",
                })
              }
              accessibilityLabel={`打开文件 ${file().path}`}
              hoverStyle={{ backgroundColor: color.hover }}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.sm,
                minHeight: isMobile() ? 44 : 40,
                paddingHorizontal: space.xs,
                borderBottomWidth: 1,
                borderBottomColor: color.border,
              }}
            >
              <Icon name="file" size={12} tint={color.mutedForeground} />
              <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {file().path}
              </Text>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1 }}>
                {file().cardTitle}
              </Text>
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
                seq {file().updatedSeq}
              </Text>
            </View>
          )}
        </Each>
      </If>
      <If when={taskPanelAccess.filesHasMore() && taskPanelAccess.filesNextCursor() !== null}>
        <View style={{ paddingTop: space.xs }}>
          <Button
            testID="task-panel-files-more"
            variant="outline"
            size="sm"
            label={taskPanelAccess.filesLoadingMore() ? "正在读取下一页…" : "加载更多文件"}
            accessibilityLabel="读取文件清单的下一页"
            disabled={taskPanelAccess.filesLoadingMore()}
            onPress={() => void taskPanelAccess.loadMoreFiles(props.sessionId)}
            style={touchStyle()}
          />
        </View>
      </If>
      <If when={taskPanelAccess.filesMoreError() !== null}>
        <StateNote tone="danger" icon="alert" text={`读取下一页失败：${taskPanelAccess.filesMoreError()?.message ?? ""}`} testID="task-panel-files-more-error" />
      </If>
      <If when={taskPanelAccess.filesTruncated()}>
        <Text testID="task-panel-files-truncated" style={{ paddingVertical: space.xxs, fontSize: typeScale.sm, color: color.mutedForeground }}>
          文件清单只读到读取上限，后面还有归属没有列出来（这次读数没有给下一页的游标）。
        </Text>
      </If>
    </View>
  );
}

/** The open preview, inline under the resource list: the file's own bounded
 * page. A reference the manifest no longer matches says 文件已变化 — the panel
 * never claims the old bytes were read. */
function ResourcePreviewCard() {
  const preview = () => taskPanelAccess.preview();
  const attachment = () => preview()?.attachment;
  const view = () => preview()?.view;
  const changed = () => {
    const current = preview();
    return current ? resourceChanged(current.attachment, current.view) : null;
  };
  // A manifest-attributed file has no recorded reference hash: there is nothing
  // to compare, so the badge names the answer instead of asking for a check.
  const manifestOnly = () => (attachment()?.hash ?? "") === "";
  return (
    <View
      testID="task-panel-preview"
      style={{
        flexDirection: "column",
        gap: space.xs,
        padding: space.sm,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: changed() === true ? color.warning : color.border,
        backgroundColor: color.hoverFaint,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minWidth: 0 }}>
        <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {view()?.path ?? attachment()?.path}
        </Text>
        <If when={changed() === true}>
          <Badge variant="warning" label="文件已变化" />
        </If>
        <If when={changed() === false}>
          <Badge variant="success" label="与引用一致" />
        </If>
        <If when={changed() === null}>
          <Badge variant="outline" label={manifestOnly() ? "清单当前版本" : "未核对"} />
        </If>
        <IconButton icon="x" label="关闭预览" onPress={() => taskPanelAccess.closePreview()} />
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {view() ? previewSummary(view()!) : ""}
        {view()?.truncated ? " · 只显示第一页" : ""}
      </Text>
      <If when={changed() === true}>
        <Text style={{ fontSize: typeScale.sm, color: color.warning, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
          这张卡记录的是 {shortHash(attachment()?.hash)}，文件现在是 {shortHash(view()?.hash)}。下面是当前内容，不是卡记录的那一份。
        </Text>
      </If>
      <ScrollArea style={{ maxHeight: 240 }} accessibilityLabel={`资源 ${view()?.path ?? ""} 内容`}>
        <Text
          testID="task-panel-preview-content"
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.key,
            lineHeight: 16,
            color: color.foreground,
            userSelect: "text",
          }}
        >
          {view()?.content ?? ""}
        </Text>
      </ScrollArea>
    </View>
  );
}

// ---- gaps / sheet / entry -------------------------------------------------

function PanelGaps(props: { panel: NonNullable<ReturnType<typeof taskPanelAccess.panel>> }) {
  const gaps = () => panelGaps(props.panel);
  return (
    <If when={gaps().length > 0}>
      <View testID="task-panel-gaps" style={{ flexDirection: "column", gap: space.xxs }}>
        <Each of={gaps()} key={(gap) => gap}>
          {(gap) => (
            <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
              · {gap()}
            </Text>
          )}
        </Each>
      </View>
    </If>
  );
}

/** The run page's entry: one line under the run facts, low noise. It renders
 * only when the panel's read is wired (the capability check is the gate; the
 * fixture and a live server with the projection both answer true). */
export function TaskPanelEntry(props: { sessionId: string; open: boolean; onOpen: () => void }) {
  const counts = () => {
    const panel = taskPanelAccess.panel();
    return panel ? panelCounts(panel) : null;
  };
  return (
    <View
      testID="run-task-panel-entry"
      role="button"
      focusable
      onPress={props.onOpen}
      accessibilityLabel="打开任务面板"
      accessibilityState={{ expanded: props.open }}
      hoverStyle={{ backgroundColor: color.hover }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        minHeight: isMobile() ? 44 : 36,
        paddingHorizontal: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.hoverFaint,
      }}
    >
      <Icon name="layers" size={12} tint={color.mutedForeground} />
      <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
        任务面板 · 任务图 / 事件流 / 资源预览
      </Text>
      <If when={counts() !== null}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
          {counts()!.cards + counts()!.members} 卡 · {counts()!.events} 事件
        </Text>
      </If>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.open ? "收起" : "打开"}</Text>
      <Icon name={props.open ? "chevron-down" : "chevron-right"} size={11} tint={color.mutedForeground} />
    </View>
  );
}

/** The phone's form: the same body in a bottom Sheet (the run page's inspector
 * uses the same grammar). */
export function TaskPanelSheet(props: { open: boolean; sessionId: string; onClose: () => void }) {
  return (
    <Sheet
      testID="task-panel-sheet"
      open={props.open}
      side="bottom"
      style={{ height: 640, maxHeight: "92%" }}
      title="任务面板"
      description="任务图、事件流与资源引用；全部是这次会话账本的投影。"
      onOpenChange={(event) => {
        if (!event.open) props.onClose();
      }}
      footer={
        <Button variant="outline" label="关闭" accessibilityLabel="关闭任务面板" onPress={props.onClose} style={touchStyle()} />
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <TaskPanelBody sessionId={props.sessionId} />
      </ScrollArea>
    </Sheet>
  );
}

