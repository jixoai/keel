// Realtime surface · TranscriptRow — one row per final transcript, plus the
// live partial row a final replaces.
//
// Three shapes: the user's own speech/text (right aligned), the assistant's
// reply (a card with its status, any tool request the provider made, and its
// audio playback state), and a centered system line for call facts (reconnect,
// closure, errors). Partial rows are dimmed and labelled; they are UI only and
// are replaced in place by the final row that follows.
import { Text, View, type LengthValue } from "nsh";
import {
  alpha,
  Badge,
  Card,
  color,
  iconSize,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../../ui/kit";
import { Icon } from "../../../ui/icons";
import { AssistantAudioState, type AssistantAudioPhase } from "./assistant-audio-state";

export type UserSource = "speech" | "text";
export type AssistantRowStatus = "thinking" | "streaming" | "final" | "interrupted";
export type ToolCallStatus = "requested" | "done" | "aborted";

export interface ToolCallView {
  id: string;
  name: string;
  args: string;
  status: ToolCallStatus;
}

export const stamp = (at: number): string => {
  const date = new Date(at);
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
};

const ASSISTANT_STATUS: Record<AssistantRowStatus, { label: string; variant: BadgeVariant }> = {
  thinking: { label: "思考中", variant: "info" },
  streaming: { label: "说话中", variant: "info" },
  final: { label: "已完成", variant: "success" },
  interrupted: { label: "已打断", variant: "warning" },
};

const TOOL_STATUS: Record<ToolCallStatus, { label: string; variant: BadgeVariant }> = {
  requested: { label: "已请求", variant: "info" },
  // The panel never sends `function_call_output`: the arguments are generated,
  // not executed. "参数已到齐" would read as "about to run".
  done: { label: "参数已生成", variant: "success" },
  aborted: { label: "已中止", variant: "warning" },
};

function ToolCallCard(props: { tool: ToolCallView }) {
  const meta = () => TOOL_STATUS[props.tool.status];
  return (
    <View
      testID="realtime-tool-row"
      accessibilityLabel={`工具调用 ${props.tool.name}`}
      style={{
        gap: space.xxs,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.muted,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Icon name="tool" size={iconSize.sm} tint={color.mutedForeground} />
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
          {props.tool.name}
        </Text>
        <Badge variant={meta().variant} label={meta().label} />
      </View>
      <If when={!!props.tool.args}>
        <Text
          style={{
            numberOfLines: 4,
            fontSize: typeScale.xs,
            lineHeight: Math.round(typeScale.xs * lineHeight.normal),
            color: color.mutedForeground,
          }}
        >
          {props.tool.args}
        </Text>
      </If>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        本面板只展示实时语音模型的工具请求：不执行，也不回传结果。
      </Text>
    </View>
  );
}

export function UserTranscriptRow(props: {
  text: string;
  at: number;
  source: UserSource;
  partial?: boolean;
  maxWidth: LengthValue;
  testID?: string;
}) {
  const badge = () => {
    // "仅界面", not "未入账": this build writes no ledger entry even for the
    // final rows, so the partial cannot claim the final is booked anywhere.
    if (props.partial) return { label: "转写中（仅界面）", variant: "info" as BadgeVariant };
    return {
      label: props.source === "speech" ? "实时转写" : "文字输入",
      variant: "ghost" as BadgeVariant,
    };
  };
  return (
    <View
      testID={props.testID ?? "realtime-user-row"}
      style={{ alignItems: "flex-end", gap: space.xs, opacity: props.partial ? 0.62 : 1 }}
    >
      <Card
        size="sm"
        style={{
          maxWidth: props.maxWidth,
          borderBottomRightRadius: radius.sm,
          paddingHorizontal: space.lg,
          paddingVertical: space.md,
          gap: space.none,
          ...(props.partial ? { borderColor: alpha(color.brand, 0.35) } : null),
        }}
      >
        <Text
          style={{
            userSelect: "text",
            fontSize: typeScale.base,
            lineHeight: Math.round(typeScale.base * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.text}
        </Text>
      </Card>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Badge
          variant={badge().variant}
          icon={props.source === "speech" ? "message" : "keyboard"}
          label={badge().label}
        />
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{stamp(props.at)}</Text>
      </View>
    </View>
  );
}

export function AssistantTranscriptRow(props: {
  text: string;
  at: number;
  status: AssistantRowStatus;
  tools: ToolCallView[];
  audio: AssistantAudioPhase;
  outputLevel: number;
  maxWidth: LengthValue;
  testID?: string;
}) {
  const meta = () => ASSISTANT_STATUS[props.status];
  return (
    <Card
      size="sm"
      testID={props.testID ?? "realtime-assistant-row"}
      // `alignSelf: flex-start` keeps a short reply a short card: the row is
      // content-sized inside the conversation column, never a full-width slab.
      style={{ alignSelf: "flex-start", maxWidth: props.maxWidth, gap: space.md }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Icon name="spark" size={iconSize.sm} tint={color.brand} />
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>Keel</Text>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{stamp(props.at)}</Text>
        <Badge variant={meta().variant} label={meta().label} />
      </View>
      <If
        when={!!props.text}
        else={
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {props.status === "interrupted" ? "（已打断，未生成内容）" : "正在准备回复…"}
          </Text>
        }
      >
        <Text
          style={{
            userSelect: "text",
            fontSize: typeScale.base,
            lineHeight: Math.round(typeScale.base * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.text}
        </Text>
      </If>
      {props.tools.map((tool) => ToolCallCard({ tool }))}
      <If when={props.status === "interrupted"}>
        <Text testID="realtime-interrupted-note" style={{ fontSize: typeScale.xs, color: color.warning }}>
          已停止生成与播放，不会补全成本轮完整回复。
        </Text>
      </If>
      <If when={props.audio !== "idle"}>
        <AssistantAudioState phase={props.audio} level={props.outputLevel} />
      </If>
    </Card>
  );
}

export function SystemLine(props: { text: string; at: number; testID?: string }) {
  return (
    <View
      testID={props.testID ?? "realtime-system-row"}
      style={{ alignItems: "center", paddingVertical: space.xxs }}
    >
      <Text
        style={{
          textAlign: "center",
          fontSize: typeScale.xs,
          lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          color: color.mutedForeground,
        }}
      >
        {`${props.text} · ${stamp(props.at)}`}
      </Text>
    </View>
  );
}
