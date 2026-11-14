// One block of the STT workbench's record: a finalized transcription, the live
// partial (dimmed, never persisted), a demo sample, or a system note.
//
// This is a transcription block, not a chat bubble: the block is left-aligned
// and full-width (a document rail on its left edge), its head carries the
// speaker (说话人), the input it came from (麦克风 / 文字输入) and
// the time, and its body is a paragraph. There is no avatar, no “my message”
// right alignment and no bubble tail anywhere — an STT pane writes down what
// was said; it does not answer.
//
// Rows are append-only: a final entry is never patched, so its getters are
// constants. The partial is the one row whose text changes, and it reads the
// signal through `props.text()` inside JSX. The source badge is the honest
// part: 麦克风 / 文字输入 are different facts and never share a
// label.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import {
  alpha,
  Badge,
  color,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../../ui/kit";
import { Icon, type IconName } from "../../../ui/icons";

export type TranscriptSource = "speech" | "text" | "file" | "note";

/** `14:22`-style clock, hours and minutes. */
const stamp = (at: number): string => {
  const date = new Date(at);
  const hh = String(date.getHours()).padStart(2, "0");
  const mm = String(date.getMinutes()).padStart(2, "0");
  return `${hh}:${mm}`;
};

interface SourceMeta {
  variant: BadgeVariant;
  label: string;
  icon: IconName;
}

const sourceMeta = (source: TranscriptSource, partial: boolean): SourceMeta => {
  if (partial) return { variant: "info", label: "转写中（未入账）", icon: "clock" };
  if (source === "text") return { variant: "ghost", label: "文字输入", icon: "keyboard" };
  if (source === "file") return { variant: "ghost", label: "音频文件", icon: "file" };
  return { variant: "ghost", label: "麦克风", icon: "message" };
};

/** The block's left rail: the live take is brand-coloured, a committed
 * transcription the neutral hairline. */
const railColor = (source: TranscriptSource, partial: boolean): string => {
  if (partial) return alpha(color.brand, 0.45);
  return color.border;
};

export function TranscriptRow(props: {
  /** The block's text, read inside JSX so the partial row stays live. */
  text: () => string;
  source: () => TranscriptSource;
  /** When the row was recorded; omitted for a partial. */
  at?: () => number;
  /** The live, not-yet-committed row. */
  partial?: boolean;
  testID?: string;
}): JSX.Element {
  const blockStyle = (): StyleProp<ViewStyle> => ({
    flexDirection: "row",
    gap: space.md,
    width: "100%",
    opacity: props.partial ? 0.7 : 1,
  });
  const meta = () => sourceMeta(props.source(), !!props.partial);
  const testID = () =>
    props.testID ?? (props.partial ? "speech-partial-row" : "speech-transcript-row");

  return (
    <If
      when={props.source() === "note"}
      else={
        <View testID={testID()} accessibilityLabel="转写记录" style={blockStyle()}>
          {/* The rail is the block's gutter — the one page-like cue an STT
              record uses; it is not a message bubble. */}
          <View
            style={{
              width: 2,
              alignSelf: "stretch",
              borderRadius: radius.pill,
              backgroundColor: railColor(props.source(), !!props.partial),
            }}
          />
          <View style={{ flex: 1, minWidth: 0, gap: space.xs }}>
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.sm,
                flexWrap: "wrap",
              }}
            >
              <Text
                style={{
                  fontSize: typeScale.sm,
                  fontWeight: weight.medium,
                  color: color.foreground,
                }}
              >
                说话人
              </Text>
              <Badge variant={meta().variant} icon={meta().icon} label={meta().label} />
              <If when={props.at !== undefined}>
                <Text
                  style={{
                    fontSize: typeScale.xs,
                    color: color.mutedForeground,
                    fontVariant: ["tabular-nums"],
                  }}
                >
                  {stamp(props.at!())}
                </Text>
              </If>
            </View>
            <Text
              style={{
                userSelect: "text",
                fontSize: typeScale.base,
                lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
                color: color.foreground,
              }}
            >
              {props.text()}
            </Text>
          </View>
        </View>
      }
    >
      {/* A system note belongs to the workbench, not to a speaker: one
          left-aligned muted line in the same document column. */}
      <View
        testID={props.testID ?? "speech-note-row"}
        style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}
      >
        <View style={{ paddingTop: 1 }}>
          <Icon name="info" size={12} tint={color.mutedForeground} />
        </View>
        <Text
          style={{
            flex: 1,
            fontSize: typeScale.xs,
            color: color.mutedForeground,
            lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          }}
        >
          {props.text()}
        </Text>
      </View>
    </If>
  );
}
