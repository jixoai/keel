// TUI harness · C1 basics — mounts the kit's base components so
// `basic.test.tsx` can render them on the terminal host.
//
// Not a product screen: it exists so every C1 component is exercised in the TUI
// gate (pressable, disabled, invalid, empty) with stable testIDs.
import { mount, signal, Text, View } from "nsh";
import { typeScale } from "../tokens";
import { Avatar, Badge, Button, Kbd, Separator, Skeleton, Spinner } from "../index";

const count = signal(0);

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", gap: 6, padding: 8 }}>
      <Button testID="btn-default" label="主按钮" onPress={() => count.update((n) => n + 1)} />
      <Button testID="btn-secondary" variant="secondary" label="次按钮" />
      <Button testID="btn-outline" variant="outline" label="描边按钮" />
      <Button testID="btn-ghost" variant="ghost" label="幽灵按钮" />
      <Button testID="btn-destructive" variant="destructive" label="危险按钮" />
      <Button testID="btn-link" variant="link" label="链接按钮" />
      <Button testID="btn-disabled" variant="outline" label="禁用按钮" disabled />
      <Button testID="btn-invalid" variant="outline" label="非法按钮" invalid />
      <Button testID="btn-loading" label="加载中" loading />
      <Button testID="btn-large" size="lg" label="大按钮" />
      <Text testID="count-readout" style={{ fontSize: typeScale.sm }}>
        {`计数 ${count()}`}
      </Text>
      <View style={{ flexDirection: "row", gap: 6 }}>
        <Badge label="默认" />
        <Badge variant="secondary" label="次" />
        <Badge variant="destructive" label="危险" />
        <Badge variant="outline" label="描边" />
        <Badge variant="success" label="成功" />
      </View>
      <Separator label="分区" />
      <View style={{ flexDirection: "row", alignItems: "center", gap: 10 }}>
        <Kbd keys={["⌘", "K"]} />
        <Spinner size={12} />
        <Avatar name="Ada Lovelace" />
      </View>
      <Skeleton lines={2} />
    </View>
  );
}

mount(Harness);
