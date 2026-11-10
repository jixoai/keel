// TUI harness · C2 inputs — Field / Label / Input / Textarea with live readouts,
// so the TUI gate can type into them and read the app-owned values back.
import { mount, signal, Text, View } from "nsh";
import { typeScale } from "../tokens";
import { Field, FieldGroup, Input, Label, Textarea } from "../index";

const title = signal("");
const note = signal("");

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", gap: 10, padding: 8 }}>
      <FieldGroup>
        <Field label="会话标题" hint="用于列表显示" testID="field-title">
          <Input
            testID="input-title"
            placeholder="输入标题"
            value={title()}
            onChange={(value) => title.set(value)}
          />
        </Field>
        <Field label="备注" error="备注过长">
          <Input testID="input-note-err" placeholder="备注" invalid />
        </Field>
        <Field label="描述">
          <Textarea
            testID="input-desc"
            rows={2}
            placeholder="补充描述"
            value={note()}
            onChange={(value) => note.set(value)}
          />
        </Field>
      </FieldGroup>
      <Label label="只读字段" />
      <Input testID="input-readonly" value="已锁定" readonly />
      <Input testID="input-disabled" placeholder="不可用" disabled />
      <Text testID="readout-title" style={{ fontSize: typeScale.sm }}>{`标题=${title()}`}</Text>
      <Text testID="readout-note" style={{ fontSize: typeScale.sm }}>{`描述=${note()}`}</Text>
    </View>
  );
}

mount(Harness);
