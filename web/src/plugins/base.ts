// The first-party client plugin's slot declarations (W12).
//
// The harness owns no slot list (spec: plugin-ui-extension, "槽位由插件声明"):
// these declarations belong to `keel.ui`, the first-party client plugin. The
// registry treats them like any other plugin's `provides.slots`, which is why
// they carry `provider: "keel.ui"` and the same validation every third-party
// declaration goes through. A future keel.ui package can replace this table
// with its manifest without moving the mechanism.
import type { SlotDeclaration } from "./types";

export const KEEL_UI_PLUGIN = "keel.ui";

/** The eight base slots plus the top band's own slot (the A4.4f contract: the
 * top band is the plugin row, never the app's navigation). */
export const BASE_SLOT_IDS = {
  route: "keel.ui.route",
  nav: "keel.ui.nav",
  action: "keel.ui.action",
  toolRenderer: "keel.ui.tool-renderer",
  messagePart: "keel.ui.message-part",
  sidebar: "keel.ui.sidebar",
  settingsSection: "keel.ui.settings-section",
  surface: "keel.ui.surface",
  topBand: "keel.ui.top-band",
} as const;

export type BaseSlotName = keyof typeof BASE_SLOT_IDS;

const allTargets = ["web", "native", "tui"] as const;

/** `data` schemas: the host consumes these JSON documents, so the slot owns
 * their shape (spec: `data` 贡献按槽位 schema 装载并校验). */
const NAV_SCHEMA = {
  type: "object",
  required: ["label"],
  properties: {
    label: { type: "string", minLength: 1, maxLength: 64 },
    path: { type: "string", maxLength: 512 },
    keywords: { type: "array", items: { type: "string", maxLength: 64 } },
    order: { type: "number" },
  },
};

const ACTION_SCHEMA = {
  type: "object",
  required: ["label"],
  properties: {
    label: { type: "string", minLength: 1, maxLength: 64 },
    action: { type: "string", enum: ["navigate", "callTool", "openUrl"] },
    path: { type: "string", maxLength: 512 },
    tool: { type: "string", maxLength: 128 },
    args: { type: "object" },
    url: { type: "string", maxLength: 2048 },
    keywords: { type: "array", items: { type: "string", maxLength: 64 } },
  },
};

const ROUTE_SCHEMA = {
  type: "object",
  required: ["path"],
  properties: {
    path: { type: "string", minLength: 1, maxLength: 512 },
    label: { type: "string", maxLength: 64 },
    title: { type: "string", maxLength: 128 },
  },
};

const TOP_BAND_SCHEMA = {
  type: "object",
  required: ["label"],
  properties: {
    label: { type: "string", minLength: 1, maxLength: 64 },
    detail: { type: "string", maxLength: 128 },
  },
};

const declaration = (
  name: BaseSlotName,
  forms: SlotDeclaration["forms"],
  arity: SlotDeclaration["arity"],
  targets: SlotDeclaration["targets"],
  schema?: Record<string, unknown>,
): SlotDeclaration => ({
  id: BASE_SLOT_IDS[name],
  provider: KEEL_UI_PLUGIN,
  forms,
  arity,
  targets,
  schema,
  builtin: true,
});

export const BASE_SLOTS: SlotDeclaration[] = [
  declaration("route", ["chunk", "iframe", "data"], "multi", [...allTargets], ROUTE_SCHEMA),
  declaration("nav", ["chunk", "data"], "multi", [...allTargets], NAV_SCHEMA),
  declaration("action", ["chunk", "data"], "multi", [...allTargets], ACTION_SCHEMA),
  declaration("toolRenderer", ["chunk", "iframe"], "multi", [...allTargets]),
  declaration("messagePart", ["chunk", "iframe"], "multi", [...allTargets]),
  declaration("sidebar", ["chunk", "iframe"], "multi", [...allTargets]),
  declaration("settingsSection", ["chunk", "iframe"], "multi", [...allTargets]),
  // A surface is a page-sized thing: the package form runs it as its own VM,
  // iframe is the legacy fallback. A chunk contribution is a host-tree
  // component, not a surface, so it is not accepted here.
  declaration("surface", ["package", "iframe"], "multi", ["web", "native"]),
  declaration("topBand", ["chunk", "data"], "multi", [...allTargets], TOP_BAND_SCHEMA),
];
