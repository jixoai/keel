// The ToolRow's MCP App slot (change feat-mcp-apps-events, P2.7/P2.8).
//
// It answers one question: this tool call carries an app binding — can this
// client, on this deployment, mount it? The answer is the capability gate
// (`can("mcp.apps")`, which is false on the terminal host) plus the catalogue
// (the binding's single source). Every failure has one face: a readable line,
// a retry, and the tool's text result still right below in `messages.tsx`.
// Nothing here can show a success the server did not produce.
import { effect, Text, View } from "nsh";
import type { McpAppBinding, ToolCallView } from "../../data/contract";
import { can } from "../../data/capabilities";
import { appBindingOf } from "../../data/mcp-apps";
import { store } from "../../data/store";
import { Icon } from "../../ui/icons";
import { Badge, Button, color, radius, space, typeScale } from "../../ui/kit";
import { appCatalogueEntry, appCatalogueState, ensureAppCatalogue } from "./catalogue";
import { McpAppView } from "./view";

/** The binding a call carries, from the complete `tool.result` (or, when the
 * adapter has not indexed it, from the projection's one `result` value). */
export function bindingOf(call: ToolCallView): McpAppBinding | null {
  const raw = store.toolResultFor(call.toolCallId);
  return appBindingOf(raw) ?? appBindingOf(call.result);
}

function Unsupported(props: { binding: McpAppBinding }) {
  return (
    <View
      testID={`mcp-app-unsupported-${props.binding.appId}`}
      accessibilityLabel="此结果带交互界面"
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.muted,
      }}
    >
      <Icon name="panel" size={14} tint={color.mutedForeground} />
      <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>
        此结果带交互界面；当前客户端以文本显示。
      </Text>
      <Badge variant="secondary" label="文本" />
    </View>
  );
}

function CatalogueFallback(props: { callId: string; reason: string; retry: () => void }) {
  return (
    <View
      testID={`mcp-app-fallback-${props.callId}`}
      accessibilityLabel="交互界面不可用"
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.muted,
      }}
    >
      <Icon name="alert" size={14} tint={color.warning} />
      <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground, userSelect: "text" }}>
        {props.reason}（文本结果仍可阅读）
      </Text>
      <Button
        variant="outline"
        size="sm"
        icon="refresh"
        label="重试"
        accessibilityLabel="重试加载交互界面"
        onPress={props.retry}
      />
    </View>
  );
}

function Loading(props: { callId: string }) {
  return (
    <View
      testID={`mcp-app-loading-${props.callId}`}
      accessibilityLabel="正在载入交互界面"
      style={{ flexDirection: "row", alignItems: "center", gap: space.sm, padding: space.md }}
    >
      <Icon name="clock" size={14} tint={color.mutedForeground} />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在载入交互界面…</Text>
    </View>
  );
}

/** The app area of one tool row. Mounted only when the call has a binding. */
export function McpAppSlot(props: { call: ToolCallView }) {
  const binding = () => bindingOf(props.call);
  const catalogue = () => appCatalogueState();
  const entry = () => {
    const current = binding();
    return current ? appCatalogueEntry(current.appId) : undefined;
  };

  effect(() => {
    const current = binding();
    if (current && can("mcp.apps")) ensureAppCatalogue(current.appId);
  });

  const retry = () => {
    const current = binding();
    if (current) ensureAppCatalogue(current.appId, true);
  };

  const fallbackReason = () => {
    const state = catalogue();
    if (state.status === "error") return `交互界面目录读取失败：${state.error ?? "未知原因"}`;
    if (binding() && !entry()) return "目录中没有这个交互界面（服务器可能已移除）";
    return "交互界面不可用";
  };

  const catalogueFallbackOrLoading = () => (
    <If
      when={catalogue().status === "loading"}
      else={<CatalogueFallback callId={props.call.toolCallId} reason={fallbackReason()} retry={retry} />}
    >
      <Loading callId={props.call.toolCallId} />
    </If>
  );

  const readyOrFallback = () => (
    <If when={entry()} else={catalogueFallbackOrLoading()}>
      {(app) => (
        <McpAppView
          callId={props.call.toolCallId}
          app={app()}
          args={props.call.args}
          theme={catalogue().theme}
          sandboxOrigin={catalogue().sandboxOrigin}
        />
      )}
    </If>
  );

  return (
    <If when={!!binding()} else={null}>
      {() => (
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <If when={!can("mcp.apps")} else={readyOrFallback()}>
            {() => <Unsupported binding={binding() as McpAppBinding} />}
          </If>
        </View>
      )}
    </If>
  );
}
