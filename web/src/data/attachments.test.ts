// The attachment projection's client-side state machine (change
// keel-plugin-attachment, task 3.1/3.3). The route is not exercised here; the
// loader is injected, so the five states and the local degradation are pinned
// without a server.
import { expect, test } from "nsh/test";
import type { AttachmentPart } from "./contract";
import {
  attachmentKind,
  attachmentPartKey,
  attachmentPartState,
  formatAttachmentSize,
  loadAttachmentPart,
  localAttachmentPart,
  resetAttachmentParts,
  refreshAttachmentPart,
  retryAttachmentPart,
} from "./attachments";
import { uniqueAttachments } from "../screens/attachment-row";
import { KeelApiError } from "./http";

const part: AttachmentPart = {
  fileId: "11111111-1111-4111-8111-111111111111",
  name: "shot.png",
  mime: "image/png",
  size: 2048,
  kind: "image",
  thumbStatus: "ready",
  thumbnailUrl: "/v1/files/11111111-1111-4111-8111-111111111111/thumbnail",
  downloadUrl: "/v1/files/11111111-1111-4111-8111-111111111111",
  delivery: { native: false, reason: "not stored" },
};

test("local part: display facts only, with an explicit not-persisted reason", () => {
  const local = localAttachmentPart({ name: "fig.png", mime: "image/png", size: 1024, url: "asset:/fig.png" });
  expect(local.kind).toBe("image");
  expect(local.thumbStatus).toBe("ready");
  expect(local.thumbnailUrl).toBe("asset:/fig.png");
  expect(local.downloadUrl).toBe("asset:/fig.png");
  expect(local.delivery.native).toBe(false);
  expect(local.delivery.reason).toContain("fileId");
  expect(localAttachmentPart({ name: "报告.pdf", mime: "application/pdf" }).kind).toBe("pdf");
});

test("kind and size: MIME authoritative, name only when the record has none", () => {
  expect(attachmentKind("image/webp")).toBe("image");
  expect(attachmentKind("application/pdf", "x.png")).toBe("pdf");
  expect(attachmentKind("", "photo.JPG")).toBe("image");
  expect(attachmentKind("text/plain", "photo.png")).toBe("file");
  expect(formatAttachmentSize(0)).toBe("");
  expect(formatAttachmentSize(2048)).toBe("2 KB");
  expect(formatAttachmentSize(2 * 1024 * 1024)).toBe("2.0 MB");
});

test("projection states: loading → ready, forbidden, error, retry", async () => {
  resetAttachmentParts();
  const key = attachmentPartKey("session-1", 3, part.fileId);
  const pending = loadAttachmentPart(key, () => Promise.resolve(part));
  expect(attachmentPartState(key)?.status).toBe("loading");
  await pending;
  expect(attachmentPartState(key)?.status).toBe("ready");
  expect(attachmentPartState(key)?.part?.mime).toBe("image/png");

  const forbiddenKey = attachmentPartKey("session-1", 4, "forbidden");
  await loadAttachmentPart(forbiddenKey, () => Promise.reject(new KeelApiError(403, "no file.read")));
  expect(attachmentPartState(forbiddenKey)?.status).toBe("forbidden");
  expect(attachmentPartState(forbiddenKey)?.code).toBe(403);

  const missingKey = attachmentPartKey("session-1", 5, "missing");
  await loadAttachmentPart(missingKey, () => Promise.reject(new KeelApiError(404, "not found")));
  expect(attachmentPartState(missingKey)?.status).toBe("error");
  expect(attachmentPartState(missingKey)?.reason).toContain("不存在");

  retryAttachmentPart(missingKey);
  expect(attachmentPartState(missingKey)).toBe(undefined);
  await loadAttachmentPart(missingKey, () => Promise.resolve(part));
  expect(attachmentPartState(missingKey)?.status).toBe("ready");
});

test("projection cache: a ready read is not repeated for the same identity", async () => {
  resetAttachmentParts();
  const key = attachmentPartKey("session-1", 9, part.fileId);
  let calls = 0;
  await loadAttachmentPart(key, () => {
    calls += 1;
    return Promise.resolve(part);
  });
  await loadAttachmentPart(key, () => {
    calls += 1;
    return Promise.resolve(part);
  });
  expect(calls).toBe(1);
});

test("refresh: a pending-thumbnail refresh never blanks a good projection", async () => {
  resetAttachmentParts();
  const key = attachmentPartKey("session-1", 11, part.fileId);
  await loadAttachmentPart(key, () => Promise.resolve(part));
  await refreshAttachmentPart(key, () => Promise.reject(new Error("transient")));
  expect(attachmentPartState(key)?.status).toBe("ready");
  expect(attachmentPartState(key)?.part?.thumbStatus).toBe("ready");
  const pending = { ...part, thumbStatus: "pending" as const, thumbnailUrl: undefined };
  await refreshAttachmentPart(key, () => Promise.resolve(pending));
  expect(attachmentPartState(key)?.status).toBe("ready");
  expect(attachmentPartState(key)?.part?.thumbStatus).toBe("pending");
});

test("dedupe: the same fileId (or the same local identity) renders once", () => {
  expect(
    uniqueAttachments([
      { fileId: "f-1", name: "a.png", mime: "image/png", size: 10 },
      { fileId: "f-1", name: "a.png", mime: "image/png", size: 10 },
      { fileId: "f-2", name: "b.pdf", mime: "application/pdf", size: 20 },
      { name: "local.png", mime: "image/png", size: 30 },
      { name: "local.png", mime: "image/png", size: 30 },
    ]).length,
  ).toBe(3);
  expect(uniqueAttachments([])).toEqual([]);
  expect(uniqueAttachments(undefined)).toEqual([]);
});
