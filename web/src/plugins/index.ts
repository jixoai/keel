// The plugin UI runtime's boot seam (W12).
//
// One function installs the first-party base slots; one syncs the server's
// inventory into the registry; the palette reads the nav/action slots through
// here. Screens import components from `slots.tsx`; nothing else touches the
// singleton registry directly.
import type { PluginsView } from "../data/contract";
import { currentPluginTarget } from "./forms";
import { registerOarToolRenderer } from "./oar";
import { SlotRegistry, slots } from "./registry";
import { syncPluginInventory } from "./sync";
import type { MountedContribution, SlotTarget } from "./types";

let baseInstalled = false;

/** Declare keel.ui's base slots once, then the first-party renderers that ship
 * with this build. Idempotent; a re-register replaces the declarations with the
 * same table. */
export function installPluginRuntime(): void {
  if (baseInstalled) return;
  baseInstalled = true;
  slots.registerBaseSlots();
  // The OAR external-run card (change keel-oar-renderer) is builtin: the
  // conversation host asks the slot for `oar`, and a deployment whose inventory
  // never named `keel.oar` still renders it (or falls back to its text row when
  // the projection route says 501).
  registerOarToolRenderer(slots);
}

/** Apply one `GET /v1/plugins` answer. The base slots are always present; an
 * inventory without them still renders the first-party slots. */
export function applyPluginInventory(view: PluginsView | null | undefined): ReturnType<typeof syncPluginInventory> {
  installPluginRuntime();
  return syncPluginInventory(slots, view);
}

/** The registry the app uses (screens may read it for a custom host). */
export { slots as pluginSlots };

// ---- palette entries ------------------------------------------------------

export interface PluginActionHandlers {
  navigate(path: string): void;
  openUrl(url: string): void;
  callTool(entry: MountedContribution, tool: string, args: Record<string, unknown>): void;
  notice(text: string): void;
}

export interface PluginPaletteEntry {
  id: string;
  label: string;
  keywords: string[];
  group: "插件导航" | "插件动作";
  entry: MountedContribution;
  run(handlers: PluginActionHandlers): void;
}

const dataPayload = (entry: MountedContribution): Record<string, unknown> => {
  const payload = entry.declaration.payload;
  return payload && typeof payload === "object" && !Array.isArray(payload) ? (payload as Record<string, unknown>) : {};
};

/** The nav/action contributions a palette can show. A chunk row uses the
 * declaration's title and its module's `activate`; a data row carries its own
 * label and action. */
export function pluginPaletteEntries(
  target: SlotTarget = currentPluginTarget(),
  registry: SlotRegistry = slots,
): PluginPaletteEntry[] {
  const out: PluginPaletteEntry[] = [];
  for (const [slotID, group] of [
    ["keel.ui.nav", "插件导航"],
    ["keel.ui.action", "插件动作"],
  ] as const) {
    const view = registry.resolve(slotID, target);
    for (const entry of view?.contributions ?? []) {
      const payload = dataPayload(entry);
      const label =
        entry.declaration.title ??
        (typeof payload.label === "string" ? payload.label : entry.declaration.pluginId);
      const keywords = Array.isArray(payload.keywords)
        ? payload.keywords.filter((word): word is string => typeof word === "string")
        : [];
      out.push({
        id: `plugin:${entry.declaration.pluginId}:${entry.declaration.id}`,
        label,
        keywords,
        group,
        entry,
        run: (handlers) => {
          if (entry.declaration.form === "data") {
            runDataAction(entry, handlers);
            return;
          }
          const module = entry.module;
          const slot = registry.slot(slotID);
          if (!module?.activate || !slot) {
            handlers.notice(`插件「${entry.declaration.pluginId}」没有提供可执行的动作。`);
            return;
          }
          try {
            void module.activate({
              pluginId: entry.declaration.pluginId,
              contribution: entry.declaration,
              slot,
              target,
              bridge: registry.bridgeFor(entry.declaration.pluginId),
            });
          } catch (error) {
            handlers.notice(error instanceof Error ? error.message : String(error));
          }
        },
      });
    }
  }
  return out;
}

/** Run one `data` nav/action payload. The four action kinds are the host's
 * vocabulary; an unknown kind is reported, never guessed. */
export function runDataAction(entry: MountedContribution, handlers: PluginActionHandlers): void {
  const payload = dataPayload(entry);
  const action = typeof payload.action === "string" ? payload.action : "navigate";
  if (action === "navigate") {
    const path = typeof payload.path === "string" ? payload.path : "";
    if (!path) {
      handlers.notice(`插件「${entry.declaration.pluginId}」的导航项没有 path。`);
      return;
    }
    handlers.navigate(path);
    return;
  }
  if (action === "openUrl") {
    const url = typeof payload.url === "string" ? payload.url : "";
    if (!url) {
      handlers.notice(`插件「${entry.declaration.pluginId}」的动作没有 url。`);
      return;
    }
    handlers.openUrl(url);
    return;
  }
  if (action === "callTool") {
    const tool = typeof payload.tool === "string" ? payload.tool : "";
    if (!tool) {
      handlers.notice(`插件「${entry.declaration.pluginId}」的动作没有 tool。`);
      return;
    }
    const args = payload.args && typeof payload.args === "object" && !Array.isArray(payload.args) ? (payload.args as Record<string, unknown>) : {};
    handlers.callTool(entry, tool, args);
    return;
  }
  handlers.notice(`插件动作「${action}」不是 navigate/openUrl/callTool。`);
}
