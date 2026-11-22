// The top band's plugin slot with no contribution: it must render no hole.
import { mount, View } from "nsh";
import { PluginTopPanels } from "./plugin-top-panels";

mount(() => (
  <View style={{ flex: 1, minHeight: 0 }}>
    <PluginTopPanels />
  </View>
));
