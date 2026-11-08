// The one destructive confirmation: a centered dialog over a scrim, with two
// proper buttons (取消 secondary, 删除 danger). It is rendered at the app root
// so it covers both the sidebar and the conversation, and it never pushes the
// list around.
import { signal, Text, View } from "nsh";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import { PillButton, Press } from "../ui/primitives";
import { alpha, colors, glass, radius } from "../ui/theme";

interface DeleteRequest {
  sessionId: string;
  title: string;
}

export const deleteRequest = signal<DeleteRequest | null>(null);

/** The session a delete dialog is waiting on, if any. */
export const pendingDeleteId = () => deleteRequest()?.sessionId ?? null;

export const requestDelete = (sessionId: string, title: string) =>
  deleteRequest.set({ sessionId, title });

export function DeleteDialog() {
  const request = () => deleteRequest();
  const close = () => deleteRequest.set(null);
  const confirm = () => {
    const item = request();
    deleteRequest.set(null);
    if (item) void store.deleteSession(item.sessionId);
  };
  return (
    <If when={!!request()}>
      <View
        style={{
          position: "absolute",
          top: 0,
          left: 0,
          right: 0,
          bottom: 0,
          zIndex: 30,
          alignItems: "center",
          justifyContent: "center",
          padding: 24,
        }}
      >
        <Press
          onPress={close}
          accessibilityLabel="取消删除"
          activeStyle={{ opacity: 1 }}
          hoverStyle={{ opacity: 1 }}
          style={{
            position: "absolute",
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: alpha(colors.background, 0.68),
          }}
        >
          <View style={{ flex: 1 }} />
        </Press>
        <View
          accessibilityLabel="删除会话确认"
          style={{
            width: "100%",
            maxWidth: 384,
            borderRadius: radius.card,
            ...glass.popover,
            backgroundColor: colors.popover,
            padding: 18,
            gap: 14,
          }}
        >
          <View style={{ flexDirection: "row", alignItems: "center", gap: 10 }}>
            <View
              style={{
                width: 32,
                height: 32,
                borderRadius: 10,
                alignItems: "center",
                justifyContent: "center",
                backgroundColor: colors.dangerTint,
              }}
            >
              <Icon name="trash" size={15} tint={colors.danger} />
            </View>
            <Text style={{ flex: 1, fontSize: 15, fontWeight: "700", color: colors.foreground }}>删除会话？</Text>
          </View>
          <Text style={{ fontSize: 12.5, lineHeight: 19, color: colors.mutedForeground }}>
            「{request()?.title ?? "会话"}」及其消息将被删除，此操作不可撤销。
          </Text>
          <View style={{ flexDirection: "row", justifyContent: "flex-end", gap: 8 }}>
            <PillButton label="取消" tone="secondary" onPress={close} />
            <PillButton label="删除" tone="destructive" onPress={confirm} />
          </View>
        </View>
      </View>
    </If>
  );
}
