// The scaffold's Native chunk: a contribution to keel.ui.settings-section.
//
// It runs in the host VM (no iframe, no second React tree) and draws with the
// host's components, so the plugin inherits the active theme. `render(context)`
// is the whole contract.
//
// A runtime chunk is one self-contained module fetched from
// /plugins/<id>/ui/settings.js and imported inside the host's realm. In a
// published (linked) web build there is no import map for the app's native
// modules, so a bare host-module import cannot resolve: the chunk must read the
// host realm's module namespace instead. The same namespace the host uses is
// reachable here, which keeps the chunk on the host tree without a second design
// system.
const hostRealm = typeof globalThis === "object" ? globalThis.__nsh_browser_realm : undefined;
const hostModules = hostRealm && hostRealm.modules;
const nsh = (hostModules && typeof hostModules.get === "function" && hostModules.get("nsh")) || {};
const Text = nsh.Text;
const View = nsh.View;

export function render(context) {
  const title = context?.contribution?.title || context?.pluginId || "Plugin";
  // A host without the NSH realm (a non-browser target that assembled this
  // chunk by another route) has no host components to draw with. Returning
  // null makes the slot show its explicit unavailable face instead of throwing.
  if (typeof Text !== "function" || typeof View !== "function") return null;
  return View({
    testID: `plugin-chunk-${context?.contribution?.id ?? "settings"}`,
    style: { padding: 8, gap: 4 },
    children: [
      Text({
        style: { fontSize: 13, fontWeight: 600 },
        children: title,
      }),
      Text({
        style: { fontSize: 12 },
        children: `由 ${context?.pluginId ?? "plugin"} 的 chunk 贡献渲染`,
      }),
    ],
  });
}
