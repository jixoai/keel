// TUI harness · one attachment row's preview selection and fallbacks (change
// keel-plugin-attachment, tasks 3.2-3.5). The scenario comes from the launch
// parameters (`host.params.scenario`); the default line below the preview is
// the host's own, exactly as `messages.tsx` keeps it.
//
//   ready      a chunk claim renders the canonical part (the normal face)
//   throwing   the chunk throws: the failure is explained, the line stays
//   loadfail   the chunk cannot load: same fallback, different reason
//   loading    the chunk never finishes: the 4s timeout face
//   iframe     an iframe claim in a terminal: "graphical interface required"
//   none       no claim: the host's default line alone, no blank preview
import { host, mount, Text, View } from "nsh";
import type { AttachmentPart } from "../data/contract";
import { SlotRegistry } from "../plugins/registry";
import { PluginAttachmentPreview } from "../plugins/slots";

const scenario = (): string => String(host.params.scenario ?? "ready");

const part: AttachmentPart = {
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
};

const contributionsFor = (name: string): Record<string, unknown>[] => {
  switch (name) {
    case "throwing":
      return [
        {
          id: "preview",
          slot: "keel.ui.message-part",
          form: "chunk",
          entry: "fixture:w12-throwing",
          part: "attachment",
          mime: ["image/*"],
        },
      ];
    case "loadfail":
      return [
        {
          id: "preview",
          slot: "keel.ui.message-part",
          form: "chunk",
          entry: "missing.js",
          part: "attachment",
          mime: ["image/*"],
        },
      ];
    case "loading":
      return [
        {
          id: "preview",
          slot: "keel.ui.message-part",
          form: "chunk",
          entry: "fixture:w12-loading",
          part: "attachment",
          mime: ["image/*"],
        },
      ];
    case "iframe":
      return [
        {
          id: "preview",
          slot: "keel.ui.message-part",
          form: "iframe",
          entry: "/plugins/demo.attach/ui/preview.html",
          part: "attachment",
          mime: ["image/*"],
        },
      ];
    case "none":
      return [];
    default:
      return [
        {
          id: "preview",
          slot: "keel.ui.message-part",
          form: "chunk",
          entry: "fixture:w12-hello",
          part: "attachment",
          mime: ["image/*"],
        },
      ];
  }
};

const registry = new SlotRegistry();
registry.registerBaseSlots();
const contributes = contributionsFor(scenario());
if (contributes.length > 0) registry.registerPlugin({ pluginId: "demo.attach", contributes });

function Harness() {
  return (
    <View testID="attachment-harness" style={{ padding: 16, gap: 12, flexDirection: "column" }}>
      <Text testID="default-row">{`默认附件行 · ${part.name} · ${part.mime} · ${part.size}`}</Text>
      <Text testID="download-entry">{`下载入口 · ${part.downloadUrl}`}</Text>
      <PluginAttachmentPreview part={part} registry={registry} testID="preview-slot" />
    </View>
  );
}

mount(Harness);
