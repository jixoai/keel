// KitGallery — the door every kit component passes before a screen may use it
// (Owner decision 2026-11-10: 组件必须先在 gallery 过门才允许进产品).
//
// One card per entry of `kit-manifest.tsx` (the spec's component list plus the
// layout blocks), three states each (default/disabled/error-or-empty), the
// component's public name in plain text so the gate and a screenshot reader can
// assert coverage. The page is one list, responsive to the window width: the
// gate photographs it at 390/768/1280.
//
// The components come from `kit-registry.generated.tsx`, which
// `gen-registry.mjs` writes from web/src/ui/kit — that is how the gallery keeps
// running while the kit lands in parallel: a name with no export yet renders as
// a "missing: <name>" card instead of failing the build. The gallery is a
// review entry only: it is not part of the product shell (web/App.tsx), and
// nothing in web/src imports it.
//
// Chrome (this file's own colours and sizes) reads the kit's tokens —
// `web/src/ui/kit/tokens.ts` — so the gallery never invents a second colour
// language: `color`, `space`, `radius`, `typeScale`, `weight`, `iconSize`,
// `alpha`.
import { Scroll, Text, View, windowSize } from "nsh";
import { Icon } from "../src/ui/icons";
import { alpha, color, iconSize, radius, space, typeScale, weight } from "../src/ui/kit/tokens";
import {
  kitBarrelPresent,
  kitComponentSources,
  kitComponentVia,
  kitComponents,
  kitDetected,
  kitUnlisted,
} from "./kit-registry.generated";
import { KIT_GROUPS, KIT_MANIFEST, type KitEntry, type KitNode, type KitState } from "./kit-manifest";

type AnyComponent = KitNode;

/** Sub-components for composite demos: what the registry has, or a visible
 * placeholder that keeps the card renderable while the piece is unlanded. */
function subComponent(name: string): AnyComponent {
  const value = kitComponents[name];
  if (typeof value === "function") return value as AnyComponent;
  const Missing = () => (
    <Text style={{ fontSize: typeScale["2xs"], color: color.danger }}>{`missing child: ${name}`}</Text>
  );
  return Missing as unknown as AnyComponent;
}


type Resolution =
  | { kind: "component"; name: string; component: AnyComponent }
  | { kind: "object"; name: string; keys: string[] }
  | { kind: "missing" };

/** The export behind an entry: its own name first, then its aliases. */
function resolveEntry(entry: KitEntry): Resolution {
  for (const candidate of [entry.name, ...(entry.aliases ?? [])]) {
    if (!Object.prototype.hasOwnProperty.call(kitComponents, candidate)) continue;
    const value = kitComponents[candidate];
    if (typeof value === "function") return { kind: "component", name: candidate, component: value as AnyComponent };
    const keys = typeof value === "object" && value !== null ? Object.keys(value as object).slice(0, 12) : [];
    return { kind: "object", name: candidate, keys };
  }
  return { kind: "missing" };
}

const TRIO: KitState[] = [
  { id: "default" },
  { id: "disabled", props: { disabled: true } },
  { id: "error", props: { error: "示例错误", invalid: true } },
];

const statesOf = (entry: KitEntry): KitState[] => entry.states ?? TRIO;

const cellProps = (entry: KitEntry, state: KitState): Record<string, unknown> => ({
  ...(entry.base ?? {}),
  ...(entry.compose ? entry.compose(subComponent) : {}),
  ...(state.props ?? {}),
});

// --- the responsive grid ----------------------------------------------------

const PAD = space["2xl"];
const GAP = space.lg;

const pageWidth = () => windowSize().width;
/** The terminal's grid is a few dozen cells wide: keep its padding tiny so a
 * card still fits (the TUI only has to render, the gate photographs the web). */
const pad = () => (pageWidth() < 300 ? space.xs : PAD);
const columns = (width: number) => (width < 620 ? 1 : width < 1000 ? 2 : 3);
const cardWidth = (width: number) => {
  const available = Math.max(0, width - pad() * 2);
  if (available <= 0) return Math.max(0, width);
  const columns_ = columns(width);
  const raw = Math.floor((available - GAP * (columns_ - 1)) / columns_);
  return Math.max(Math.min(180, available), Math.min(raw, available));
};

// --- the parts --------------------------------------------------------------

function Chip(props: { label: string; tone: "quiet" | "brand" | "warn" | "danger" | "ok" }) {
  const face = () =>
    props.tone === "brand"
      ? color.brand
      : props.tone === "warn"
        ? color.warning
        : props.tone === "danger"
          ? color.danger
          : props.tone === "ok"
            ? color.success
            : color.mutedForeground;
  return (
    <View
      style={{
        paddingHorizontal: space.sm,
        height: 16,
        borderRadius: radius.sm,
        justifyContent: "center",
        backgroundColor: alpha(face(), 0.16),
      }}
    >
      <Text style={{ fontSize: typeScale["2xs"], fontWeight: weight.semibold, color: face() }}>{props.label}</Text>
    </View>
  );
}

/** A state cell: the label first (it is the gate's text), then the render. */
function DemoCell(props: { entry: KitEntry; state: KitState; component: AnyComponent }) {
  const props_ = cellProps(props.entry, props.state);
  const C = props.component;
  return (
    <View style={{ gap: space.xs }}>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{`${props.entry.name} · ${props.state.id}`}</Text>
      <View
        style={{
          minHeight: 30,
          padding: space.md,
          gap: space.sm,
          borderRadius: radius.md,
          borderWidth: 1,
          borderColor: alpha(color.border, 0.7),
          backgroundColor: alpha(color.card, 0.5),
        }}
      >
        <C {...props_} />
      </View>
    </View>
  );
}

function PlaceholderCard(props: { entry: KitEntry }) {
  const resolution = resolveEntry(props.entry);
  const marker = () =>
    resolution.kind === "missing"
      ? `missing: ${props.entry.name}`
      : `compound: ${props.entry.name}（导出为对象，键：${resolution.kind === "object" ? resolution.keys.join(", ") : ""}）`;
  const face = () => (resolution.kind === "missing" ? color.danger : color.warning);
  return (
    <View
      style={{
        minHeight: 56,
        padding: space.md,
        gap: space.xs,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: alpha(face(), 0.55),
        backgroundColor: alpha(face(), 0.08),
        justifyContent: "center",
      }}
    >
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: face() }}>{marker()}</Text>
      <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>
        {resolution.kind === "missing"
          ? "web/src/ui/kit 还没有这个导出；落盘后跑 scripts/kit-gate.sh 重新生成 registry"
          : "gallery 只渲染函数导出，对象（复合件）请在 kit 单测里覆盖；此处列出成员名"}
      </Text>
    </View>
  );
}

function EntryCard(props: { entry: KitEntry }) {
  // The registry is generated at build time and never changes at runtime, so
  // resolving once per instance is enough; the styles below still read
  // windowSize() reactively for the grid.
  const resolved = resolveEntry(props.entry);
  return (
    <View
      style={{
        width: props.entry.wide ? Math.max(0, pageWidth() - pad() * 2) : cardWidth(pageWidth()),
        padding: space.lg,
        gap: space.lg,
        borderRadius: radius.lg,
        borderWidth: 1,
        borderColor: alpha(color.border, 0.5),
        backgroundColor: alpha(color.card, 0.55),
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>{props.entry.name}</Text>
        {props.entry.aliases && props.entry.aliases.length > 0 ? (
          <Chip label={`别名 ${props.entry.aliases.join(" / ")}`} tone="quiet" />
        ) : null}
        {resolved.kind === "component" ? <Chip label="gallery 已渲染" tone="ok" /> : null}
        {resolved.kind === "component" && kitComponentVia[resolved.name] === "direct" && kitBarrelPresent ? (
          <Chip label="未走 index 公共出口" tone="warn" />
        ) : null}
        {resolved.kind === "missing" ? <Chip label="缺件" tone="danger" /> : null}
      </View>

      {props.entry.note ? (
        <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>{props.entry.note}</Text>
      ) : null}

      {resolved.kind === "component" ? (
        <View style={{ gap: space.lg }}>
          {statesOf(props.entry).map((state) => (
            <DemoCell entry={props.entry} state={state} component={resolved.component} />
          ))}
          <Text style={{ fontSize: typeScale["2xs"], color: alpha(color.mutedForeground, 0.8) }}>
            {`来源 ${kitComponentSources[resolved.name] ?? "?"}（${kitComponentVia[resolved.name] ?? "?"}）`}
          </Text>
        </View>
      ) : (
        <PlaceholderCard entry={props.entry} />
      )}
    </View>
  );
}

function Header(props: { rendered: number; objects: number; missing: string[] }) {
  return (
    <View
      style={{
        padding: pad(),
        gap: space.sm,
        borderBottomWidth: 1,
        borderColor: alpha(color.border, 0.6),
        backgroundColor: alpha(color.panel, 0.85),
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Icon name="layers" size={iconSize.lg} tint={color.brand} />
        <Text style={{ fontSize: typeScale.lg, fontWeight: weight.bold, color: color.foreground }}>Keel UI Kit Gallery</Text>
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {`组件清单 ${KIT_MANIFEST.length} 件 · 已渲染 ${props.rendered} · 复合对象 ${props.objects} · 缺件 ${props.missing.length} · kit 导出 ${kitDetected.length} 件（清单内 ${kitDetected.length - kitUnlisted.length} · 其它 ${kitUnlisted.length}）`}
      </Text>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        {kitBarrelPresent
          ? "公共出口：web/src/ui/kit（index）"
          : "公共出口：web/src/ui/kit 尚无 index 桶文件（registry 逐模块直引）"}
        {" · 状态：每组 default / disabled / error（显示件为 default / empty）"}
      </Text>
      {props.missing.length > 0 ? (
        <Text style={{ fontSize: typeScale.xs, color: color.danger }}>
          {`缺件名单（${props.missing.length}）：${props.missing.join("、")}`}
        </Text>
      ) : null}
    </View>
  );
}

function Group(props: { label: string; entries: KitEntry[] }) {
  return (
    <View style={{ gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.bold, color: color.foreground }}>{props.label}</Text>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{`${props.entries.length} 件`}</Text>
        <View style={{ flex: 1, height: 1, backgroundColor: alpha(color.border, 0.5) }} />
      </View>
      <View style={{ flexDirection: "row", flexWrap: "wrap", gap: GAP }}>
        {props.entries.map((entry) => (
          <EntryCard entry={entry} />
        ))}
      </View>
    </View>
  );
}

/** kit 里存在、但清单（或别名）没要求的导出：只列名，不 import——一个没有被
 * 清单使用的模块坏掉时不该拖垮 gallery 的 build（`nsh check` 仍全量把关）。 */
function Unlisted(props: { names: string[] }) {
  return (
    <View style={{ gap: space.sm }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.bold, color: color.foreground }}>kit 其它导出（未列入清单）</Text>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{`${props.names.length} 件`}</Text>
        <View style={{ flex: 1, height: 1, backgroundColor: alpha(color.border, 0.5) }} />
      </View>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        {props.names.length > 0 ? props.names.join(" · ") : "（无）"}
      </Text>
    </View>
  );
}

/** Every entry the gallery renders. */
const ENTRIES = KIT_MANIFEST;

export function KitGallery() {
  const missing = () => ENTRIES.filter((entry) => resolveEntry(entry).kind === "missing").map((entry) => entry.name);
  const objects = () => ENTRIES.filter((entry) => resolveEntry(entry).kind === "object").length;
  const rendered = () => ENTRIES.filter((entry) => resolveEntry(entry).kind === "component").length;
  return (
    <View style={{ flex: 1, backgroundColor: color.background }}>
      <Header rendered={rendered()} objects={objects()} missing={missing()} />
      <Scroll style={{ flex: 1 }} contentStyle={{ padding: pad(), gap: space["2xl"] }}>
        {KIT_GROUPS.map((group) => (
          <Group label={group.label} entries={ENTRIES.filter((entry) => entry.group === group.id)} />
        ))}
        <Unlisted names={kitUnlisted} />
      </Scroll>
    </View>
  );
}
