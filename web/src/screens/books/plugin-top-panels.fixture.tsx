// The top band's plugin slot with one contribution — the shape a plugin will
// use once the UI contribution contract lands.
import { mount, Text, View } from "nsh";
import { PluginTopPanels, registerPluginTopPanel } from "./plugin-top-panels";

registerPluginTopPanel({
  id: "demo",
  label: "演示插件面板",
  node: <Text>插件面板内容</Text>,
});

mount(() => (
  <View style={{ flex: 1, minHeight: 0 }}>
    <PluginTopPanels />
  </View>
));
