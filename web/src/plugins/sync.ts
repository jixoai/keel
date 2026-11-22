// Inventory → slot registry sync (W12).
//
// `GET /v1/plugins` is the one machine-readable truth for what is installed and
// enabled. This module is the only place that turns that inventory into slot
// declarations and contributions; there is no second installer and no second
// plugin registry. A plugin that is disabled or uninstalled is unregistered on
// the same sync, so its slots and panels disappear together.
//
// The shape read here is the documented one (`layer`, `provides.slots`,
// `contributes`, `extensibility`). The server may not carry the fields yet
// (W0.5 is landing them); the reader is tolerant, and a plugin without them
// simply contributes nothing.
import type { PluginsView } from "../data/contract";
import type { SlotRegistry } from "./registry";

interface ExtensionPluginView {
  id?: string;
  state?: string;
  tools?: string[];
  layer?: string;
  slots?: unknown[];
  contributes?: unknown[];
  contributions?: unknown[];
  extensibility?: { usable?: boolean; doc?: string; schema?: string; reason?: string };
  provides?: {
    tools?: string[];
    slots?: unknown[];
  };
}

export interface SyncResult {
  registered: string[];
  unregistered: string[];
  failed: string[];
}

/** The plugins the last sync managed, so a disable/uninstall can be seen. */
const applied = new Map<string, string>();
const tools = new Map<string, string[]>();
const disabled = new Set<string>();

/** Forget what the previous sync applied (tests, a tenant switch). */
export function resetPluginSync(): void {
  applied.clear();
  tools.clear();
  disabled.clear();
}

/** Read a plugin's slots from either the top-level field or `provides`. */
export function pluginSlots(plugin: ExtensionPluginView): unknown[] {
  if (Array.isArray(plugin.slots)) return plugin.slots;
  if (Array.isArray(plugin.provides?.slots)) return plugin.provides.slots;
  return [];
}

/** Read a plugin's contributions (the documented name, plus a tolerance alias
 * for an early field spelling). */
export function pluginContributions(plugin: ExtensionPluginView): unknown[] {
  if (Array.isArray(plugin.contributes)) return plugin.contributes;
  if (Array.isArray(plugin.contributions)) return plugin.contributions;
  return [];
}

/** The enabled plugins of the inventory, in a stable order (declarations are
 * registered before contributions by the two passes below). */
function enabledPlugins(view: PluginsView | null | undefined): ExtensionPluginView[] {
  const plugins = (view?.plugins ?? []) as ExtensionPluginView[];
  return plugins.filter((plugin) => {
    if (!plugin.id) return false;
    const state = String(plugin.state ?? "");
    return state === "enabled" || state === "active";
  });
}

/** Apply one inventory document. Returns what changed. */
export function syncPluginInventory(registry: SlotRegistry, view: PluginsView | null | undefined): SyncResult {
  const result: SyncResult = { registered: [], unregistered: [], failed: [] };
  const plugins = enabledPlugins(view);
  const present = new Set(plugins.map((plugin) => plugin.id ?? ""));

  // 1. Drop what is no longer enabled/present.
  for (const id of [...applied.keys()]) {
    if (present.has(id)) continue;
    registry.unregisterPlugin(id);
    applied.delete(id);
    tools.delete(id);
    disabled.add(id);
    result.unregistered.push(id);
  }

  // 2. Declarations first: a contributor scanned before its slot provider must
  // still find the slot. This pass touches slots only.
  for (const plugin of plugins) {
    const id = plugin.id ?? "";
    const slots = pluginSlots(plugin);
    if (slots.length === 0) continue;
    const outcome = registry.declareSlots(id, slots);
    if (!outcome.ok) result.failed.push(id);
  }

  // 3. Contributions, one plugin at a time. An unchanged declaration is not
  // re-registered (so its chunks are not reloaded on every refresh).
  for (const plugin of plugins) {
    const id = plugin.id ?? "";
    const slots = pluginSlots(plugin);
    const contributes = pluginContributions(plugin);
    tools.set(id, plugin.tools ?? plugin.provides?.tools ?? []);
    registry.setPluginTools(id, tools.get(id) ?? []);
    const signature = stableSignature(slots, contributes);
    if (applied.get(id) === signature) continue;
    const outcome = registry.registerPlugin({ pluginId: id, contributes });
    if (!outcome.ok) {
      result.failed.push(id);
      continue;
    }
    applied.set(id, signature);
    if (!result.registered.includes(id)) result.registered.push(id);
  }
  return result;
}

/** The signature compares what the client reads, not the whole manifest (a
 * timestamp or an unrelated field must not reload a chunk). */
function stableSignature(slots: unknown[], contributes: unknown[]): string {
  try {
    return JSON.stringify({ slots, contributes });
  } catch {
    return `${slots.length}:${contributes.length}`;
  }
}

/** The plugins the last sync dropped, for a screen that lists "removed". */
export function disabledPlugins(): string[] {
  return [...disabled];
}
