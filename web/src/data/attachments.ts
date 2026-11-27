// Attachment projection reads (change keel-plugin-attachment, task 3.1).
//
// The conversation stores only a fileId plus display metadata. This module owns
// the one read of the canonical `AttachmentPart` (`GET
// /v1/sessions/{id}/attachments/{fileId}/part?seq=`), its per-attachment cache
// and its five-state face (loading / ready / error / forbidden / absent). A
// transport that predates the route (the fixture) degrades to a local part
// built from the event payload, explicitly marked as not persisted.
import { signal } from "nsh";
import type { AttachmentKind, AttachmentPart, AttachmentRef, AttachmentThumbStatus } from "./contract";
import { KeelApiError } from "./http";
import { normalizeMime } from "../plugins/mime";

export type AttachmentPartStatus = "loading" | "ready" | "error" | "forbidden";

export interface AttachmentPartState {
  status: AttachmentPartStatus;
  part?: AttachmentPart;
  /** A human-readable reason for a non-ready state. */
  reason?: string;
  /** The HTTP status, when the refusal came from the part route. */
  code?: number;
}

const states = signal<Record<string, AttachmentPartState>>({});
const inFlight = new Map<string, Promise<void>>();

export function attachmentPartKey(sessionId: string, seq: number | undefined, fileId: string | undefined): string {
  return `${sessionId}\u0000${seq ?? 0}\u0000${fileId ?? ""}`;
}

export function attachmentPartState(key: string): AttachmentPartState | undefined {
  return states()[key];
}

/** The local degradation: what the event payload already says, never presented
 * as a server projection. The reason names the missing fact so the row can
 * explain itself instead of pretending. */
export function localAttachmentPart(ref: AttachmentRef): AttachmentPart {
  const mime = normalizeMime(ref.mime);
  const thumbStatus: AttachmentThumbStatus = ref.url ? "ready" : "none";
  const deliveryReason = ref.fileId
    ? "本次投递形态未存储（attempt 结果不随消息持久化）"
    : "附件没有 fileId，只能按展示信息保留";
  return {
    fileId: ref.fileId ?? "",
    name: ref.name,
    mime,
    size: ref.size ?? 0,
    kind: attachmentKind(mime, ref.name),
    thumbStatus,
    ...(thumbStatus === "ready" && ref.url ? { thumbnailUrl: ref.url } : {}),
    downloadUrl: ref.url ?? "",
    delivery: { native: false, reason: deliveryReason },
  };
}

/** Classify by the stored MIME; the display name is consulted only when the
 * blob record carries no MIME at all (a name can lie, a stored type cannot). */
export function attachmentKind(mime: string, name?: string): AttachmentKind {
  const value = normalizeMime(mime);
  if (value) {
    if (value === "application/pdf") return "pdf";
    if (value.startsWith("image/")) return "image";
    if (value.startsWith("audio/")) return "audio";
    if (value.startsWith("video/")) return "video";
    return "file";
  }
  const lower = String(name ?? "").toLowerCase();
  const extension = lower.includes(".") ? lower.slice(lower.lastIndexOf(".")) : "";
  if ([".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif", ".bmp", ".tif", ".tiff"].includes(extension)) {
    return "image";
  }
  if (extension === ".pdf") return "pdf";
  if ([".wav", ".mp3", ".m4a", ".aac", ".ogg", ".flac"].includes(extension)) return "audio";
  if ([".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v"].includes(extension)) return "video";
  return "file";
}

export function formatAttachmentSize(size: number | undefined): string {
  if (!size || size <= 0) return "";
  if (size >= 1024 * 1024 * 1024) return `${(size / 1024 / 1024 / 1024).toFixed(1)} GB`;
  if (size >= 1024 * 1024) return `${(size / 1024 / 1024).toFixed(1)} MB`;
  return `${Math.max(1, Math.round(size / 1024))} KB`;
}

const classify = (error: unknown): AttachmentPartState => {
  if (error instanceof KeelApiError) {
    if (error.status === 403) {
      return { status: "forbidden", code: 403, reason: "没有权限读取这个附件。" };
    }
    if (error.status === 404) {
      return { status: "error", code: 404, reason: "附件不存在，或不属于当前会话。" };
    }
    if (error.status === 409) {
      return { status: "error", code: 409, reason: "附件仍在上传中，完成后再试。" };
    }
    return { status: "error", code: error.status, reason: error.message };
  }
  return { status: "error", reason: error instanceof Error ? error.message : String(error) };
};

/** Read one part once per (session, seq, fileId). A concurrent call joins the
 * in-flight read; a failed read stays failed until `retryAttachmentPart`. */
export async function loadAttachmentPart(
  key: string,
  loader: () => Promise<AttachmentPart>,
): Promise<void> {
  const existing = states()[key];
  if (existing && existing.status !== "loading") return;
  if (inFlight.has(key)) return inFlight.get(key);
  states.update((map) => ({ ...map, [key]: { status: "loading" } }));
  const run = (async () => {
    try {
      const part = await loader();
      states.update((map) => ({ ...map, [key]: { status: "ready", part } }));
    } catch (error) {
      states.update((map) => ({ ...map, [key]: classify(error) }));
    } finally {
      inFlight.delete(key);
    }
  })();
  inFlight.set(key, run);
  return run;
}

/** Seed the cache with the local degradation (no server route / no fileId). */
export function useLocalAttachmentPart(key: string, ref: AttachmentRef): void {
  const existing = states()[key];
  if (existing?.status === "ready" || existing?.status === "loading") return;
  states.update((map) => ({ ...map, [key]: { status: "ready", part: localAttachmentPart(ref) } }));
}

/** Forget one read so the row's retry can ask again. */
export function retryAttachmentPart(key: string): void {
  states.update((map) => {
    const next = { ...map };
    delete next[key];
    return next;
  });
}

/** The read-access verdict for a blob URL. `HEAD` asks the server whether this
 * principal may read the blob (`file.read`) without transferring bytes. */
export type AttachmentAccess = "checking" | "ok" | "forbidden" | "error";

/** Probe a same-origin blob URL's read access. The metadata route carries the
 * same `file.read` predicate as the bytes route but returns no viewable bytes,
 * which is why the probe never downloads the attachment. Non-`/v1/` URLs (a
 * fixture's asset, an optimistic blob:) are local and need no probe. */
export async function probeAttachmentAccess(url: string): Promise<AttachmentAccess> {
  if (!url || !url.startsWith("/v1/files/")) return "ok";
  const value = url.split("?", 1)[0]!;
  const metaUrl = value.endsWith("/meta") ? value : `${value.replace(/\/thumbnail$/, "")}/meta`;
  try {
    const response = await fetch(metaUrl, { credentials: "same-origin" });
    if (response.status === 401 || response.status === 403) return "forbidden";
    if (response.status >= 400) return "error";
    return "ok";
  } catch {
    return "error";
  }
}

/** Tests and a tenant switch: drop every cached projection. */
export function resetAttachmentParts(): void {
  states.set({});
  inFlight.clear();
}
