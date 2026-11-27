// Layout harness for the generated command form (change keel-plugin-commands
// 4.5). It mounts the real `CommandForm` with a representative three-field
// schema so a 390-wide terminal can be measured; the form's execution path is
// covered end to end in commands.tui.test.tsx.
import { mount, View } from "nsh";
import type { CommandView } from "../data/contract";
import { CommandForm } from "./composer-commands";

const command: CommandView = {
  name: "demo.workflow:plan",
  source: "plugin",
  summary: "把目标拆成可执行的计划",
  argsSchema: {
    type: "object",
    required: ["goal"],
    properties: {
      goal: { type: "string", description: "要拆解的目标" },
      depth: { type: "integer", minimum: 1, maximum: 9, default: 3 },
      dryRun: { type: "boolean", default: false },
    },
  },
  danger: "write",
  idempotent: false,
  targets: ["web", "native", "tui"],
  requiredPermissions: ["session.write", "plugin.invoke"],
  available: true,
};

mount(() => (
  <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
    <CommandForm
      command={command}
      values={{ goal: "", depth: "3", dryRun: false }}
      errors={{}}
      executing={false}
      onChange={() => {}}
      onExecute={() => {}}
      onCancel={() => {}}
    />
  </View>
));
