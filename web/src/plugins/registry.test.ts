// The W12 slot registry, pinned without a screen: validation, duplicate
// handling, target fallback, the four forms, load/unload and the inventory
// sync. The chunk form uses the bundled fixture, so the same test host proves
// the terminal path (no fetch) and the host-tree render contract.
import { expect, test } from "nsh/test";
import type { PluginsView } from "../data/contract";
import { BASE_SLOTS, KEEL_UI_PLUGIN } from "./base";
import { currentPluginTarget } from "./forms";
import { chunkSpecifier } from "./loader";
import { SlotRegistry } from "./registry";
import { pluginPaletteEntries, runDataAction, type PluginActionHandlers } from "./index";
import { pluginContributions, pluginSlots, resetPluginSync, syncPluginInventory } from "./sync";
import type { SlotIssue } from "./types";

const registry = () => {
  const slots = new SlotRegistry();
  slots.registerBaseSlots();
  return slots;
};

const codes = (issues: SlotIssue[]): string[] => issues.map((issue) => issue.code);

const wait = async (ms = 10) => new Promise((resolve) => setTimeout(resolve, ms));

const fixturePlugin = (extra: Record<string, unknown> = {}) => ({
  pluginId: "demo.panel",
  contributes: [
    {
      id: "hello",
      slot: "keel.ui.top-band",
      form: "chunk",
      entry: "fixture:w12-hello",
      title: "W12 插件条",
      capabilities: ["state"],
    },
  ],
  ...extra,
});

test("base slots: keel.ui declares the eight slots plus the top band", () => {
  const slots = registry();
  const ids = slots.slots().map((slot) => slot.id);
  for (const id of [
    "keel.ui.route",
    "keel.ui.nav",
    "keel.ui.action",
    "keel.ui.tool-renderer",
    "keel.ui.message-part",
    "keel.ui.sidebar",
    "keel.ui.settings-section",
    "keel.ui.surface",
    "keel.ui.top-band",
  ]) {
    expect(ids.includes(id)).toBe(true);
  }
  expect(slots.slots().every((slot) => slot.provider === KEEL_UI_PLUGIN)).toBe(true);
  expect(BASE_SLOTS.length).toBe(9);
});

test("slot validation: a foreign namespace, a missing form set and an unknown target fail closed", () => {
  const slots = registry();
  const foreign = slots.registerPlugin({
    pluginId: "demo.panel",
    slots: [{ id: "other.panel.widget", forms: ["chunk"], targets: ["web"] }],
  });
  expect(foreign.ok).toBe(false);
  expect(codes(foreign.issues)).toContain("ERR_SLOT_INVALID");

  const noForms = slots.registerPlugin({
    pluginId: "demo.panel",
    slots: [{ id: "demo.panel.widget", forms: [], targets: ["web"] }],
  });
  expect(noForms.ok).toBe(false);

  const badTarget = slots.registerPlugin({
    pluginId: "demo.panel",
    slots: [{ id: "demo.panel.widget", forms: ["chunk"], targets: ["watch"] }],
  });
  expect(badTarget.ok).toBe(false);
  expect(codes(badTarget.issues)).toContain("ERR_SLOT_INVALID");

  const noTarget = slots.registerPlugin({
    pluginId: "demo.panel",
    slots: [{ id: "demo.panel.widget", forms: ["chunk"] }],
  });
  expect(noTarget.ok).toBe(false);
  expect(codes(noTarget.issues)).toContain("ERR_SLOT_INVALID");
});

test("duplicate slots: the same provider replaces, another provider conflicts", () => {
  const slots = registry();
  const first = slots.registerPlugin({
    pluginId: "demo",
    slots: [{ id: "demo.one.widget", forms: ["chunk"], targets: ["web"] }],
  });
  expect(first.ok).toBe(true);
  const again = slots.registerPlugin({
    pluginId: "demo",
    slots: [{ id: "demo.one.widget", forms: ["chunk", "data"], targets: ["web", "tui"] }],
  });
  expect(again.ok).toBe(true);
  expect(slots.slot("demo.one.widget")?.forms.length).toBe(2);

  // `demo.one` also owns the `demo.one.` namespace, so the same id is a real
  // conflict between two declaring plugins.
  const other = slots.registerPlugin({
    pluginId: "demo.one",
    slots: [{ id: "demo.one.widget", forms: ["chunk"], targets: ["web"] }],
  });
  expect(other.ok).toBe(false);
  expect(codes(other.issues)).toContain("ERR_SLOT_CONFLICT");
  // The failed declaration left the original untouched.
  expect(slots.slot("demo.one.widget")?.provider).toBe("demo");

  // A provider that withdraws a slot takes its contributors with it.
  slots.registerPlugin({
    pluginId: "demo.consumer",
    contributes: [{ id: "widget", slot: "demo.one.widget", form: "chunk", entry: "fixture:w12-hello" }],
  });
  expect(slots.contributions().length).toBe(1);
  const withdrawn = slots.registerPlugin({ pluginId: "demo", slots: [] });
  expect(withdrawn.ok).toBe(true);
  expect(slots.slot("demo.one.widget")).toBe(undefined);
  expect(slots.contributions().length).toBe(0);
  expect(codes(slots.issues())).toContain("ERR_SLOT_UNKNOWN");
});

test("contribution validation: a form the slot does not accept, a missing entry and an escaping path", () => {
  const slots = registry();
  const iframe = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [{ id: "bad", slot: "keel.ui.action", form: "iframe", entry: "/x.html" }],
  });
  expect(iframe.ok).toBe(false);
  expect(codes(iframe.issues)).toContain("ERR_FORM_NOT_ALLOWED");

  const noEntry = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [{ id: "bad", slot: "keel.ui.action", form: "chunk" }],
  });
  expect(noEntry.ok).toBe(false);
  expect(codes(noEntry.issues)).toContain("ERR_CONTRIBUTION_INVALID");

  const escape = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [{ id: "bad", slot: "keel.ui.action", form: "chunk", entry: "../x.js" }],
  });
  expect(escape.ok).toBe(false);
});

test("duplicate contributions: same plugin replaces, another plugin conflicts, singleton is full", () => {
  const slots = registry();
  const one = slots.registerPlugin(fixturePlugin());
  expect(one.ok).toBe(true);
  const replace = slots.registerPlugin(fixturePlugin({ contributes: [
    { id: "hello", slot: "keel.ui.top-band", form: "chunk", entry: "fixture:w12-hello", title: "替换" },
  ] }));
  expect(replace.ok).toBe(true);
  expect(slots.contributions().filter((entry) => entry.declaration.id === "hello").length).toBe(1);
  expect(slots.contributions()[0].declaration.title).toBe("替换");

  const other = slots.registerPlugin({
    pluginId: "demo.other",
    contributes: [{ id: "hello", slot: "keel.ui.top-band", form: "chunk", entry: "fixture:w12-hello" }],
  });
  expect(other.ok).toBe(false);
  expect(codes(other.issues)).toContain("ERR_CONTRIBUTION_CONFLICT");

  const single = slots.registerPlugin({
    pluginId: "demo.single",
    slots: [{ id: "demo.single.widget", forms: ["chunk"], arity: "single", targets: ["web"] }],
    contributes: [{ id: "one", slot: "demo.single.widget", form: "chunk", entry: "fixture:w12-hello" }],
  });
  expect(single.ok).toBe(true);
  const second = slots.registerPlugin({
    pluginId: "demo.other",
    contributes: [{ id: "two", slot: "demo.single.widget", form: "chunk", entry: "fixture:w12-hello" }],
  });
  expect(second.ok).toBe(false);
  expect(codes(second.issues)).toContain("ERR_SLOT_FULL");
});

test("target fallback: package and iframe say unavailable in a terminal, chunk and data do not", () => {
  const slots = registry();
  slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      { id: "chunk", slot: "keel.ui.settings-section", form: "chunk", entry: "fixture:w12-hello" },
      { id: "iframe", slot: "keel.ui.settings-section", form: "iframe", entry: "/plugins/demo.panel/ui/index.html" },
      { id: "surface", slot: "keel.ui.surface", form: "iframe", entry: "/plugins/demo.panel/ui/index.html" },
      { id: "package", slot: "keel.ui.surface", form: "package", entry: "ui/demo.nshpkg" },
    ],
  });
  const view = slots.resolve("keel.ui.settings-section", "tui");
  expect(view?.contributions.some((entry) => entry.declaration.id === "chunk")).toBe(true);
  expect(view?.unavailable.some((entry) => entry.declaration.id === "iframe")).toBe(true);
  const iframe = view?.unavailable.find((entry) => entry.declaration.id === "iframe");
  expect(iframe?.code).toBe("ERR_TARGET_UNAVAILABLE");
  expect(iframe?.reason?.includes("终端")).toBe(true);

  const surface = slots.resolve("keel.ui.surface", "tui");
  expect(surface?.unsupported?.code).toBe("ERR_TARGET_UNAVAILABLE");
  // The package runner is not wired: the form says so on a target that could
  // otherwise run it, rather than drawing an empty page.
  const webSurface = slots.resolve("keel.ui.surface", "web");
  expect(webSurface?.unsupported).toBe(undefined);
  const packaged = webSurface?.unavailable.find((entry) => entry.declaration.id === "package");
  expect(packaged?.code).toBe("ERR_UNSUPPORTED");
  expect(webSurface?.contributions.some((entry) => entry.declaration.id === "surface")).toBe(true);
});

test("data contributions: the slot schema rejects a nav item without a label", () => {
  const slots = registry();
  const missing = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [{ id: "nav", slot: "keel.ui.nav", form: "data", payload: { path: "/settings" } }],
  });
  expect(missing.ok).toBe(false);
  expect(codes(missing.issues)).toContain("ERR_CONTRIBUTION_INVALID");

  const good = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      { id: "nav", slot: "keel.ui.nav", form: "data", payload: { label: "面板", path: "/plugins", keywords: ["panel"] } },
      { id: "act", slot: "keel.ui.action", form: "data", payload: { label: "打开设置", action: "navigate", path: "/settings" } },
    ],
  });
  expect(good.ok).toBe(true);
  const entries = pluginPaletteEntries("web", slots);
  expect(entries.filter((entry) => entry.group === "插件导航").length).toBe(1);
  expect(entries.filter((entry) => entry.group === "插件动作").length).toBe(1);
});

test("data actions: navigate/openUrl/callTool are the closed vocabulary", () => {
  const slots = registry();
  slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      { id: "nav", slot: "keel.ui.nav", form: "data", payload: { label: "去插件", path: "/plugins" } },
      { id: "act", slot: "keel.ui.action", form: "data", payload: { label: "打开链接", action: "openUrl", url: "https://example.com" } },
    ],
  });
  const calls: string[] = [];
  const handlers: PluginActionHandlers = {
    navigate: (path) => calls.push(`navigate:${path}`),
    openUrl: (url) => calls.push(`openUrl:${url}`),
    callTool: (_entry, tool) => calls.push(`call:${tool}`),
    notice: (text) => calls.push(`notice:${text}`),
  };
  const entries = pluginPaletteEntries("web", slots);
  entries.find((entry) => entry.group === "插件导航")?.run(handlers);
  entries.find((entry) => entry.group === "插件动作")?.run(handlers);
  expect(calls).toEqual(["navigate:/plugins", "openUrl:https://example.com"]);
});

test("chunk lifecycle: the bundled fixture loads, renders, and unload leaves no residue", async () => {
  const slots = registry();
  const result = slots.registerPlugin(fixturePlugin());
  expect(result.ok).toBe(true);
  await wait(20);
  const mounted = slots.contributions()[0];
  expect(mounted.status).toBe("ready");
  expect(typeof mounted.module?.render).toBe("function");
  const context = {
    pluginId: mounted.declaration.pluginId,
    contribution: mounted.declaration,
    slot: slots.slot("keel.ui.top-band")!,
    target: "web" as const,
    bridge: slots.bridgeFor(mounted.declaration.pluginId),
  };
  expect(mounted.module?.render(context)).not.toBe(null);
  const removed = slots.unregisterPlugin("demo.panel");
  expect(removed).toBeGreaterThan(0);
  expect(slots.contributions().length).toBe(0);
  expect(slots.slots().length).toBe(BASE_SLOTS.length);
});

test("unloading a slot provider withdraws every contribution to its slots", () => {
  const slots = registry();
  slots.registerPlugin({
    pluginId: "demo.provider",
    slots: [{ id: "demo.provider.widget", forms: ["chunk"], targets: ["web"] }],
  });
  const contributor = slots.registerPlugin({
    pluginId: "demo.consumer",
    contributes: [{ id: "widget", slot: "demo.provider.widget", form: "chunk", entry: "fixture:w12-hello" }],
  });
  expect(contributor.ok).toBe(true);
  slots.unregisterPlugin("demo.provider");
  expect(slots.slots().some((slot) => slot.id === "demo.provider.widget")).toBe(false);
  expect(slots.contributions().length).toBe(0);
  expect(codes(slots.issues())).toContain("ERR_SLOT_UNKNOWN");
});

test("bridge: an undeclared verb fails with ERR_ACCESS_DENIED before any host call", async () => {
  const slots = registry();
  slots.registerPlugin(fixturePlugin());
  const bridge = slots.bridgeFor("demo.panel");
  let code = "";
  try {
    await bridge.callTool("anything", {});
  } catch (error) {
    code = (error as { code?: string }).code ?? "";
  }
  expect(code).toBe("ERR_ACCESS_DENIED");

  let subscribeCode = "";
  try {
    bridge.onEvent(() => {});
  } catch (error) {
    subscribeCode = (error as { code?: string }).code ?? "";
  }
  expect(subscribeCode).toBe("ERR_ACCESS_DENIED");
});

test("inventory sync: enabled plugins load, disabled/uninstalled ones unload", async () => {
  resetPluginSync();
  const slots = registry();
  const inventory = (state: string): PluginsView => ({
    generation: 1,
    plugins: [
      {
        id: "keel.w12.fixture",
        state,
        tools: [],
        provides: {},
        contributes: [
          { id: "hello", slot: "keel.ui.top-band", form: "chunk", entry: "fixture:w12-hello", title: "W12" },
        ],
      },
    ],
  });
  const applied = syncPluginInventory(slots, inventory("enabled"));
  expect(applied.registered).toContain("keel.w12.fixture");
  await wait(20);
  expect(slots.contributions().length).toBe(1);
  const disabled = syncPluginInventory(slots, inventory("disabled"));
  expect(disabled.unregistered).toContain("keel.w12.fixture");
  expect(slots.contributions().length).toBe(0);
});

test("inventory sync: provides.slots is read, so a third-party slot provider works", () => {
  resetPluginSync();
  const slots = registry();
  const view: PluginsView = {
    plugins: [
      {
        id: "demo.provider",
        state: "enabled",
        provides: { slots: [{ id: "demo.provider.widget", forms: ["data"], arity: "multi", targets: ["web"] }] },
      },
      {
        id: "demo.consumer",
        state: "enabled",
        contributes: [
          { id: "widget", slot: "demo.provider.widget", form: "data", payload: { label: "部件" } },
        ],
      },
    ],
  };
  const result = syncPluginInventory(slots, view);
  expect(result.failed).toEqual([]);
  expect(slots.slot("demo.provider.widget")?.provider).toBe("demo.provider");
  expect(slots.resolve("demo.provider.widget", "web")?.contributions.length).toBe(1);
});

test("inventory readers tolerate both the documented and the early field spellings", () => {
  expect(pluginSlots({ provides: { slots: [{ id: "a.b" }] } })).toEqual([{ id: "a.b" }]);
  expect(pluginSlots({ slots: [{ id: "a.c" }] })).toEqual([{ id: "a.c" }]);
  expect(pluginContributions({ contributes: [{ slot: "a.b", form: "data" }] }).length).toBe(1);
  expect(pluginContributions({ contributions: [{ slot: "a.b", form: "data" }] }).length).toBe(1);
  expect(currentPluginTarget()).toBe("native");
  expect(typeof runDataAction).toBe("function");
});

test("chunk specifier: a package entry is same-origin; a bundled entry stays a fixture", () => {
  expect(chunkSpecifier("ui/panel.js", "demo.panel")).toBe("/plugins/demo.panel/ui/panel.js");
  expect(chunkSpecifier("/plugins/demo.panel/ui/panel.js", "demo.panel")).toBe(
    "/plugins/demo.panel/ui/panel.js",
  );
  expect(chunkSpecifier("fixture:w12-hello", "demo.panel")).toBe("fixture:w12-hello");
});

// ---- attachment preview claims (change keel-plugin-attachment, W3) ----------

test("attachment selection: only part=attachment claims; a plain message-part contribution is not a renderer", async () => {
  const slots = registry();
  const result = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      { id: "assistant-part", slot: "keel.ui.message-part", form: "chunk", entry: "fixture:w12-hello" },
    ],
  });
  expect(result.ok).toBe(true);
  await wait(30);
  expect(slots.messagePart("assistant", "web")?.declaration.id).toBe("assistant-part");
  expect(slots.attachmentSelection("image/png", "web").ready).toBe(undefined);
  expect(slots.attachmentSelection("image/png", "web").unavailable.length).toBe(0);
});

test("attachment selection: declaration order, MIME narrowing, and no match", async () => {
  const slots = registry();
  const result = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      {
        id: "pdf",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["application/pdf"],
      },
      {
        id: "image-wild",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["image/*"],
      },
      {
        id: "image-exact",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["image/png"],
      },
    ],
  });
  expect(result.ok).toBe(true);
  await wait(40);
  expect(slots.attachmentSelection("application/pdf", "web").ready?.declaration.id).toBe("pdf");
  expect(slots.attachmentSelection("image/png", "web").ready?.declaration.id).toBe("image-wild");
  expect(slots.attachmentSelection("image/webp", "web").ready?.declaration.id).toBe("image-wild");
  expect(slots.attachmentSelection("text/plain", "web").ready).toBe(undefined);
  expect(slots.attachmentSelection("", "web").ready).toBe(undefined);
});

test("attachment selection: an iframe claim is unavailable in the terminal, explicitly", () => {
  const slots = registry();
  const result = slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      {
        id: "pdf-frame",
        slot: "keel.ui.message-part",
        form: "iframe",
        entry: "/plugins/demo.panel/ui/pdf.html",
        part: "attachment",
        mime: ["application/pdf"],
      },
    ],
  });
  expect(result.ok).toBe(true);
  const terminal = slots.attachmentSelection("application/pdf", "tui");
  expect(terminal.ready).toBe(undefined);
  expect(terminal.unavailable[0]?.code).toBe("ERR_TARGET_UNAVAILABLE");
  expect(terminal.unavailable[0]?.reason?.includes("终端")).toBe(true);
  const web = slots.attachmentSelection("application/pdf", "web");
  expect(web.ready?.declaration.id).toBe("pdf-frame");
});

test("attachment claims: bad MIME grammar, MIME without the claim, and repeats fail closed", () => {
  const badGrammar = registry().registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      {
        id: "bad",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["IMAGE/PNG"],
      },
    ],
  });
  expect(badGrammar.ok).toBe(false);
  expect(badGrammar.issues.some((issue) => issue.field === "contributes[0].mime[0]")).toBe(true);

  const noClaim = registry().registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      { id: "widen", slot: "keel.ui.message-part", form: "chunk", entry: "fixture:w12-hello", mime: ["image/png"] },
    ],
  });
  expect(noClaim.ok).toBe(false);

  const wrongPart = registry().registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      {
        id: "other",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "assistant",
      },
    ],
  });
  expect(wrongPart.ok).toBe(false);

  const duplicate = registry().registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      {
        id: "first",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["image/png"],
      },
      {
        id: "second",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["image/png"],
      },
    ],
  });
  expect(duplicate.ok).toBe(false);
  expect(duplicate.issues.some((issue) => issue.field === "contributes[1].mime")).toBe(true);
});

test("attachment selection: unregistering the plugin withdraws the preview", async () => {
  const slots = registry();
  slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [
      {
        id: "image",
        slot: "keel.ui.message-part",
        form: "chunk",
        entry: "fixture:w12-hello",
        part: "attachment",
        mime: ["image/*"],
      },
    ],
  });
  await wait(30);
  expect(slots.attachmentSelection("image/png", "web").ready?.declaration.id).toBe("image");
  slots.unregisterPlugin("demo.panel");
  expect(slots.attachmentSelection("image/png", "web").ready).toBe(undefined);
});

test("messagePart: a no-part contribution is a wildcard for host kinds, never for attachments", async () => {
  const slots = registry();
  slots.registerPlugin({
    pluginId: "demo.panel",
    contributes: [{ id: "plain", slot: "keel.ui.message-part", form: "chunk", entry: "fixture:w12-hello" }],
  });
  await wait(30);
  expect(slots.messagePart("assistant", "web")?.declaration.id).toBe("plain");
  expect(slots.messagePart("attachment", "web")).toBe(undefined);
});
