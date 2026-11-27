// The host's default attachment line and its five states (change
// keel-plugin-attachment, tasks 3.1-3.5).
//
// This module is the one attachment row: it reads the canonical projection
// (or a host-supplied source, which the render tests inject), draws the
// thumbnail lifecycle, the explicit no-permission face, and the plugin preview
// above the facts. The facts line — name, size, MIME and download — is always
// drawn: a preview or its failure can never hide or remove it.
import { cleanup, effect, Image, openUrl, platform, signal, Text, View } from "nsh";
import type { AttachmentPart, AttachmentRef } from "../data/contract";
import { absoluteAttachmentUrl } from "../data/attachment-link";
import {
  attachmentPartKey,
  attachmentPartState,
  formatAttachmentSize,
  loadAttachmentPart,
  localAttachmentPart,
  probeAttachmentAccess,
  refreshAttachmentPart,
  retryAttachmentPart,
  useLocalAttachmentPart,
  type AttachmentAccess,
  type AttachmentPartStatus,
} from "../data/attachments";
import { store } from "../data/store";
import { Icon, type IconName } from "../ui/icons";
import {
  Alert,
  Button,
  Card,
  Skeleton,
  Spinner,
  color,
  iconSize,
  radius,
  space,
  typeScale,
} from "../ui/kit";
import { PluginAttachmentPreview } from "../plugins/slots";
import type { SlotRegistry } from "../plugins/registry";

/** One attachment's kind glyph: the host's single icon language. */
export const attachmentIcon = (kind: string): IconName =>
  kind === "image" ? "image" : kind === "audio" || kind === "video" ? "play" : "file";

/** The projection's delivery fact in words. The server records `native:false`
 * with an "attempt not stored" reason today (design §11); a future persisted
 * attempt would render as 原生. Never invent a delivery form. */
export const deliveryLabel = (delivery: { native: boolean; reason?: string }): string => {
  if (delivery.native) return "本次投递：原生";
  const reason = delivery.reason ?? "";
  if (/not stored|未存储|attempt/i.test(reason)) return "本次投递形态未记录（attempt 结果不随消息存储）";
  return reason || "本次投递形态未记录";
};

/** What a render test (or another host) supplies instead of the module's
 * projection cache. Production passes nothing and the row loads the part. */
export interface AttachmentRowSource {
  part: AttachmentPart;
  status: AttachmentPartStatus;
  reason?: string;
  access?: AttachmentAccess;
}

/** How many non-destructive refreshes a `pending` thumbnail gets. After this
 * the row says the wait ended; it does not poll forever. */
const THUMB_POLL_LIMIT = 12;

export function AttachmentRow(props: {
  item: AttachmentRef;
  index: number;
  sessionId: string;
  seq?: number;
  source?: AttachmentRowSource;
  /** A host-supplied registry (render tests); production reads the app's. */
  registry?: SlotRegistry;
}) {
  // A row without a fileId (the fixture, an optimistic send, a legacy message)
  // still needs a cache identity of its own: name+mime+size keeps two local
  // attachments from sharing one projection.
  const cacheId =
    props.item.fileId ??
    `local:${props.item.name}\u0000${props.item.mime ?? ""}\u0000${props.item.size ?? 0}`;
  const key = attachmentPartKey(props.sessionId, props.seq, cacheId);
  const injected = () => props.source;
  const state = () => injected() ?? attachmentPartState(key);
  const localOnly = () =>
    !injected() && (!props.item.fileId || props.seq === undefined || !store.attachmentPartAvailable());
  const part = () => injected()?.part ?? attachmentPartState(key)?.part ?? localAttachmentPart(props.item);
  const status = (): AttachmentPartStatus =>
    injected()?.status ?? attachmentPartState(key)?.status ?? (localOnly() ? "ready" : "loading");
  const ready = () => status() === "ready";
  const name = () => part().name || props.item.name || `附件 ${props.index + 1}`;
  const sizeLabel = () => formatAttachmentSize(part().size || props.item.size);
  const download = () => part().downloadUrl || props.item.url || "";
  /** Only the projection's ready thumbnail is ever a thumbnail. A `none` or
   * `failed` blob is a placeholder, never the original bytes under a
   * 280×158 crop. */
  const thumbSrc = () => {
    const value = part();
    if (value.kind !== "image") return "";
    if (value.thumbStatus === "ready" && value.thumbnailUrl) return value.thumbnailUrl;
    return "";
  };
  const thumbLoadedFor = signal<string | null>(null);
  const thumbError = signal<{ src: string; code?: string } | null>(null);
  const thumbLoaded = () => !!thumbSrc() && thumbLoadedFor() === thumbSrc();
  const thumbFailure = () => {
    const failure = thumbError();
    return failure && failure.src === thumbSrc() ? failure : null;
  };
  const openNote = signal<string | null>(null);
  const access = signal<AttachmentAccess>(injected()?.access ?? "checking");
  const accessProbeFor = signal<string | null>(null);
  const accessDenied = () => access() === "forbidden";
  const thumbPolls = signal(0);
  const thumbExhausted = signal(false);

  const loader = () =>
    store.attachmentPart(props.sessionId, props.item.fileId!, props.seq!);
  /** Retry after an error: drop the cached failure and read again. */
  const reloadPart = () => {
    retryAttachmentPart(key);
    if (props.item.fileId && props.seq !== undefined && store.attachmentPartAvailable()) {
      void loadAttachmentPart(key, loader);
    }
  };
  /** Pending-thumbnail refresh: update the projection in place, never blank the
   * facts line or turn a good part into `loading`. */
  const refreshPart = () => {
    if (!props.item.fileId || props.seq === undefined || !store.attachmentPartAvailable()) return;
    void refreshAttachmentPart(key, loader);
  };
  const openDownload = async () => {
    openNote.set(null);
    try {
      const absolute = await absoluteAttachmentUrl(download());
      const result = await openUrl(absolute);
      if (!result.opened) openNote.set(`没有应用能打开：${absolute}`);
    } catch (error) {
      openNote.set(error instanceof Error ? error.message : String(error));
    }
  };
  effect(() => {
    if (injected()) return;
    if (props.item.fileId && props.seq !== undefined && store.attachmentPartAvailable()) {
      void loadAttachmentPart(key, loader);
      return;
    }
    useLocalAttachmentPart(key, props.item);
  });
  effect(() => {
    if (injected() || status() !== "ready" || part().thumbStatus !== "pending") {
      thumbExhausted.set(false);
      return;
    }
    if (thumbPolls() >= THUMB_POLL_LIMIT) {
      thumbExhausted.set(true);
      return;
    }
    const timer = setTimeout(() => {
      thumbPolls.update((value) => value + 1);
      refreshPart();
    }, 1500);
    cleanup(() => clearTimeout(timer));
  });
  // `file.read` is a different permission from the session read that produced
  // the projection. A metadata GET asks it without downloading a byte: a
  // refusal is the row's explicit no-permission face, not a broken image. The
  // probe runs in the terminal too, so the same five states reach both hosts.
  effect(() => {
    if (injected()?.access) return;
    const url = download();
    if (!url || accessProbeFor() === url) return;
    accessProbeFor.set(url);
    access.set("checking");
    void probeAttachmentAccess(url).then((verdict) => {
      if (accessProbeFor() === url) access.set(verdict);
    });
  });
  const thumbStatusLabel = () => {
    if (accessDenied()) return "无权限查看缩略图";
    if (thumbFailure()) return thumbFailure()?.code === "ERR_ACCESS_DENIED" ? "无权限查看缩略图" : "缩略图加载失败";
    switch (part().thumbStatus) {
      case "pending":
        return thumbExhausted() ? "缩略图仍未生成（已停止刷新）" : "缩略图生成中";
      case "failed":
        return "缩略图不可用";
      case "none":
        return "没有缩略图";
      case "ready":
        return "缩略图已就绪";
      default:
        return "";
    }
  };
  return (
    <Card
      size="sm"
      testID={`attachment-${props.item.fileId || `local-${props.index}`}`}
      accessibilityLabel={`附件 ${name()}`}
      style={{ alignSelf: "flex-end", maxWidth: "100%", width: 320, gap: space.sm, padding: space.sm }}
    >
      <If when={status() === "loading"}>
        <View testID="attachment-state-loading" style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Spinner size={iconSize.xs} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>正在读取附件…</Text>
        </View>
      </If>
      <If when={status() === "forbidden"}>
        <Alert
          variant="destructive"
          icon="lock"
          title="无权限查看附件"
          description="当前账号没有读取这个附件的权限；没有返回任何字节。"
          testID="attachment-state-forbidden"
        />
      </If>
      <If when={status() === "error"}>
        <Alert
          variant="destructive"
          icon="alert"
          title="附件读取失败"
          description={state()?.reason ?? "附件投影读取失败。"}
          action={
            <Button variant="ghost" size="sm" icon="refresh" label="重试" accessibilityLabel="重试读取附件" onPress={reloadPart} />
          }
          testID="attachment-state-error"
        />
      </If>
      <If when={accessDenied()}>
        <Alert
          variant="destructive"
          icon="lock"
          title="无权限查看附件内容"
          description="当前账号缺少 file.read：缩略图与下载都不会返回字节。附件名称与大小仍可见。"
          testID="attachment-state-file-forbidden"
        />
      </If>
      {/* The preview: only the plugin's additive face. The default facts below
          always render, so a preview can never make an attachment undownloadable. */}
      <If when={ready()}>
        <PluginAttachmentPreview part={part()} registry={props.registry} />
      </If>
      <If when={platform.idiom === "terminal"}>
        <View
          testID="attachment-terminal-face"
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: space.sm,
            padding: space.sm,
            borderRadius: radius.sm,
            backgroundColor: color.muted,
            borderWidth: 1,
            borderColor: color.border,
          }}
        >
          <Icon name={attachmentIcon(part().kind)} size={iconSize.sm} tint={color.mutedForeground} />
          <Text style={{ flexShrink: 1, minWidth: 0, fontSize: typeScale.xs, color: color.mutedForeground, numberOfLines: 2 }}>
            {part().kind === "image" ? `图片附件 · ${thumbStatusLabel()}` : `${part().mime || "附件"}（终端按文本展示）`}
          </Text>
        </View>
      </If>
      <If when={platform.idiom !== "terminal" && thumbSrc() !== "" && !accessDenied()}>
        <View
          style={{
            position: "relative",
            width: 280,
            maxWidth: "100%",
            height: 158,
            borderRadius: radius.sm,
            overflow: "hidden",
            borderWidth: 1,
            borderColor: color.border,
            backgroundColor: color.muted,
          }}
        >
          <If when={access() === "checking" || (!thumbLoaded() && !thumbFailure())}>
            <Skeleton
              width="100%"
              height={158}
              radius={radius.sm}
              accessibilityLabel="缩略图正在生成"
              testID="attachment-thumb-pending"
              style={{ position: "absolute", left: 0, top: 0, right: 0, bottom: 0 }}
            />
          </If>
          <If when={!!thumbFailure()}>
            <View
              testID="attachment-thumb-failed"
              style={{ flex: 1, alignItems: "center", justifyContent: "center", gap: space.xs, padding: space.sm }}
            >
              <Icon name="image" size={iconSize.md} tint={color.mutedForeground} />
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, textAlign: "center" }}>
                {thumbStatusLabel()}
              </Text>
            </View>
          </If>
          <If when={access() !== "checking"}>
            <Image
              src={thumbSrc()}
              accessibilityLabel={`缩略图 ${name()}`}
              onLoad={() => thumbLoadedFor.set(thumbSrc())}
              onError={(event) => thumbError.set({ src: thumbSrc(), code: event.code })}
              style={{
                position: "absolute",
                left: 0,
                top: 0,
                width: "100%",
                height: 158,
                objectFit: "cover",
                opacity: thumbLoaded() ? 1 : 0,
              }}
            />
          </If>
        </View>
      </If>
      <If when={platform.idiom !== "terminal" && part().kind === "image" && thumbSrc() === ""}>
        <View
          testID={part().thumbStatus === "failed" ? "attachment-thumb-failed" : "attachment-thumb-none"}
          style={{
            width: 280,
            maxWidth: "100%",
            height: 96,
            borderRadius: radius.sm,
            alignItems: "center",
            justifyContent: "center",
            gap: space.xs,
            backgroundColor: color.muted,
            borderWidth: 1,
            borderColor: color.border,
          }}
        >
          <Icon name="image" size={iconSize.lg} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{thumbStatusLabel()}</Text>
        </View>
      </If>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Icon name={attachmentIcon(part().kind)} size={iconSize.sm} tint={color.mutedForeground} />
        <Text
          style={{ flexShrink: 1, minWidth: 0, numberOfLines: 1, fontSize: typeScale.sm, color: color.foreground }}
        >
          {name()}
        </Text>
        <If when={!!sizeLabel()}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{sizeLabel()}</Text>
        </If>
        <If when={!!part().mime}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{part().mime}</Text>
        </If>
        <If when={!!download()}>
          <Button
            variant="ghost"
            size="sm"
            icon={accessDenied() ? "lock" : "download"}
            label={accessDenied() ? "无权限下载" : "下载"}
            accessibilityLabel={accessDenied() ? `无权限下载附件 ${name()}` : `下载附件 ${name()}`}
            disabled={accessDenied()}
            onPress={() => void openDownload()}
          />
        </If>
      </View>
      <If when={!!part().delivery.reason && !part().delivery.native}>
        <Text
          style={{ fontSize: typeScale.xs, color: color.mutedForeground, numberOfLines: 2 }}
          testID="attachment-delivery"
        >
          {deliveryLabel(part().delivery)}
        </Text>
      </If>
      <If when={!!openNote()}>
        <Text style={{ fontSize: typeScale.xs, color: color.danger }}>{openNote()}</Text>
      </If>
    </Card>
  );
}

/** Attachment rows are deduplicated by `fileId` (or name+mime+size for an
 * optimistic row): the transcript may carry the same file through both the
 * explicit attachments array and an image content block, and the message row
 * must not print it twice. */
export function uniqueAttachments(items: AttachmentRef[] | undefined): AttachmentRef[] {
  if (!items?.length) return [];
  const seen = new Set<string>();
  const out: AttachmentRef[] = [];
  for (const item of items) {
    if (!item || typeof item.name !== "string") continue;
    const identity = item.fileId ? `id:${item.fileId}` : `name:${item.name}\u0000${item.mime ?? ""}\u0000${item.size ?? 0}`;
    if (seen.has(identity)) continue;
    seen.add(identity);
    out.push(item);
  }
  return out;
}
