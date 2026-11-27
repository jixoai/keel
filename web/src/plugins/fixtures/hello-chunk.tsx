// The W12 fixture plugin's chunk: a Native contribution rendered by the host
// tree (web, native and TUI alike).
//
// It draws with the host kit's tokens only — no colour, radius, shadow or font
// of its own — and it works without the bridge, so a terminal (no host.call)
// still proves the chunk form.
import type { JSX } from "nsh";
import { Text, View } from "nsh";
import { alpha, color, font, radius, space, typeScale, weight } from "../../ui/kit";
import type { PluginChunkContext } from "../types";

function caption(context: PluginChunkContext): string {
  const slot = context.contribution.slot;
  if (slot.endsWith(".top-band")) return "W12 插件条";
  if (slot.endsWith(".sidebar")) return "W12 插件侧栏";
  if (slot.endsWith(".settings-section")) return "W12 插件设置区";
  if (slot.endsWith(".action")) return "W12 插件动作";
  if (slot.endsWith(".tool-renderer")) return "W12 工具渲染器";
  if (slot.endsWith(".message-part")) return "W12 消息部件";
  if (slot.endsWith(".route")) return "W12 插件路由";
  if (slot.endsWith(".surface")) return "W12 插件表面";
  return "W12 插件贡献";
}

function detail(context: PluginChunkContext): string {
  if (context.contribution.slot.endsWith(".tool-renderer") && context.props) {
    const call = context.props as { name?: string };
    return `工具 ${call.name ?? "?"} 使用插件渲染器`;
  }
  return "由 chunk 贡献渲染 · 同宿主 VM";
}

/** The chunk's public entry: the host calls this inside a guarded getter. */
export function render(context: PluginChunkContext): JSX.Element {
  const slot = context.contribution.slot;
  const attachment = (
    context.props as { part?: { name?: string; mime?: string; size?: number; kind?: string } } | undefined
  )?.part;
  if (slot.endsWith(".message-part") && attachment) {
    return (
      <View
        testID={`plugin-chunk-${context.contribution.id}`}
        style={{
          minWidth: 0,
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          paddingHorizontal: space.sm,
          paddingVertical: space.xs,
          borderRadius: radius.sm,
          backgroundColor: color.brandTint,
          borderWidth: 1,
          borderColor: alpha(color.brand, 0.3),
        }}
      >
        <View style={{ width: 6, height: 6, borderRadius: radius.pill, backgroundColor: color.brand }} />
        <View style={{ minWidth: 0, flexDirection: "column", gap: 1 }}>
          <Text
            style={{
              fontSize: typeScale.sm,
              fontWeight: weight.semibold,
              color: color.foreground,
              numberOfLines: 1,
            }}
          >
            {`W12 附件预览 · ${attachment.name ?? "附件"}`}
          </Text>
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              color: color.mutedForeground,
              numberOfLines: 1,
            }}
          >
            {`${attachment.mime ?? "?"} · ${attachment.kind ?? "file"} · chunk`}
          </Text>
        </View>
      </View>
    );
  }
  if (slot.endsWith(".route") || slot.endsWith(".surface")) {
    return (
      <View
        testID={`plugin-chunk-${context.contribution.id}`}
        style={{
          flex: 1,
          minWidth: 0,
          padding: space.xl,
          gap: space.md,
          maxWidth: 720,
        }}
      >
        <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>
          {context.contribution.title ?? caption(context)}
        </Text>
        <Text
          style={{
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * 1.5),
            color: color.mutedForeground,
          }}
        >
          这个页面由插件的 chunk 贡献渲染：与宿主同一个 VM、同一套主题令牌，没有 iframe，也没有第二个设计系统。
        </Text>
        <View
          style={{
            padding: space.lg,
            gap: space.sm,
            borderRadius: radius.lg,
            backgroundColor: color.card,
            borderWidth: 1,
            borderColor: color.border,
          }}
        >
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
            原生路由
          </Text>
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              color: color.mutedForeground,
            }}
          >
            {`slot=${slot} · form=${context.contribution.form} · target=${context.target}`}
          </Text>
        </View>
      </View>
    );
  }
  return (
    <View
      testID={`plugin-chunk-${context.contribution.id}`}
      style={{
        minWidth: 0,
        flexShrink: 1,
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        paddingHorizontal: space.sm,
        paddingVertical: space.xxs,
        borderRadius: radius.sm,
        backgroundColor: color.glassControl,
        borderWidth: 1,
        borderColor: color.glassControlRim,
      }}
    >
      <View
        style={{
          width: 6,
          height: 6,
          borderRadius: radius.pill,
          backgroundColor: color.brand,
        }}
      />
      <View style={{ minWidth: 0, flexDirection: "column", gap: 1 }}>
        <Text
          style={{
            fontSize: typeScale.sm,
            fontWeight: weight.semibold,
            color: color.foreground,
            numberOfLines: 1,
          }}
        >
          {context.contribution.title ?? caption(context)}
        </Text>
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.xs,
            color: color.mutedForeground,
            numberOfLines: 1,
          }}
        >
          {detail(context)}
        </Text>
      </View>
    </View>
  );
}
