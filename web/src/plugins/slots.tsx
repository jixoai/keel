// Slot hosts (W12): the one place plugins are drawn.
//
// Every host reads the registry through `resolve(slot, target)`, so loading,
// target fallback and failure states are the registry's answers, not each
// screen's guess. A chunk renders inside a guarded call: a throwing plugin
// shows its code in place and leaves the rest of the page alone.
import type { JSX } from "nsh";
import { cleanup, effect, openUrl, signal, Text, View, WebView } from "nsh";
import { absoluteAttachmentUrl } from "../data/attachment-link";
import type { AttachmentPart } from "../data/contract";
import { Alert, Button, Spinner, alpha, color, font, iconSize, radius, space, typeScale, weight } from "../ui/kit";
import { currentPluginTarget } from "./forms";
import type { SlotRegistry } from "./registry";
import { slots as globalRegistry } from "./registry";
import type { AttachmentPreviewProps, MountedContribution, PluginChunkContext, SlotTarget } from "./types";

const registryOf = (registry?: SlotRegistry): SlotRegistry => registry ?? globalRegistry;

// ---- the failure / unavailable faces --------------------------------------

/** A contribution that failed to load: the code, the sentence, and a retry.
 * The slot around it keeps rendering (spec: 贡献失败不拖垮宿主). */
export function PluginFailure(props: {
  entry: MountedContribution;
  registry?: SlotRegistry;
  compact?: boolean;
}) {
  const pluginId = props.entry.declaration.pluginId;
  const retry = () => void registryOf(props.registry).reloadPlugin(pluginId);
  return (
    <View
      testID={`plugin-failure-${props.entry.declaration.id}`}
      accessibilityLabel={`插件贡献失败 ${props.entry.declaration.slot}`}
      style={{
        minWidth: 0,
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        paddingHorizontal: space.sm,
        paddingVertical: space.xxs,
        borderRadius: radius.sm,
        backgroundColor: color.dangerTint,
        borderWidth: 1,
        borderColor: alpha(color.danger, 0.3),
        flexWrap: "wrap",
      }}
    >
      <Text style={{ fontSize: typeScale.xs, fontWeight: weight.semibold, color: color.danger }}>
        {props.entry.code ?? "ERR_PLUGIN"}
      </Text>
      <Text
        style={{
          flexShrink: 1,
          minWidth: 0,
          fontSize: typeScale.xs,
          color: color.foreground,
          numberOfLines: props.compact ? 1 : 3,
        }}
      >
        {props.entry.reason ?? "插件贡献装载失败。"}
      </Text>
      <Button
        variant="ghost"
        size="sm"
        icon="refresh"
        label="重试"
        accessibilityLabel={`重试插件 ${pluginId}`}
        onPress={retry}
      />
    </View>
  );
}

/** A form/target the host cannot run here: explicit, never a blank hole. */
export function PluginUnavailable(props: { code?: string; reason?: string; testID?: string }) {
  return (
    <View
      testID={props.testID ?? "plugin-unavailable"}
      accessibilityLabel="插件贡献在此目标不可用"
      style={{
        minWidth: 0,
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        paddingHorizontal: space.sm,
        paddingVertical: space.xxs,
        borderRadius: radius.sm,
        backgroundColor: color.warningTint,
        borderWidth: 1,
        borderColor: alpha(color.warning, 0.3),
      }}
    >
      <Text style={{ fontSize: typeScale.xs, fontWeight: weight.semibold, color: color.warning }}>
        {props.code ?? "ERR_TARGET_UNAVAILABLE"}
      </Text>
      <Text
        style={{
          flexShrink: 1,
          minWidth: 0,
          fontSize: typeScale.xs,
          color: color.foreground,
          numberOfLines: 2,
        }}
      >
        {props.reason ?? "这个贡献在当前目标不可用。"}
      </Text>
    </View>
  );
}

// ---- rendering one mounted contribution -----------------------------------

function safeRender(
  entry: MountedContribution,
  slotID: string,
  slot: ReturnType<SlotRegistry["slot"]>,
  registry: SlotRegistry,
  target: SlotTarget,
  props?: unknown,
  failure?: (entry: MountedContribution) => JSX.Element,
): JSX.Element {
  const module = entry.module;
  if (!module) return failure ? failure(entry) : <PluginFailure entry={entry} registry={registry} />;
  const declaration = slot;
  if (!declaration) return failure ? failure(entry) : <PluginFailure entry={entry} registry={registry} />;
  const context: PluginChunkContext = {
    pluginId: entry.declaration.pluginId,
    contribution: entry.declaration,
    slot: declaration,
    target,
    bridge: registry.bridgeFor(entry.declaration.pluginId),
    props,
  };
  try {
    return module.render(context) ?? (
      <PluginUnavailable code="ERR_PLUGIN" reason="这个 chunk 没有渲染任何内容。" />
    );
  } catch (error) {
    if (failure) {
      return failure({
        ...entry,
        code: "ERR_PLUGIN",
        reason: error instanceof Error ? error.message : String(error),
      });
    }
    return (
      <PluginFailure
        entry={{
          ...entry,
          code: "ERR_PLUGIN",
          reason: error instanceof Error ? error.message : String(error),
        }}
        registry={registry}
      />
    );
  }
}

/** Draws a data contribution as a compact host row (a nav item, an action,
 * the top band). The payload is already schema-validated by the registry. */
export function PluginDataRow(props: { entry: MountedContribution; detail?: string }) {
  const payload = (props.entry.declaration.payload ?? {}) as { label?: unknown; detail?: unknown };
  const label =
    props.entry.declaration.title ??
    (typeof payload.label === "string" ? payload.label : props.entry.declaration.pluginId);
  const detail =
    props.detail ??
    (typeof payload.detail === "string" ? payload.detail : props.entry.declaration.description ?? "");
  return (
    <View
      testID={`plugin-data-${props.entry.declaration.id}`}
      style={{ minWidth: 0, flexDirection: "column", gap: 1 }}
    >
      <Text
        style={{
          fontSize: typeScale.sm,
          fontWeight: weight.semibold,
          color: color.foreground,
          numberOfLines: 1,
        }}
      >
        {label}
      </Text>
      <If when={!!detail}>
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.xs,
            color: color.mutedForeground,
            numberOfLines: 1,
          }}
        >
          {detail}
        </Text>
      </If>
    </View>
  );
}

// ---- the generic slot host ------------------------------------------------

export function PluginSlot(props: {
  slot: string;
  registry?: SlotRegistry;
  testID?: string;
  /** What a ready contribution draws. */
  render: (entry: MountedContribution, target: SlotTarget) => JSX.Element;
  /** What an unavailable/failed contribution draws (defaults to the warning
   * note or the failure face). */
  renderUnavailable?: (entry: MountedContribution) => JSX.Element;
  /** Skip the warning face for slots where hiding an entry is the explicit
   * degradation (a nav row with no target). */
  showUnavailable?: boolean;
  style?: Record<string, unknown>;
}) {
  const registry = registryOf(props.registry);
  const target = () => currentPluginTarget();
  const view = () => registry.resolve(props.slot, target());
  const showUnavailable = () => props.showUnavailable !== false;
  return (
    <View testID={props.testID ?? `plugin-slot-${props.slot}`} style={props.style ?? { minWidth: 0 }}>
      <If when={!!view() && !!view()?.unsupported}>
        <PluginUnavailable
          code={view()?.unsupported?.code}
          reason={view()?.unsupported?.reason}
          testID={`plugin-slot-unavailable-${props.slot}`}
        />
      </If>
      <If when={!!view() && !view()?.unsupported}>
        <Each of={view()?.contributions ?? []} key={(entry) => `${entry.declaration.pluginId}:${entry.declaration.id}`}>
          {(entry) => (
            <View style={{ minWidth: 0 }}>
              <If when={entry().status === "loading"}>
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                  <Spinner size={iconSize.xs} tint={color.mutedForeground} />
                  <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                    {`正在装载 ${entry().declaration.title ?? entry().declaration.id}…`}
                  </Text>
                </View>
              </If>
              <If when={entry().status !== "loading"}>
                {props.render(entry(), target())}
              </If>
            </View>
          )}
        </Each>
        <If when={showUnavailable() && (view()?.unavailable.length ?? 0) > 0}>
          <Each of={view()?.unavailable ?? []} key={(entry) => `unavailable-${entry.declaration.pluginId}:${entry.declaration.id}`}>
            {(entry) =>
              props.renderUnavailable ? (
                props.renderUnavailable(entry())
              ) : entry().status === "failed" ? (
                <PluginFailure entry={entry()} registry={registry} />
              ) : (
                <PluginUnavailable code={entry().code} reason={entry().reason} />
              )
            }
          </Each>
        </If>
      </If>
    </View>
  );
}

/** A chunk or data contribution drawn as a top-band panel (the A4.4f use
 * case). */
export function PluginTopBandEntry(props: { entry: MountedContribution; registry?: SlotRegistry }) {
  const registry = registryOf(props.registry);
  const target = currentPluginTarget();
  return (
    <If
      when={props.entry.declaration.form === "chunk"}
      else={<PluginDataRow entry={props.entry} />}
    >
      {() =>
        safeRender(
          props.entry,
          props.entry.declaration.slot,
          registry.slot(props.entry.declaration.slot),
          registry,
          target,
        )
      }
    </If>
  );
}

// ---- the specific slots ---------------------------------------------------

/** The workspace sidebar's plugin section: a heading, then the sidebar
 * contributions; nothing when the slot has none. */
export function PluginSidebarSlot(props: { registry?: SlotRegistry }) {
  const registry = registryOf(props.registry);
  const view = () => registry.resolve("keel.ui.sidebar", currentPluginTarget());
  const hasContent = () => (view()?.contributions.length ?? 0) > 0 || (view()?.unavailable.length ?? 0) > 0;
  return (
    <If when={hasContent()}>
      <View
        testID="plugin-sidebar-slot"
        accessibilityLabel="插件侧栏"
        style={{
          paddingHorizontal: space.md,
          paddingVertical: space.sm,
          gap: space.xs,
          borderTopWidth: 1,
          borderTopColor: color.sidebarBorder,
          backgroundColor: color.sidebar,
        }}
      >
        <Text
          style={{
            fontSize: typeScale.xs,
            fontWeight: weight.medium,
            letterSpacing: 0.5,
            color: color.mutedForeground,
            textTransform: "uppercase",
          }}
        >
          插件
        </Text>
        <PluginSlot
          slot="keel.ui.sidebar"
          registry={registry}
          testID="plugin-sidebar-contributions"
          render={(entry, target) =>
            entry.declaration.form === "iframe" ? (
              <PluginIframeEntry entry={entry} compact />
            ) : (
              safeRender(entry, entry.declaration.slot, view()?.slot, registry, target)
            )
          }
        />
      </View>
    </If>
  );
}

/** The settings page's contributed sections (drawn inside 插件). */
export function PluginSettingsSections(props: { registry?: SlotRegistry }) {
  const registry = registryOf(props.registry);
  const target = () => currentPluginTarget();
  const view = () => registry.resolve("keel.ui.settings-section", target());
  const hasContent = () =>
    (view()?.contributions.length ?? 0) > 0 || (view()?.unavailable.length ?? 0) > 0;
  return (
    <If when={hasContent()}>
      <View testID="plugin-settings-sections" style={{ gap: space.lg }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
          插件设置区
        </Text>
        <Each of={view()?.contributions ?? []} key={(entry) => `${entry.declaration.pluginId}:${entry.declaration.id}`}>
          {(entry) => (
            <View
              style={{
                padding: space.lg,
                gap: space.sm,
                borderRadius: radius.lg,
                backgroundColor: color.card,
                borderWidth: 1,
                borderColor: color.border,
              }}
            >
              {entry().declaration.form === "iframe" ? (
                <PluginIframeEntry entry={entry()} />
              ) : entry().status === "loading" ? (
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                  <Spinner size={iconSize.xs} tint={color.mutedForeground} />
                  <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>正在装载…</Text>
                </View>
              ) : (
                safeRender(
                  entry(),
                  entry().declaration.slot,
                  view()?.slot,
                  registry,
                  target(),
                )
              )}
            </View>
          )}
        </Each>
        <Each
          of={view()?.unavailable ?? []}
          key={(entry) => `settings-unavailable-${entry.declaration.id}`}
        >
          {(entry) =>
            entry().status === "failed" ? (
              <PluginFailure entry={entry()} registry={registry} />
            ) : (
              <PluginUnavailable code={entry().code} reason={entry().reason} />
            )
          }
        </Each>
      </View>
    </If>
  );
}

/** The action slot's buttons, drawn where the host has room for actions (the
 * plugin settings page). A data action is handled by `runPluginDataAction`. */
export function PluginActionButtons(props: { registry?: SlotRegistry; onData: (entry: MountedContribution) => void }) {
  const registry = registryOf(props.registry);
  const target = () => currentPluginTarget();
  const view = () => registry.resolve("keel.ui.action", target());
  const hasContent = () =>
    (view()?.contributions.length ?? 0) > 0 || (view()?.unavailable.length ?? 0) > 0;
  const activate = (entry: MountedContribution) => {
    if (entry.declaration.form === "data") {
      props.onData(entry);
      return;
    }
    const module = entry.module;
    const slot = view()?.slot;
    if (!module?.activate || !slot) return;
    try {
      void module.activate({
        pluginId: entry.declaration.pluginId,
        contribution: entry.declaration,
        slot,
        target: target(),
        bridge: registry.bridgeFor(entry.declaration.pluginId),
      });
    } catch {
      /* the failure is the plugin's own; the host stays */
    }
  };
  const labelOf = (entry: MountedContribution): string => {
    if (entry.declaration.title) return entry.declaration.title;
    const payload = entry.declaration.payload;
    if (payload && typeof payload === "object" && !Array.isArray(payload)) {
      const label = (payload as { label?: unknown }).label;
      if (typeof label === "string" && label) return label;
    }
    return entry.declaration.pluginId;
  };
  return (
    <If when={hasContent()}>
      <View testID="plugin-action-buttons" style={{ gap: space.sm }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Each of={view()?.contributions ?? []} key={(entry) => `action-${entry.declaration.id}`}>
            {(entry) => (
              <Button
                variant="outline"
                size="sm"
                icon="play"
                label={labelOf(entry())}
                accessibilityLabel={`插件动作 ${labelOf(entry())}`}
                onPress={() => activate(entry())}
              />
            )}
          </Each>
        </View>
        <Each of={view()?.unavailable ?? []} key={(entry) => `action-unavailable-${entry.declaration.id}`}>
          {(entry) =>
            entry().status === "failed" ? (
              <PluginFailure entry={entry()} registry={registry} compact />
            ) : (
              <PluginUnavailable code={entry().code} reason={entry().reason} />
            )
          }
        </Each>
      </View>
    </If>
  );
}

/** The tool-renderer slot: drawn inside the default tool row, above the text
 * result. A renderer for one tool (or every tool) replaces nothing else. */
export function PluginToolRendererSlot(props: { call: { toolCallId: string; name: string; args?: unknown; result?: unknown; error?: string } }) {
  const registry = globalRegistry;
  const target = currentPluginTarget();
  const entry = () => registry.toolRenderer(props.call.name, target);
  return (
    <If when={entry()}>
      {(candidate) =>
        safeRender(
          candidate(),
          candidate().declaration.slot,
          registry.slot("keel.ui.tool-renderer"),
          registry,
          target,
          props.call,
        )
      }
    </If>
  );
}

/** A tool renderer resolved by name, for a host that supplies its own props
 * and its own degradation (the conversation's external-run card). Unlike
 * `PluginToolRendererSlot` this never draws a plugin failure into the
 * conversation: no contribution, a failed chunk or a missing module is the
 * host's own face, so a plugin can never park a turn. */
export function PluginToolRendererEntry(props: {
  toolName: string;
  /** What the ready contribution draws (the host's own data). */
  props?: unknown;
  /** The host's face when the slot cannot run here (text, in the conversation). */
  fallback: JSX.Element;
  registry?: SlotRegistry;
}): JSX.Element {
  const registry = registryOf(props.registry);
  const target = currentPluginTarget();
  const candidate = () => registry.toolRenderer(props.toolName, target);
  return (
    <If when={candidate()} else={props.fallback}>
      {(entry) =>
        safeRender(
          entry(),
          entry().declaration.slot,
          registry.slot("keel.ui.tool-renderer"),
          registry,
          target,
          props.props,
        )
      }
    </If>
  );
}

/** The message-part slot: a contribution may draw a part kind (a task card, a
 * deliverable) in the host tree. Renders nothing when no plugin claims it. */
export function PluginMessagePartSlot(props: { partKind: string; props?: unknown }) {
  const registry = globalRegistry;
  const target = currentPluginTarget();
  const entry = () => registry.messagePart(props.partKind, target);
  return (
    <If when={entry()}>
      {(candidate) =>
        safeRender(
          candidate(),
          candidate().declaration.slot,
          registry.slot("keel.ui.message-part"),
          registry,
          target,
          props.props,
        )
      }
    </If>
  );
}

/** How long a matching attachment contribution may stay in `declared/loading`
 * before the host says so and falls back. Measured per attachment row; a slow
 * plugin never blocks the message or another attachment. */
const ATTACHMENT_PREVIEW_TIMEOUT_MS = 4000;

/** The attachment preview slot (change keel-plugin-attachment, tasks 3.2-3.5).
 *
 * The host's default attachment line is drawn by the caller and is never
 * replaced: this component adds a preview above it, and every failure face
 * explains why the preview is absent. Selection is the registry's
 * declaration-order answer for this MIME; a plugin without
 * `part = attachment` is never a candidate.
 *
 * Forms: `chunk` renders in the host tree on every target (TUI included);
 * `iframe` draws a labelled non-native frame on web/native and an explicit
 * "terminal unavailable" face on TUI; `package` is an explicit unsupported
 * face in this client. A throwing chunk and a load failure both leave the
 * default row in place. */
export function PluginAttachmentPreview(props: {
  part: AttachmentPart;
  registry?: SlotRegistry;
  testID?: string;
}) {
  const registry = registryOf(props.registry);
  const target = () => currentPluginTarget();
  const selection = () => registry.attachmentSelection(props.part.mime, target());
  const ready = () => selection().ready;
  const unavailable = () => selection().unavailable[0];
  const unavailableToShow = () => (ready() || timedOut() ? undefined : selection().unavailable[0]);
  const loading = () => selection().loading.length > 0;
  const timedOut = signal(false);
  const openNote = signal<string | null>(null);
  const retry = () => {
    const entry = loading() ? selection().loading[0] : unavailable();
    timedOut.set(false);
    if (entry) void registry.reloadPlugin(entry.declaration.pluginId);
  };
  effect(() => {
    if (ready() || !loading()) {
      timedOut.set(false);
      return;
    }
    const timer = setTimeout(() => {
      if (!selection().ready && selection().loading.length > 0) timedOut.set(true);
    }, ATTACHMENT_PREVIEW_TIMEOUT_MS);
    cleanup(() => clearTimeout(timer));
  });
  const openExternal = async (url: string) => {
    openNote.set(null);
    try {
      const absolute = await absoluteAttachmentUrl(url);
      const result = await openUrl(absolute);
      if (!result.opened) openNote.set(`没有应用能打开：${absolute}`);
    } catch (error) {
      openNote.set(error instanceof Error ? error.message : String(error));
    }
  };
  const contextProps = (): AttachmentPreviewProps => ({
    part: props.part,
    target: target(),
    withDefaultRow: true,
  });
  /** The attachment slot's own failure face: plain Chinese, the plugin's
   * reason, and a retry. The technical `ERR_*` stays in the diagnostic code,
   * not as the sentence a reader sees. */
  const failureFace = (entry: MountedContribution): JSX.Element => (
    <Alert
      variant="default"
      icon="alert"
      title="附件预览失败"
      description={entry.reason ?? "预览插件没有完成渲染；默认附件行仍可查看与下载。"}
      action={
        <Button
          variant="ghost"
          size="sm"
          icon="refresh"
          label="重试"
          accessibilityLabel={`重试附件预览 ${entry.declaration.pluginId}`}
          onPress={() => void registry.reloadPlugin(entry.declaration.pluginId)}
        />
      }
      testID="attachment-preview-failure"
    />
  );
  /** An iframe page receives the same read-only projection as a chunk, as
   * query parameters (there is no host-tree props path for it). Values are
   * encoded; the page must not treat the name as a path. */
  const iframeSrc = (entry: MountedContribution): string => {
    const base = entry.declaration.entry ?? "";
    if (!base) return base;
    const value = props.part;
    const pairs = [
      ["fileId", value.fileId],
      ["name", value.name ?? ""],
      ["mime", value.mime],
      ["size", String(value.size)],
      ["kind", value.kind],
      ["thumbStatus", value.thumbStatus],
      ["downloadUrl", value.downloadUrl],
    ].map(([key, item]) => `${key}=${encodeURIComponent(item)}`);
    return `${base}${base.includes("?") ? "&" : "?"}${pairs.join("&")}`;
  };
  return (
    <View testID={props.testID ?? `plugin-attachment-${props.part.fileId || "local"}`} style={{ minWidth: 0, gap: space.sm }}>
      <If when={ready()}>
        {(entry) => (
          <View style={{ minWidth: 0 }}>
            {entry().declaration.form === "iframe" ? (
              <PluginIframeEntry entry={entry()} src={iframeSrc(entry())} />
            ) : entry().declaration.form === "package" ? (
              <PluginUnavailable
                code="ERR_UNSUPPORTED"
                reason="附件预览使用签名包表面，本客户端还没有接线；已回退到默认附件行。"
              />
            ) : (
              safeRender(
                entry(),
                entry().declaration.slot,
                registry.slot("keel.ui.message-part"),
                registry,
                target(),
                contextProps(),
                failureFace,
              )
            )}
          </View>
        )}
      </If>
      <If when={!ready() && loading() && !timedOut()}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Spinner size={iconSize.xs} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>正在装载附件预览…</Text>
        </View>
      </If>
      <If when={!ready() && timedOut()}>
        <Alert
          variant="default"
          icon="clock"
          title="附件预览加载超时"
          description={`预览插件在 ${ATTACHMENT_PREVIEW_TIMEOUT_MS / 1000} 秒内没有就绪，已回退到默认附件行；其他附件不受影响。`}
          action={
            <Button variant="ghost" size="sm" icon="refresh" label="重试" accessibilityLabel="重试附件预览" onPress={retry} />
          }
          testID="attachment-preview-timeout"
        />
      </If>
      <If when={unavailableToShow()}>
        {(entry) => (
          <View style={{ minWidth: 0, gap: space.xs }}>
            {entry().declaration.form === "iframe" &&
            entry().code === "ERR_TARGET_UNAVAILABLE" &&
            target() === "tui" ? (
              <Alert
                variant="default"
                icon="terminal"
                title="终端不可用（需要图形界面）"
                description={`预览插件「${entry().declaration.pluginId}」使用 iframe；请在有图形界面的浏览器中打开。默认附件行仍可查看与下载。`}
                action={
                  entry().declaration.entry ? (
                    <Button
                      variant="outline"
                      size="sm"
                      icon="external"
                      label="在浏览器打开"
                      accessibilityLabel="在浏览器打开附件预览"
                      onPress={() => void openExternal(iframeSrc(entry()))}
                    />
                  ) : undefined
                }
                testID="attachment-preview-terminal"
              />
            ) : entry().status === "failed" ? (
              <Alert
                variant="default"
                icon="alert"
                title="附件预览失败"
                description={entry().reason ?? "预览插件没有完成渲染；默认附件行仍可查看与下载。"}
                action={
                  <Button
                    variant="ghost"
                    size="sm"
                    icon="refresh"
                    label="重试"
                    accessibilityLabel={`重试附件预览 ${entry().declaration.pluginId}`}
                    onPress={() => void registry.reloadPlugin(entry().declaration.pluginId)}
                  />
                }
                testID="attachment-preview-failure"
              />
            ) : (
              <PluginUnavailable code={entry().code} reason={entry().reason} />
            )}
            <If when={!!openNote()}>
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{openNote()}</Text>
            </If>
            <If when={!!entry().declaration.entry && entry().declaration.form === "iframe"}>
              <Text
                style={{
                  fontFamily: font.mono,
                  fontSize: typeScale.xs,
                  color: color.mutedForeground,
                  numberOfLines: 1,
                }}
              >
                {iframeSrc(entry())}
              </Text>
            </If>
          </View>
        )}
      </If>
    </View>
  );
}

/** The iframe fallback: an explicit browser frame, labelled as non-native
 * theme. The terminal never reaches it (the registry marks the target
 * unsupported first). */
export function PluginIframeEntry(props: { entry: MountedContribution; compact?: boolean; src?: string }) {
  const entryUrl = () => props.entry.declaration.entry ?? "";
  const url = () => props.src ?? entryUrl();
  const open = async () => {
    try {
      const absolute = await absoluteAttachmentUrl(url());
      await openUrl(absolute);
    } catch {
      /* the address stays on screen; openUrl refuses cross-origin or unknown */
    }
  };
  return (
    <View
      testID={`plugin-iframe-${props.entry.declaration.id}`}
      style={{ minWidth: 0, gap: space.sm }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.xs, fontWeight: weight.semibold, color: color.warning }}>
          iframe · 非原生主题
        </Text>
        <Text
          style={{
            flexShrink: 1,
            minWidth: 0,
            fontFamily: font.mono,
            fontSize: typeScale.xs,
            color: color.mutedForeground,
            numberOfLines: 1,
          }}
        >
          {url()}
        </Text>
        <Button
          variant="ghost"
          size="sm"
          icon="external"
          label="打开"
          accessibilityLabel="打开插件页面"
          onPress={() => void open()}
        />
      </View>
      <If when={!props.compact}>
        <IframeFrame url={url()} id={props.entry.declaration.id} />
      </If>
    </View>
  );
}

/** The frame's application policy is its URL: validation refuses anything but
 * a same-origin `/plugins/<id>/` page, so the web view has no remote, data: or
 * protocol-relative source. NSH exposes no per-frame sandbox on this target;
 * the hosted namespace and the plugin's declared capabilities are the boundary. */
function IframeFrame(props: { url: string; id: string }) {
  return (
    <WebView
      testID={`plugin-iframe-frame-${props.id}`}
      accessibilityLabel={`插件页面 ${props.url}`}
      src={props.url}
      style={{
        width: "100%",
        height: 320,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.card,
      }}
    />
  );
}

/** The surface slot: a package/iframe page. The package runner is not wired
 * in this slice, so the form says so instead of drawing an empty page. */
export function PluginSurfaceHost(props: { contribution: MountedContribution; registry?: SlotRegistry }) {
  const entry = props.contribution;
  if (entry.declaration.form === "iframe") return <PluginIframeEntry entry={entry} />;
  return (
    <PluginUnavailable
      code="ERR_UNSUPPORTED"
      reason="签名包表面（独立 VM）尚未在本客户端接线；iframe 兜底可用。"
      testID={`plugin-package-${entry.declaration.id}`}
    />
  );
}

/** Every surface contribution (optionally one plugin's), drawn where the host
 * has a page-sized body. A package is an explicit unsupported face in this
 * slice; an iframe is the legacy fallback. */
export function PluginSurfaceSlot(props: { pluginId?: string; registry?: SlotRegistry }) {
  const registry = registryOf(props.registry);
  const target = () => currentPluginTarget();
  const view = () => registry.resolve("keel.ui.surface", target());
  const entries = () =>
    [...(view()?.contributions ?? []), ...(view()?.unavailable ?? [])].filter(
      (entry) => !props.pluginId || entry.declaration.pluginId === props.pluginId,
    );
  return (
    <If when={entries().length > 0}>
      <View testID="plugin-surface-slot" style={{ minWidth: 0, gap: space.md }}>
        <Each of={entries()} key={(entry) => `surface-${entry.declaration.pluginId}:${entry.declaration.id}`}>
          {(entry) => <PluginSurfaceHost contribution={entry()} registry={registry} />}
        </Each>
      </View>
    </If>
  );
}

/** The route slot for one plugin: the first route contribution it made. */
export function PluginRouteSurface(props: { pluginId: string; registry?: SlotRegistry }) {
  const registry = registryOf(props.registry);
  const target = currentPluginTarget();
  const view = () => registry.resolve("keel.ui.route", target);
  const entry = () =>
    view()?.contributions.find((candidate) => candidate.declaration.pluginId === props.pluginId) ??
    view()?.unavailable.find((candidate) => candidate.declaration.pluginId === props.pluginId);
  return (
    <View style={{ flex: 1, minHeight: 0 }}>
      <If when={!entry()}>
        <PluginUnavailable code="ERR_NOT_FOUND" reason="这个插件没有声明可渲染的路由贡献。" />
      </If>
      <If when={entry()}>
        {(candidate) =>
          candidate().status === "failed" ? (
            <PluginFailure entry={candidate()} registry={registry} />
          ) : candidate().status === "unavailable" ? (
            <PluginUnavailable code={candidate().code} reason={candidate().reason} />
          ) : candidate().declaration.form === "iframe" ? (
            <PluginIframeEntry entry={candidate()} />
          ) : (
            safeRender(
              candidate(),
              candidate().declaration.slot,
              view()?.slot,
              registry,
              target,
            )
          )
        }
      </If>
    </View>
  );
}
