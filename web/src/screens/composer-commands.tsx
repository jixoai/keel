// The Composer's `/` command menu and the args_schema form
// (change keel-plugin-commands, tasks 4.1/4.4).
//
// The catalog is the server's (`store.commandCatalog()`): this file never keeps
// a second list of commands. The menu draws the same order the server published
// (builtin → plugin → skill) and keeps an unavailable command visible with its
// reason — a command that cannot run is explained, not silently hidden.
//
// The form is generated from `args_schema` with a closed subset (string,
// number/integer, boolean, enum, array of primitives, and a JSON field for
// anything else). A schema with exactly one `text` string property does not get
// a form: the rest of the line is that argument (design §7). Chrome is the kit:
// Card/Field/Input/Select/Switch/Textarea/Badge/Button and tokens only.
import { signal, Text, View } from "nsh";
import type { CommandView } from "../data/contract";
import {
  commandAvailabilityText,
  commandDangerLabel,
  commandGroups,
  commandSourceLabel,
  commandFields,
  matchingCommands,
  type CommandField,
  type CommandValues,
} from "../data/commands";
import { store } from "../data/store";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  color,
  Field,
  iconSize,
  Input,
  lineHeight,
  Select,
  space,
  Switch,
  Textarea,
  typeScale,
  weight,
} from "../ui/kit";
import { Icon } from "../ui/icons";

/** A launch requested from outside the Composer (the Mod+K palette). The
 * nonce makes two launches of the same command distinct. */
export const commandLaunch = signal<{ name: string; nonce: number } | null>(null);

export function launchCommand(name: string): void {
  commandLaunch.set({ name, nonce: Date.now() });
}

/** Why the menu is showing (the store owns the facts). */
function menuBusy(): boolean {
  return store.commandCatalogLoading();
}

function menuError(): string | null {
  return store.commandCatalogError();
}

function menuCatalog() {
  return store.commandCatalog();
}

/** The retry a failed catalog read offers; the same entry the palette uses. */
export function retryCommandCatalog(): void {
  void store.loadCommands({ force: true });
}

/**
 * The Composer's command menu. `query` is the text after the leading '/';
 * `onPick` receives the chosen catalog row (the caller decides execute vs form).
 */
export function CommandMenu(props: {
  query: string;
  busy?: boolean;
  onPick: (command: CommandView) => void;
}) {
  const rows = () => matchingCommands(menuCatalog()?.commands ?? [], props.query);
  const groups = () => commandGroups(rows());
  const total = () => rows().length;
  const truncated = () => !!menuCatalog()?.truncated;
  const warnings = () => menuCatalog()?.warnings ?? [];
  return (
    <Card size="sm" testID="composer-commands" accessibilityLabel="命令菜单">
      <CardHeader
        title="命令"
        description="输入 / 选择命令；不可用的命令会写明原因。"
        action={
          <If when={!!menuCatalog() && total() > 0}>
            <Badge
              variant="secondary"
              label={`${total()} 条`}
              accessibilityLabel={`命令 ${total()} 条`}
              testID="composer-commands-count"
            />
          </If>
        }
      />
      <CardContent>
        <If when={menuBusy() && !menuCatalog()}>
          <Text testID="composer-commands-loading" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            正在读取命令目录…
          </Text>
        </If>
        <If when={!!menuError()}>
          <View style={{ gap: space.sm }} testID="composer-commands-error">
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
              <Icon name="alert" size={iconSize.xs} tint={color.destructive} />
              <Text
                style={{
                  flex: 1,
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.destructive,
                }}
              >
                {menuError() ?? ""}
              </Text>
            </View>
            <View style={{ flexDirection: "row" }}>
              <Button
                size="sm"
                variant="outline"
                label="重试"
                accessibilityLabel="重试读取命令目录"
                testID="composer-commands-retry"
                onPress={() => retryCommandCatalog()}
              />
            </View>
          </View>
        </If>
        <If
          when={
            !menuBusy() &&
            !menuError() &&
            !!menuCatalog() &&
            menuCatalog()!.commands.length === 0
          }
        >
          <Text testID="composer-commands-empty" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            当前会话没有可用命令。
          </Text>
        </If>
        <If when={warnings().length > 0}>
          <Text testID="composer-commands-warning" style={{ fontSize: typeScale.xs, color: color.warning }}>
            {`目录不完整：${warnings().join("、")}`}
          </Text>
        </If>
        <View style={{ gap: space.md }}>
          {groups().map((group) => (
            <View testID={`composer-commands-group-${group.id}`} style={{ gap: space.xs }}>
              <Text
                style={{
                  fontSize: typeScale.xs,
                  fontWeight: weight.semibold,
                  color: color.mutedForeground,
                }}
              >
                {group.label}
              </Text>
              {group.commands.map((command) => {
                const reason = () => commandAvailabilityText(command);
                return (
                  <View
                    testID={`composer-commands-row-${command.name}`}
                    style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}
                  >
                    <Button
                      size="sm"
                      variant="ghost"
                      label={`/${command.name}`}
                      testID={`composer-commands-pick-${command.name}`}
                      disabled={!command.available || !!props.busy}
                      accessibilityLabel={
                        command.available
                          ? `执行命令 /${command.name}`
                          : `命令 /${command.name} 不可用：${reason()}`
                      }
                      onPress={() => props.onPick(command)}
                    />
                    <Badge
                      variant="outline"
                      label={commandSourceLabel(command.source)}
                      accessibilityLabel={`来源 ${commandSourceLabel(command.source)}`}
                    />
                    <If when={command.danger === "destructive"}>
                      <Badge
                        variant="warning"
                        label={commandDangerLabel(command.danger)}
                        accessibilityLabel={commandDangerLabel(command.danger)}
                      />
                    </If>
                    <If when={command.idempotent}>
                      <Badge variant="secondary" label="幂等" accessibilityLabel="幂等" />
                    </If>
                    <Text
                      style={{
                        flex: 1,
                        fontSize: typeScale.xs,
                        lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                        color: command.available ? color.mutedForeground : color.warning,
                      }}
                    >
                      {`${command.summary}${reason() ? ` · ${reason()}` : ""}`}
                    </Text>
                  </View>
                );
              })}
            </View>
          ))}
        </View>
        <If when={truncated()}>
          <Text testID="composer-commands-truncated" style={{ fontSize: typeScale.xs, color: color.warning }}>
            目录超过上限，只显示前 256 条。
          </Text>
        </If>
      </CardContent>
    </Card>
  );
}

/** One form control per schema field; the label/error stack is the kit Field. */
function CommandFieldControl(props: {
  field: CommandField;
  value: string | boolean;
  error?: string;
  disabled?: boolean;
  onChange: (value: string | boolean) => void;
}) {
  const field = () => props.field;
  const value = () => props.value;
  const stringValue = (): string => {
    const current = value();
    return typeof current === "string" ? current : "";
  };
  const selectValue = (): string | null => {
    const current = value();
    return typeof current === "string" ? current : null;
  };
  const testID = () => `command-field-${field().name}`;
  return (
    <Field
      label={field().label}
      required={field().required}
      hint={field().hint}
      error={props.error}
      disabled={props.disabled}
      testID={`command-field-${field().name}-field`}
    >
      <If
        when={field().kind === "boolean"}
        else={
          <If
            when={field().kind === "enum"}
            else={
              <If
                when={field().kind === "array" || field().kind === "json" || !!field().multiline}
                else={
                  <Input
                    testID={testID()}
                    value={stringValue()}
                    disabled={props.disabled}
                    invalid={!!props.error}
                    placeholder={field().kind === "number" || field().kind === "integer" ? "数字" : ""}
                    accessibilityLabel={field().label}
                    onChange={(next) => props.onChange(next)}
                  />
                }
              >
                <Textarea
                  testID={testID()}
                  value={stringValue()}
                  disabled={props.disabled}
                  invalid={!!props.error}
                  rows={field().kind === "json" ? 4 : Math.min(6, Math.max(2, String(value()).split("\n").length))}
                  placeholder={
                    field().kind === "array"
                      ? "每行一项"
                      : field().kind === "json"
                        ? '{"…": "…"}'
                        : ""
                  }
                  accessibilityLabel={field().label}
                  onChange={(next) => props.onChange(next)}
                />
              </If>
            }
          >
            <Select
              testID={testID()}
              items={(field().options ?? []).map((option) => ({ value: option.value, title: option.label }))}
              value={selectValue()}
              disabled={props.disabled}
              invalid={!!props.error}
              placeholder="请选择"
              accessibilityLabel={field().label}
              onChange={(next) => props.onChange(next ?? "")}
            />
          </If>
        }
      >
        <Switch
          testID={testID()}
          checked={value() === true}
          disabled={props.disabled}
          label={value() === true ? "开" : "关"}
          accessibilityLabel={field().label}
          onChange={(next) => props.onChange(next)}
        />
      </If>
    </Field>
  );
}

/**
 * The generated argument form. `errors` is keyed by field name; the execute
 * action is the card's, so Enter in a text field (which submits nothing here)
 * never sends a half-formed command by accident.
 */
export function CommandForm(props: {
  command: CommandView;
  values: CommandValues;
  errors: Record<string, string>;
  /** A whole-command failure (permission, busy, refusal, transport). */
  error?: string;
  executing: boolean;
  onChange: (field: string, value: string | boolean) => void;
  onExecute: () => void;
  onCancel: () => void;
}) {
  const fields = () => commandFields(props.command);
  return (
    <Card size="sm" testID="composer-command-form" accessibilityLabel={`命令 /${props.command.name} 参数`}>
      <CardHeader
        title={`/${props.command.name}`}
        description={props.command.summary}
        action={
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
            <Badge variant="outline" label={commandSourceLabel(props.command.source)} />
            <If when={props.command.danger === "destructive"}>
              <Badge variant="warning" label={commandDangerLabel(props.command.danger)} />
            </If>
          </View>
        }
      />
      <CardContent>
        <View style={{ gap: space.md }}>
          {fields().map((field) => (
            <CommandFieldControl
              field={field}
              value={props.values[field.name] ?? (field.kind === "boolean" ? false : "")}
              error={props.errors[field.name]}
              disabled={props.executing}
              onChange={(value) => props.onChange(field.name, value)}
            />
          ))}
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Button
              size="sm"
              variant="default"
              label={props.executing ? "执行中…" : "执行命令"}
              loading={props.executing}
              disabled={props.executing}
              accessibilityLabel={`执行命令 /${props.command.name}`}
              testID="composer-command-execute"
              onPress={() => props.onExecute()}
            />
            <Button
              size="sm"
              variant="ghost"
              label="取消"
              disabled={props.executing}
              accessibilityLabel="取消命令"
              testID="composer-command-cancel"
              onPress={() => props.onCancel()}
            />
            <Text
              style={{
                flex: 1,
                fontSize: typeScale.xs,
                lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                color: color.mutedForeground,
              }}
            >
              {props.command.danger === "destructive"
                ? "执行前会弹出审批；拒绝或超时都不会产生副作用。"
                : "参数只进入本次命令，不会变成对话消息。"}
            </Text>
          </View>
          <If when={!!props.error}>
            <View
              testID="composer-command-error"
              style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}
            >
              <Icon name="alert" size={iconSize.xs} tint={color.destructive} />
              <Text
                style={{
                  flex: 1,
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.destructive,
                }}
              >
                {props.error ?? ""}
              </Text>
            </View>
          </If>
        </View>
      </CardContent>
    </Card>
  );
}
