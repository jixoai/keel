// The honest placeholder surface (change keel-session-surfaces-v1).
//
// `video` and `generic` are real registry answers: a video provider is a
// different medium from chat, and an unrecognized provider type must say "no
// dedicated panel" instead of pretending to be a chat model. Before this file
// existed the conversation panel fell through to the LLM shell for both, so a
// video session looked like a text conversation with an empty history — the one
// outcome the registry exists to prevent.
//
// What this file is: a single centered card that names the surface, states why
// it has no panel (verbatim descriptor reason when the registry supplies one),
// and gives exactly one way forward. It never draws a composer and never
// fabricates a result, so nothing here can be mistaken for a working session.
//
// The primary action is the settings route (`打开模型设置`): configuring a
// provider is what turns an unsupported medium into a usable one. The secondary
// (`返回对话`) only appears when the surface was opened as a client-local
// preview, where leaving is a real navigation.
import { Text, View, windowSize } from "nsh";
import {
  Button,
  color,
  ConversationColumn,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  space,
  typeScale,
} from "../../ui/kit";
import type { SurfaceDescriptor, SurfaceKind } from "../registry";

/** Why this kind has no panel. `descriptor.reason` wins when the registry wrote
 * one (an unknown provider type names itself); video is a known medium without
 * a shipped panel, so it gets its own sentence rather than an empty reason. */
export function unsupportedReason(kind: SurfaceKind, descriptor: SurfaceDescriptor): string {
  if (descriptor.reason) return descriptor.reason;
  if (kind === "video") {
    return "视频生成面板尚未提供：当前没有已接入的视频 Provider，所以这里不显示生成输入框，也不会生成任何占位结果。";
  }
  return "该会话类型尚无专用面板。";
}

export function UnsupportedSurface(props: {
  wide: boolean;
  kind: SurfaceKind;
  descriptor: SurfaceDescriptor;
  onOpenSettings: () => void;
  /** Present only for the client-local preview, where leaving is navigation. */
  onLeave?: () => void;
}) {
  return (
    <View style={{ flex: 1, minHeight: 0, justifyContent: "center" }} testID="unsupported-surface">
      <ConversationColumn rail={false} windowWidthPx={windowSize().width} testID="unsupported-column">
        <Empty accessibilityLabel={`${props.descriptor.label}暂无专用面板`} testID="unsupported-empty">
          <EmptyMedia icon={props.kind === "video" ? "play" : "info"} tone="neutral" />
          <View style={{ alignItems: "center", gap: space.sm, paddingHorizontal: space.xl }}>
            <EmptyTitle label={props.descriptor.label} testID="unsupported-title" />
            <EmptyDescription label={unsupportedReason(props.kind, props.descriptor)} testID="unsupported-reason" />
          </View>
          <EmptyContent>
            <Button
              testID="unsupported-open-settings"
              variant="default"
              size="lg"
              icon="settings"
              label="打开模型设置"
              accessibilityLabel="打开模型设置"
              onPress={() => props.onOpenSettings()}
              style={{ minHeight: 36 }}
            />
            <If when={!!props.onLeave}>
              <Button
                testID="unsupported-leave"
                variant="secondary"
                size="lg"
                icon="chevron-left"
                label="返回对话"
                accessibilityLabel="返回对话"
                onPress={() => props.onLeave?.()}
                style={{ minHeight: 36 }}
              />
            </If>
          </EmptyContent>
          <Text
            style={{
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * 1.5),
              color: color.mutedForeground,
              textAlign: "center",
            }}
          >
            也可以用顶栏的模型切换换回文本模型；跨类型的切换会新建会话，历史不会混用。
          </Text>
        </Empty>
      </ConversationColumn>
    </View>
  );
}
