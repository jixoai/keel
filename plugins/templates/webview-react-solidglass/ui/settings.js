// The scaffold's Native chunk: a contribution to keel.ui.settings-section.
//
// It runs in the host VM (no iframe, no second React tree) and draws with the
// host's components, so the plugin inherits the active theme. `render(context)`
// is the whole contract; keep this file self-contained (a runtime chunk is
// loaded as one module from /plugins/<id>/ui/settings.js).
import { Text, View } from "nsh";

export function render(context) {
  const title = context?.contribution?.title || context?.pluginId || "Plugin";
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
