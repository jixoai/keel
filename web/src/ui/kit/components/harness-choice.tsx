// TUI harness · C2 choices — Switch / Checkbox / RadioGroup / Select /
// Combobox / Command, each driving a readout the TUI gate can assert on.
import { mount, signal, Text, View } from "nsh";
import { typeScale } from "../tokens";
import { Checkbox, Combobox, Command, Field, RadioGroup, Select, Switch } from "../index";

const retry = signal(true);
const remember = signal(false);
const effort = signal("中");
const model = signal<string | null>(null);
const picks = signal<string[]>([]);
const commandLog = signal("未选择");

const models = [
  { value: "flash", title: "deepseek-flash" },
  { value: "pro", title: "deepseek-pro" },
  { value: "claude", title: "claude-sonnet", disabled: true },
];

const commands = [
  { id: "deploy", label: "部署到测试环境", group: "发布" },
  { id: "rollback", label: "回滚上一个版本", group: "发布" },
  { id: "settings", label: "打开设置", shortcut: "mod+," },
];

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", gap: 8, padding: 8 }}>
      <Switch testID="switch-retry" label="自动重试" checked={retry()} onChange={(v) => retry.set(v)} />
      <Checkbox
        testID="check-remember"
        label="记住选择"
        checked={remember()}
        onChange={(v) => remember.set(v)}
      />
      <Checkbox testID="check-disabled" label="不可勾选" disabled />
      <RadioGroup
        testID="radio-effort"
        accessibilityLabel="思考档"
        options={[
          { value: "高", label: "高" },
          { value: "中", label: "中" },
          { value: "低", label: "低" },
        ]}
        value={effort()}
        onChange={(value) => effort.set(value)}
      />
      <Field label="模型">
        <Select
          testID="select-model"
          accessibilityLabel="选择模型"
          items={models}
          value={model()}
          onChange={(value) => model.set(value)}
          placeholder="请选择模型"
        />
      </Field>
      <Field label="多选模型">
        <Combobox
          testID="combobox-models"
          accessibilityLabel="多选模型"
          items={models}
          multiple
          value={picks()}
          onChange={(values) => picks.set(values)}
          placeholder="搜索模型"
        />
      </Field>
      <Command
        testID="command-palette"
        accessibilityLabel="命令面板"
        items={commands}
        onSelect={(id) => commandLog.set(`已选 ${id}`)}
      />
      <Text testID="readout-retry" style={{ fontSize: typeScale.sm }}>{`重试=${retry() ? "开" : "关"}`}</Text>
      <Text testID="readout-remember" style={{ fontSize: typeScale.sm }}>{`记住=${remember() ? "开" : "关"}`}</Text>
      <Text testID="readout-effort" style={{ fontSize: typeScale.sm }}>{`档位=${effort()}`}</Text>
      <Text testID="readout-model" style={{ fontSize: typeScale.sm }}>{`模型=${model() ?? "无"}`}</Text>
      <Text testID="readout-picks" style={{ fontSize: typeScale.sm }}>{`多选=${picks().join("/") || "无"}`}</Text>
      <Text testID="readout-command" style={{ fontSize: typeScale.sm }}>{commandLog()}</Text>
    </View>
  );
}

mount(Harness);
