// TUI harness · one attachment row's five states and plugin preview fallbacks
// (change keel-plugin-attachment, tasks 3.2-3.5). The scenario comes from the
// launch parameters (`host.params.scenario`).
//
//   success        a ready projection with a ready thumbnail
//   pending        thumbnail still generating (bounded refresh face)
//   failed         thumbnail generation failed (placeholder, never the original)
//   none           no thumbnail (placeholder, never the original)
//   loading        the canonical projection is being read
//   error          the projection read failed (retry face)
//   part-forbidden the part route refused (session/permission face)
//   file-forbidden the projection is readable but file.read is not
//   ready          a chunk preview renders above the facts
//   throwing       the chunk throws; the facts line stays
//   loadfail       the chunk cannot load; the facts line stays
//   loading-preview the chunk never finishes; the 4s timeout face
//   iframe         an iframe claim in a terminal: graphical interface required
//   no-claim       no contribution: the facts line alone
//
// The row is the production component with a host-supplied source; the facts
// line is what every scenario must keep.
import { host, mount, Text, View } from "nsh";
import type { AttachmentPart } from "../data/contract";
import type { AttachmentAccess, AttachmentPartStatus } from "../data/attachments";
import { SlotRegistry } from "../plugins/registry";
import { AttachmentRow, type AttachmentRowSource, uniqueAttachments } from "./attachment-row";

const scenario = (): string => String(host.params.scenario ?? "success");

const basePart = (overrides: Partial<AttachmentPart> = {}): AttachmentPart => ({
  fileId: "11111111-1111-4111-8111-111111111111",
  name: "shot.png",
  mime: "image/png",
  size: 39_037,
  kind: "image",
  width: 1280,
  height: 720,
  thumbStatus: "ready",
  thumbnailUrl: "/v1/files/11111111-1111-4111-8111-111111111111/thumbnail",
  downloadUrl: "/v1/files/11111111-1111-4111-8111-111111111111",
  delivery: { native: false, reason: "delivery is decided during an attempt and is not stored with this message" },
  ...overrides,
});

const contribution = (form: string, entry: string, mime = ["image/*"]): Record<string, unknown> => ({
  id: "preview",
  slot: "keel.ui.message-part",
  form,
  entry,
  part: "attachment",
  mime,
});

function sourceFor(name: string): { source: AttachmentRowSource; part: AttachmentPart } {
  switch (name) {
    case "pending":
      return { part: basePart({ thumbStatus: "pending", thumbnailUrl: undefined }), source: { part: basePart({ thumbStatus: "pending", thumbnailUrl: undefined }), status: "ready", access: "ok" } };
    case "failed":
      return { part: basePart({ thumbStatus: "failed", thumbnailUrl: undefined }), source: { part: basePart({ thumbStatus: "failed", thumbnailUrl: undefined }), status: "ready", access: "ok" } };
    case "none":
      return { part: basePart({ thumbStatus: "none", thumbnailUrl: undefined }), source: { part: basePart({ thumbStatus: "none", thumbnailUrl: undefined }), status: "ready", access: "ok" } };
    case "loading":
      return { part: basePart(), source: { part: basePart(), status: "loading" as AttachmentPartStatus } };
    case "error":
      return { part: basePart(), source: { part: basePart(), status: "error" as AttachmentPartStatus, reason: "投影读取失败（测试夹具）" } };
    case "part-forbidden":
      return { part: basePart(), source: { part: basePart(), status: "forbidden" as AttachmentPartStatus } };
    case "file-forbidden":
      return { part: basePart(), source: { part: basePart(), status: "ready", access: "forbidden" as AttachmentAccess } };
    default:
      return { part: basePart(), source: { part: basePart(), status: "ready", access: "ok" } };
  }
}

const contributionsFor = (name: string): Record<string, unknown>[] => {
  switch (name) {
    case "ready":
      return [contribution("chunk", "fixture:w12-hello")];
    case "throwing":
      return [contribution("chunk", "fixture:w12-throwing")];
    case "loadfail":
      return [contribution("chunk", "missing.js")];
    case "loading-preview":
      return [contribution("chunk", "fixture:w12-loading")];
    case "iframe":
      return [contribution("iframe", "/plugins/demo.attach/ui/preview.html")];
    case "no-claim":
      return [];
    default:
      return [];
  }
};

const registry = new SlotRegistry();
registry.registerBaseSlots();
const contributes = contributionsFor(scenario());
if (contributes.length > 0) registry.registerPlugin({ pluginId: "demo.attach", contributes });
const { source } = sourceFor(scenario());
const baseItem = {
  fileId: source.part.fileId,
  name: source.part.name ?? "shot.png",
  mime: source.part.mime,
  size: source.part.size,
};
const items =
  scenario() === "empty"
    ? []
    : scenario() === "duplicate"
      ? [baseItem, { ...baseItem }]
      : [baseItem];
const rows = uniqueAttachments(items);

function Harness() {
  return (
    <View testID="attachment-harness" style={{ padding: 16, gap: 12, flexDirection: "column" }}>
      <If when={rows.length === 0}>
        <Text testID="list-empty">没有附件行</Text>
      </If>
      {rows.map((item, index) => (
        <AttachmentRow
          item={item}
          index={index}
          sessionId="harness-session"
          source={source}
          registry={registry}
        />
      ))}
      <If when={rows.length > 0}>
        <Text testID="facts-guard">{`事实行守卫 · ${source.part.name} · ${source.part.mime} · ${source.part.size}`}</Text>
      </If>
    </View>
  );
}

mount(Harness);
