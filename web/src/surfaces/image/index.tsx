// Image session surface (change keel-session-surfaces-v1, spec
// media-session-surface).
//
// What this file is: the session surface an `image` model opens — a prompt
// composer over a gallery of generation turns. On mount it asks the media
// gateway (`GET /v1/media/capabilities`) what it can serve:
//
//   - configured image provider → real turns through `POST
//     /v1/media/images/generate` (and `/edit` for a multi-turn edit). Results
//     are the provider's URLs and every asset carries the model that made it.
//   - no provider, no image capability, or an unreachable gateway → the
//     configuration state: what is missing, why, a "配置图像 Provider" action,
//     and example prompts that only fill the composer (never send). Nothing is
//     generated locally and nothing pretends a provider answered.
//
// States are the product states: checking, configuration required, generating
// with cancel (the cancel aborts the fetch), success, and a failure that keeps
// the prompt/params and retries with the gateway's sanitized message. A 503
// names the configuration step instead of pretending the provider answered.
// The Composer/Timeline split, the append-only gallery and the `parentAssetId`
// version chain are the only gallery semantics.
//
// Structure: `ComposerDock` → `ComposerSurface` (prompt, negative prompt, the
// declared schema params, generate/cancel), and `ConversationColumn` →
// `ScrollArea` (turns → assets). Both blocks come from the kit, so this file
// writes no column width, no inset and no colour. The only numbers it owns are
// its own limits (prompt growth, preview cap).
//
// Multi-turn editing: an asset can seed a new turn ("编辑"); its children carry
// `parentAssetId`, so the tile shows `v2 · 派生自 v1` and the preview dialog
// draws the whole chain as a breadcrumb. "重新生成" is a sibling by
// construction: it starts a fresh turn with the same prompt/params and no
// parent. Regeneration never rewrites history; the gallery is append-only.
//
// NSH discipline: components run once, so every signal is read inside JSX or an
// `<If>` (never copied into a body-level local), and render helpers take values,
// not accessors, because they are called from the one tracked expression that
// maps `turns()`.
import {
  cleanup,
  effect,
  Image,
  type InputRef,
  signal,
  Text,
  untracked,
  View,
  windowSize,
} from "nsh";
import { Input as NativeInput } from "nsh/base";
import {
  editImage,
  generateImages,
  MediaGatewayError,
  mediaCapabilities,
} from "../../data/media";
import {
  alpha,
  Badge,
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
  Button,
  Card,
  CardContent,
  CardHeader,
  Collapsible,
  CollapsibleTrigger,
  ComposerDock,
  ComposerSurface,
  ConversationColumn,
  Dialog,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Field,
  IconButton,
  Input,
  ScrollArea,
  Separator,
  Skeleton,
  Spinner,
  Tab,
  Tabs,
  Tooltip,
  color,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { type IconName, Icon } from "../../ui/icons";
import { store } from "../../data/store";
import { resolveSurface } from "../registry";

export type AspectRatio = "1:1" | "16:9" | "9:16";
type TurnKind = "generate" | "edit" | "regenerate";
type TurnStatus = "generating" | "done" | "failed" | "cancelled";
/** The gateway probe: `checking` until the answer arrives, `ready` only when
 * the gateway says an image provider is configured, `unconfigured` for every
 * other answer (no provider, no image capability, unreachable). */
type MediaState = "checking" | "ready" | "unconfigured";

const SAMPLE_PROMPTS = ["薄暮海岸，青绿与靛蓝的玻璃质感", "琥珀色几何棱镜与暖色磨砂光"];

/** The preview dialog's image ceiling (a portrait asset must not push the
 * dialog past the viewport). */
const PREVIEW_MAX_HEIGHT_PX = 420;
/** The prompt field grows to six relaxed lines, then scrolls. */
const PROMPT_LINES = 6;

interface GalleryAsset {
  id: string;
  src: string;
  name: string;
  /** 1 for a root sample, parent.version + 1 for an edit. */
  version: number;
  parentAssetId?: string;
  prompt: string;
  aspect: AspectRatio;
  turnId: string;
  /** The provider model that made this asset, when the gateway names one. */
  model?: string;
}

interface GalleryTurn {
  id: string;
  kind: TurnKind;
  status: TurnStatus;
  prompt: string;
  negative: string;
  aspect: AspectRatio;
  count: number;
  assets: GalleryAsset[];
  error?: string;
  editParentId?: string;
  editInstruction?: string;
  attempt: number;
  /** The provider model that served the turn, when the gateway names one. */
  model?: string;
  /** The gateway answered 503: the failure card offers configuration. */
  configRequired?: boolean;
}

const prompt = signal("");
const negative = signal("");
const negativeOpen = signal(false);
const aspect = signal<AspectRatio>("1:1");
const count = signal(2);
const editTarget = signal<string | null>(null);
const editInstruction = signal("");
const turns = signal<GalleryTurn[]>([]);
const busy = signal(false);
const activeTurn = signal<string | null>(null);
const previewId = signal<string | null>(null);
const configOpen = signal(false);
/** The gateway probe's answer. Nothing generates before it says `ready`. */
const mediaState = signal<MediaState>("checking");
/** Why the probe reached `unconfigured` (the server's note, or a neutral
 * reachability sentence): shown next to the configuration action. */
const mediaNote = signal("");
const promptHeight = signal(0);
const promptTick = signal(0);
/** Each turn's measured gallery width, so the tiles split it exactly. */
const gridWidths = signal<Record<string, number>>({});
/** The width of this surface's own pane (measured). */
const paneWidthPx = signal(0);
/** The shell's `wide` fact, the fallback until the pane has measured itself. */
const wideMode = signal(false);

let promptInput: InputRef | undefined;
/** The fetch a real generation is riding; cancel/unmount aborts it. */
let activeController: AbortController | undefined;
let sequence = 0;
let probed = false;

const nextId = (prefix: string): string => `${prefix}-${++sequence}`;

const promptFieldHeight = (): number => {
  const oneLine = Math.round(typeScale.base * lineHeight.relaxed) + space.xs;
  return Math.min(Math.max(promptHeight(), oneLine), oneLine * PROMPT_LINES);
};

const clip = (text: string, max: number): string =>
  text.length > max ? `${text.slice(0, Math.max(0, max - 1))}…` : text;

const aspectRatioOf = (value: AspectRatio): number =>
  value === "16:9" ? 16 / 9 : value === "9:16" ? 9 / 16 : 1;

const ready = (): boolean => mediaState() === "ready";

/** The provider-facing size for a declared aspect. The gateway takes a `WxH`
 * box; these are the standard square/landscape/portrait boxes, not a claim
 * about a model's own limits. */
const sizeForAspect = (value: AspectRatio): string =>
  value === "16:9" ? "1280x720" : value === "9:16" ? "720x1280" : "1024x1024";

/** The one-line provenance an asset shows: its model, or the gateway's
 * turn-level model. Never a made-up name. */
const modelLabel = (asset: GalleryAsset): string => asset.model ?? "Provider";

/** The reference-chip name a gallery asset gets in the preview/edit flow. */
const assetTag = (asset: GalleryAsset): string => `${modelLabel(asset)} v${asset.version}`;

const mediaErrorText = (error: unknown): string => {
  if (error instanceof MediaGatewayError) return error.message;
  if (error instanceof Error) return error.message;
  return String(error);
};

/** A probe reason that never leaks a URL or a key: the gateway's sanitized
 * note is kept; a transport failure gets one neutral sentence. */
const probeNote = (error: unknown): string =>
  error instanceof MediaGatewayError && error.kind === "notConfigured"
    ? error.message
    : "暂时无法连接图像服务，请确认服务可用后重试。";

/** Ask the gateway what it can serve. A configured image gateway unlocks
 * generation before the first turn; every other answer is the configuration
 * state with its reason. */
const probeMedia = async () => {
  mediaState.set("checking");
  mediaNote.set("");
  try {
    const capabilities = await mediaCapabilities();
    if (capabilities.configured && capabilities.image) {
      mediaState.set("ready");
      return;
    }
    mediaState.set("unconfigured");
    mediaNote.set(
      capabilities.configured
        ? "图像网关已连接，但没有启用图像生成。"
        : "图像网关尚未配置图像 Provider。",
    );
  } catch (error) {
    mediaState.set("unconfigured");
    mediaNote.set(probeNote(error));
  }
};

const findAsset = (assetId: string): GalleryAsset | null => {
  for (const turn of turns()) {
    const hit = turn.assets.find((asset) => asset.id === assetId);
    if (hit) return hit;
  }
  return null;
};

const parentOf = (asset: GalleryAsset): GalleryAsset | null =>
  asset.parentAssetId ? findAsset(asset.parentAssetId) : null;

/** The ancestor assets of `asset`, oldest first (the asset itself excluded). */
const ancestorsOf = (asset: GalleryAsset): GalleryAsset[] => {
  const chain: GalleryAsset[] = [];
  let cursor = parentOf(asset);
  while (cursor && chain.length < 24) {
    chain.unshift(cursor);
    cursor = parentOf(cursor);
  }
  return chain;
};

const activeEdit = (): GalleryAsset | null => {
  const id = editTarget();
  return id ? findAsset(id) : null;
};

const canGenerate = (): boolean => {
  if (!ready() || busy()) return false;
  if (!prompt().trim()) return false;
  return !editTarget() || !!editInstruction().trim();
};

const narrow = (): boolean =>
  paneWidthPx() > 0 ? paneWidthPx() < 640 : !wideMode() || windowSize().width < 640;

/** 1 asset stays one column, 2 share a row, 3–4 fill a two-column grid; below
 * 640px everything is one column (the 390px requirement). */
const gridColumns = (assets: number): number => (narrow() ? 1 : assets <= 2 ? Math.max(assets, 1) : 2);

const tileWidth = (turnId: string, assets: number): number | "48%" | "100%" => {
  const columns = gridColumns(assets);
  if (columns === 1) return "100%";
  const measured = gridWidths()[turnId] ?? 0;
  if (measured <= 0) return "48%";
  return Math.max(96, Math.floor((measured - space.md * (columns - 1)) / columns));
};


/** The model this panel's session selected, when it is an image model. The
 * gateway routes on the model id: sending it is what makes "the provider the
 * user picked" the provider that answers. Without it the gateway falls back to
 * the deployment default, which in a mixed deployment is a different (and in
 * the demo case, a paid) provider. Returns undefined when the selection is not
 * an image model, so the gateway default still applies. */
const selectedImageModel = (): string | undefined => {
  const selection = store.sessionSelection() ?? store.tenantSelection();
  if (!selection?.modelId) return undefined;
  const row = store
    .models()
    .find((model) => model.modelId === selection.modelId && (!selection.providerId || model.providerId === selection.providerId));
  if (!row) return undefined;
  return resolveSurface(row).kind === "image" ? selection.modelId : undefined;
};

const appendTurn = (turn: GalleryTurn) => turns.update((list) => [...list, turn]);

const replaceTurn = (turnId: string, patch: Partial<GalleryTurn>) =>
  turns.update((list) => list.map((turn) => (turn.id === turnId ? { ...turn, ...patch } : turn)));

const imageUrlOf = (asset: GalleryAsset): string => asset.src;

/** Map the gateway's assets onto this gallery's projection: provider URLs
 * verbatim, the model that made each one, the version chain the parent implies.
 * No local file ever enters a turn. */
const realAssetsForTurn = (
  turn: GalleryTurn,
  result: { assets: { url: string; model?: string }[]; model?: string },
  parent: GalleryAsset | null,
): GalleryAsset[] =>
  result.assets.map((asset) => {
    const model = asset.model ?? result.model;
    const projected: GalleryAsset = {
      id: nextId("asset"),
      src: asset.url,
      name: model ?? "Provider",
      version: parent ? parent.version + 1 : 1,
      prompt: turn.prompt,
      aspect: turn.aspect,
      turnId: turn.id,
    };
    if (parent) projected.parentAssetId = parent.id;
    if (model) projected.model = model;
    return projected;
  });

/** One real turn: the gateway call, its abort, and the three outcomes the
 * surface already knows (done / failed / cancelled). An abort is the user's own
 * cancel: the turn was already marked cancelled, so nothing here writes over
 * it. A 503 keeps the turn retryable and marks it as needing configuration. */
const runRealTurn = async (turnId: string) => {
  const turn = turns().find((item) => item.id === turnId);
  if (!turn) return;
  const parent = turn.editParentId ? findAsset(turn.editParentId) : null;
  if (turn.kind === "edit" && !parent) {
    replaceTurn(turnId, {
      status: "failed",
      assets: [],
      error: "找不到可编辑的资产：请从画廊里重新选择一张图。",
    });
    return;
  }

  const controller = new AbortController();
  activeController = controller;
  busy.set(true);
  activeTurn.set(turnId);

  try {
    const result =
      turn.kind === "edit" && parent
        ? await editImage(
            {
              prompt: (turn.editInstruction ?? "").trim() || turn.prompt,
              image: imageUrlOf(parent),
              ...(selectedImageModel() ? { model: selectedImageModel()! } : {}),
            },
            { signal: controller.signal },
          )
        : await generateImages(
            {
              prompt: turn.prompt,
              ...(turn.negative ? { negativePrompt: turn.negative } : {}),
              size: sizeForAspect(turn.aspect),
              n: turn.count,
              ...(selectedImageModel() ? { model: selectedImageModel()! } : {}),
            },
            { signal: controller.signal },
          );

    const current = turns().find((item) => item.id === turnId);
    if (!current || current.status !== "generating") return;
    const assets = realAssetsForTurn(turn, result, parent);
    if (!assets.length) {
      replaceTurn(turnId, {
        status: "failed",
        assets: [],
        error: "图像网关没有返回任何图像，本轮输入已保留，可直接重试。",
      });
      return;
    }
    replaceTurn(turnId, {
      status: "done",
      assets,
      error: undefined,
      configRequired: false,
      ...(result.model ? { model: result.model } : {}),
    });
    if (turn.kind === "edit" && editTarget() === turn.editParentId) {
      editTarget.set(null);
      editInstruction.set("");
    }
  } catch (error) {
    // A cancel aborts the fetch; its turn is already cancelled.
    if (controller.signal.aborted || (error instanceof MediaGatewayError && error.aborted)) return;
    const current = turns().find((item) => item.id === turnId);
    if (!current || current.status !== "generating") return;
    replaceTurn(turnId, {
      status: "failed",
      assets: [],
      error: mediaErrorText(error),
      configRequired: error instanceof MediaGatewayError && error.kind === "notConfigured",
    });
  } finally {
    if (activeController === controller) activeController = undefined;
    if (activeTurn() === turnId) {
      busy.set(false);
      activeTurn.set(null);
    }
  }
};

const enqueueTurn = (config: {
  kind: TurnKind;
  prompt: string;
  negative: string;
  aspect: AspectRatio;
  count: number;
  editParent?: GalleryAsset;
  editInstruction?: string;
}) => {
  if (!ready() || busy() || !config.prompt.trim()) return;
  const turn: GalleryTurn = {
    id: nextId("turn"),
    kind: config.kind,
    status: "generating",
    prompt: config.prompt.trim(),
    negative: config.negative.trim(),
    aspect: config.aspect,
    count: Math.max(1, Math.min(4, config.count)),
    assets: [],
    editParentId: config.editParent?.id,
    editInstruction: config.editInstruction,
    attempt: 1,
  };
  appendTurn(turn);
  void runRealTurn(turn.id);
};

const generateFromComposer = () => {
  if (!canGenerate()) return;
  const edit = activeEdit();
  enqueueTurn({
    kind: edit ? "edit" : "generate",
    prompt: prompt().trim() || edit?.prompt || "",
    negative: negative(),
    aspect: aspect(),
    count: count(),
    editParent: edit ?? undefined,
    editInstruction: edit ? editInstruction().trim() : undefined,
  });
};

const cancelGeneration = () => {
  if (!busy()) return;
  const turnId = activeTurn();
  const controller = activeController;
  activeController = undefined;
  if (controller) controller.abort();
  if (turnId) replaceTurn(turnId, { status: "cancelled", assets: [], error: undefined });
  busy.set(false);
  activeTurn.set(null);
};

const retryTurn = (turn: GalleryTurn) => {
  if (busy() || !ready()) return;
  replaceTurn(turn.id, {
    status: "generating",
    assets: [],
    error: undefined,
    configRequired: false,
    attempt: turn.attempt + 1,
  });
  void runRealTurn(turn.id);
};

const regenerate = (asset: GalleryAsset) => {
  if (busy() || !ready()) return;
  const source = turns().find((turn) => turn.id === asset.turnId);
  if (!source) return;
  enqueueTurn({
    kind: "regenerate",
    prompt: asset.prompt,
    negative: source.negative,
    aspect: asset.aspect,
    count: 1,
  });
};

const beginEdit = (asset: GalleryAsset) => {
  editTarget.set(asset.id);
  editInstruction.set("");
  // The edit inherits the source's declared params, so the only new thing the
  // user writes is the instruction.
  aspect.set(asset.aspect);
  if (!prompt().trim()) prompt.set(asset.prompt);
  previewId.set(null);
  promptTick.update((value) => value + 1);
};

const prefill = (text: string) => {
  prompt.set(text);
  promptTick.update((value) => value + 1);
};

// ---- render helpers (values in, JSX out; called from the tracked tree) ----

const statusVariant = (status: TurnStatus): "success" | "warning" | "info" | "destructive" =>
  status === "failed" ? "destructive" : status === "cancelled" ? "warning" : status === "generating" ? "info" : "success";

const statusLabel = (turn: GalleryTurn): string =>
  turn.status === "failed"
    ? "生成失败"
    : turn.status === "cancelled"
      ? "已取消"
      : turn.status === "generating"
        ? "生成中"
        : `已完成 · ${turn.assets.length} 张`;

const kindLabel = (kind: TurnKind): string =>
  kind === "edit" ? "多轮编辑" : kind === "regenerate" ? "重新生成" : "首轮生成";

function AssetAction(props: {
  icon: IconName;
  hint: string;
  label: string;
  testID: string;
  onPress: () => void;
}) {
  return (
    <View tooltip={<Tooltip label={props.hint} />}>
      <IconButton
        icon={props.icon}
        size="sm"
        label={props.label}
        testID={props.testID}
        onPress={props.onPress}
      />
    </View>
  );
}

function renderAsset(asset: GalleryAsset, width: number | "48%" | "100%") {
  const parent = parentOf(asset);
  return (
    <View style={{ width, gap: space.xs }} testID={`image-cell-${asset.id}`}>
      <View
        testID={`image-asset-${asset.id}`}
        role="button"
        focusable
        accessibilityLabel={`预览${assetTag(asset)}`}
        onPress={() => previewId.set(asset.id)}
        hoverStyle={{ opacity: 0.94 }}
        style={{
          position: "relative",
          overflow: "hidden",
          borderRadius: radius.lg,
          borderWidth: 1,
          borderColor: color.border,
          backgroundColor: color.muted,
        }}
      >
        <Image
          src={asset.src}
          style={{
            width: "100%",
            aspectRatio: aspectRatioOf(asset.aspect),
            objectFit: "cover",
          }}
        />
        <View
          style={{
            position: "absolute",
            top: space.sm,
            left: space.sm,
            flexDirection: "row",
            flexWrap: "wrap",
            gap: space.xs,
          }}
        >
          <Badge variant="secondary" label={`v${asset.version}`} testID={`image-version-${asset.id}`} />
          <If when={!!parent}>
            <Badge variant="info" icon="layers" label={`派生自 v${parent?.version ?? 1}`} />
          </If>
        </View>
        <View style={{ position: "absolute", top: space.sm, right: space.sm }}>
          <Badge
            variant="success"
            icon="spark"
            label={modelLabel(asset)}
            testID={`image-model-${asset.id}`}
          />
        </View>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.xxs }}>
        <AssetAction
          icon="search"
          hint="预览大图与版本链"
          label={`预览 v${asset.version}`}
          testID={`image-preview-${asset.id}`}
          onPress={() => previewId.set(asset.id)}
        />
        <AssetAction
          icon="tool"
          hint="以这张图为基础做多轮编辑"
          label={`编辑 v${asset.version}`}
          testID={`image-edit-${asset.id}`}
          onPress={() => beginEdit(asset)}
        />
        <AssetAction
          icon="refresh"
          hint="用同一组参数再生成一张（同级，不派生）"
          label={`重新生成 v${asset.version}`}
          testID={`image-regenerate-${asset.id}`}
          onPress={() => regenerate(asset)}
        />
        <View style={{ flex: 1 }} />
        <Text style={{ numberOfLines: 1, fontSize: typeScale.xs, color: color.mutedForeground }}>
          {"Provider 输出"}
        </Text>
      </View>
    </View>
  );
}

function renderGrid(turn: GalleryTurn, assets: GalleryAsset[], skeletonCount = 0) {
  const total = skeletonCount > 0 ? skeletonCount : assets.length;
  const width = tileWidth(turn.id, total);
  return (
    <View
      testID={`image-grid-${turn.id}`}
      onLayout={(event) => {
        // The measured width feeds back into the tiles' width, so an
        // unconditional write is a layout loop: measure → set → re-layout →
        // measure. Ignore sub-pixel noise and unchanged widths; a real resize
        // still lands because its delta is far above this threshold.
        const next = event.bounds.size.width;
        const previous = gridWidths()[turn.id] ?? 0;
        if (Math.abs(next - previous) < 0.5) return;
        gridWidths.update((widths) => ({ ...widths, [turn.id]: next }));
      }}
      style={{ flexDirection: "row", flexWrap: "wrap", gap: space.md, alignItems: "flex-start" }}
    >
      <If when={skeletonCount > 0}>
        {Array.from({ length: skeletonCount }, (_, index) => (
          <View style={{ width, gap: space.sm }}>
            <Skeleton
              height={Math.max(96, Math.round(Number(width) / aspectRatioOf(turn.aspect)) || 180)}
              radius={radius.lg}
              accessibilityLabel={`生成中占位 ${index + 1}`}
              testID={`image-skeleton-${turn.id}-${index + 1}`}
            />
          </View>
        ))}
      </If>
      <If when={skeletonCount === 0}>
        {assets.map((asset) => (
          <View style={{ width }}>
            {renderAsset(asset, "100%")}
          </View>
        ))}
      </If>
    </View>
  );
}

function renderRetryRow(turn: GalleryTurn, label: string) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.md }}>
      <Button
        variant="outline"
        size="sm"
        icon="refresh"
        label={label}
        accessibilityLabel="重试本轮生成"
        testID={`image-retry-${turn.id}`}
        disabled={busy() || !ready()}
        onPress={() => retryTurn(turn)}
      />
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, flex: 1, minWidth: 160 }}>
        prompt 与参数都留在 composer 里；重试沿用本轮快照。
      </Text>
    </View>
  );
}

function renderTurn(turn: GalleryTurn, index: number) {
  const parent = turn.editParentId ? findAsset(turn.editParentId) : null;
  return (
    <Card
      testID={`image-turn-${turn.id}`}
      accessibilityLabel={`第 ${index + 1} 轮：${turn.prompt}`}
    >
      <CardHeader
        title={turn.prompt}
        description={
          turn.kind === "edit"
            ? `编辑指令：${turn.editInstruction ?? "（无）"}`
            : turn.kind === "regenerate"
              ? "重新生成：同参数的新样本，作为同级资产加入画廊"
              : "生成参数"
        }
        action={
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
            <Badge
              variant={statusVariant(turn.status)}
              icon={turn.status === "generating" ? "clock" : turn.status === "failed" ? "alert" : "check"}
              label={statusLabel(turn)}
              testID={`image-status-${turn.id}`}
            />
            <Badge variant="outline" label={kindLabel(turn.kind)} />
          </View>
        }
      />
      <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: space.xs }}>
        <Badge variant="secondary" label={`比例 ${turn.aspect}`} />
        <Badge variant="secondary" label={`${turn.count} 张`} />
        <If when={!!turn.model}>
          <Badge variant="success" icon="spark" label={turn.model ?? ""} />
        </If>
        <If when={!!turn.negative.trim()}>
          <Badge variant="outline" label={`负向：${clip(turn.negative, 16)}`} />
        </If>
        <If when={!!parent}>
          <Badge variant="info" icon="layers" label={`派生自 v${parent?.version ?? 1}`} />
        </If>
      </View>
      <CardContent>
        <If when={turn.status === "generating"}>
          {renderGrid(turn, [], turn.count)}
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Spinner size={14} accessibilityLabel="生成中" />
            <Text testID={`image-progress-${turn.id}`} style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              {`生成中 · 正在生成 ${turn.count} 张，可取消`}
            </Text>
          </View>
        </If>
        <If when={turn.status === "failed"}>
          <View
            role="alert"
            accessibilityLabel="生成失败"
            testID={`image-error-${turn.id}`}
            style={{
              gap: space.md,
              padding: space.lg,
              borderRadius: radius.lg,
              borderWidth: 1,
              borderColor: alpha(color.danger, 0.35),
              backgroundColor: color.dangerTint,
            }}
          >
            <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
              <Icon name="alert" size={16} tint={color.danger} />
              <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.danger }}>
                {turn.error ?? "生成失败"}
              </Text>
            </View>
            {renderRetryRow(turn, "重试")}
            <If when={turn.configRequired === true}>
              <Button
                variant="outline"
                size="sm"
                icon="settings"
                label="配置图像 Provider"
                accessibilityLabel="配置图像 Provider"
                testID={`image-error-config-${turn.id}`}
                onPress={() => configOpen.set(true)}
              />
            </If>
          </View>
        </If>
        <If when={turn.status === "cancelled"}>
          <View
            testID={`image-cancelled-${turn.id}`}
            style={{
              gap: space.md,
              padding: space.lg,
              borderRadius: radius.lg,
              borderWidth: 1,
              borderColor: alpha(color.warning, 0.35),
              backgroundColor: color.warningTint,
            }}
          >
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Badge variant="warning" label="已取消" />
              <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.warning }}>
                本轮已取消，尚未产生资产。
              </Text>
            </View>
            {renderRetryRow(turn, "重试")}
          </View>
        </If>
        <If when={turn.status === "done"}>{renderGrid(turn, turn.assets)}</If>
      </CardContent>
    </Card>
  );
}

function renderSamples() {
  return (
    <EmptyContent style={{ flexWrap: "wrap", justifyContent: "center", maxWidth: 560 }}>
      <Button
        variant="secondary"
        label={SAMPLE_PROMPTS[0]}
        accessibilityLabel={`使用示例 prompt ${SAMPLE_PROMPTS[0]}`}
        testID="image-sample-0"
        onPress={() => prefill(SAMPLE_PROMPTS[0]!)}
      />
      <Button
        variant="secondary"
        label={SAMPLE_PROMPTS[1]}
        accessibilityLabel={`使用示例 prompt ${SAMPLE_PROMPTS[1]}`}
        testID="image-sample-1"
        onPress={() => prefill(SAMPLE_PROMPTS[1]!)}
      />
    </EmptyContent>
  );
}

function renderChecking() {
  return (
    <Empty testID="image-checking" accessibilityLabel="正在检查图像服务" style={{ paddingVertical: space["3xl"] }}>
      <EmptyMedia icon="image" tone="brand" />
      <View style={{ alignItems: "center", gap: space.sm }}>
        <Spinner size={18} accessibilityLabel="检查中" />
        <EmptyTitle label="正在检查图像服务" />
        <EmptyDescription label="确认图像 Provider 是否可用…" />
      </View>
    </Empty>
  );
}

function renderConfigState() {
  return (
    <Empty testID="image-config-state" accessibilityLabel="图像服务配置态" style={{ paddingVertical: space["3xl"] }}>
      <EmptyMedia icon="image" tone="brand" />
      <View style={{ alignItems: "center", gap: space.sm }}>
        <EmptyTitle label="图像生成尚未配置" />
        <EmptyDescription label="配置一个图像 Provider 后，才能在这里生成或编辑图片。" />
        <If when={mediaNote() !== ""}>
          <EmptyDescription label={mediaNote()} testID="image-config-reason" />
        </If>
        <EmptyDescription label="示例只填入 prompt，不会自动发送。" />
      </View>
      {renderSamples()}
      <EmptyContent style={{ flexWrap: "wrap", justifyContent: "center" }}>
        <Button
          variant="outline"
          icon="settings"
          label="配置图像 Provider"
          accessibilityLabel="配置图像 Provider"
          testID="image-config-cta"
          onPress={() => configOpen.set(true)}
        />
        <Button
          variant="secondary"
          icon="refresh"
          label="重新检查"
          accessibilityLabel="重新检查图像服务"
          testID="image-config-recheck"
          onPress={() => void probeMedia()}
        />
      </EmptyContent>
    </Empty>
  );
}

function renderReadyEmpty() {
  return (
    <Empty testID="image-empty" accessibilityLabel="图像空态" style={{ paddingVertical: space["3xl"] }}>
      <EmptyMedia icon="image" tone="brand" />
      <View style={{ alignItems: "center", gap: space.sm }}>
        <EmptyTitle label="描述一张图，开始第一轮生成" />
        <EmptyDescription label="图像服务已连接：生成会调用真实 Provider，结果由网关返回。" />
        <EmptyDescription label="示例只填入 prompt，不会自动发送；生成可能产生 Provider 费用。" />
      </View>
      {renderSamples()}
    </Empty>
  );
}

function renderEmpty() {
  return (
    <If
      when={mediaState() === "checking"}
      else={
        <If when={mediaState() === "unconfigured"} else={renderReadyEmpty()}>
          {renderConfigState()}
        </If>
      }
    >
      {renderChecking()}
    </If>
  );
}

function renderPreview() {
  const asset = previewId() ? findAsset(previewId()!) : null;
  return (
    <If when={!!asset}>
      <Dialog
        open
        testID="image-preview-dialog"
        title={asset ? assetTag(asset) : "资产"}
        description={asset?.prompt ?? ""}
        size="lg"
        onOpenChange={(event) => {
          if (!event.open) previewId.set(null);
        }}
        footer={
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
            <Button
              variant="outline"
              size="sm"
              icon="tool"
              label="编辑"
              accessibilityLabel="编辑这张图"
              onPress={() => asset && beginEdit(asset)}
            />
            <Button
              size="sm"
              label="关闭"
              accessibilityLabel="关闭预览"
              onPress={() => previewId.set(null)}
            />
          </View>
        }
      >
        <Image
          src={asset?.src ?? ""}
          style={{
            width: "100%",
            aspectRatio: aspectRatioOf(asset?.aspect ?? "1:1"),
            maxHeight: PREVIEW_MAX_HEIGHT_PX,
            objectFit: "contain",
            borderRadius: radius.md,
            borderWidth: 1,
            borderColor: color.border,
            backgroundColor: color.muted,
          }}
        />
        <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
          <Badge variant="secondary" label={`比例 ${asset?.aspect ?? ""}`} />
          <Badge variant="success" icon="spark" label={asset ? modelLabel(asset) : "Provider"} />
          <Badge variant="outline" label="Provider 输出" />
        </View>
        <If when={!!asset && ancestorsOf(asset).length > 0}>
          <View style={{ gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>
              版本链
            </Text>
            <Breadcrumb testID="image-preview-chain" accessibilityLabel="版本链">
              <BreadcrumbList>
                {asset
                  ? ancestorsOf(asset).map((ancestor) => (
                      <BreadcrumbItem>
                        <BreadcrumbLink label={`v${ancestor.version}`} />
                        <BreadcrumbSeparator />
                      </BreadcrumbItem>
                    ))
                  : null}
                <BreadcrumbItem>
                  <BreadcrumbPage label={`v${asset?.version ?? 1}（当前）`} />
                </BreadcrumbItem>
              </BreadcrumbList>
            </Breadcrumb>
            <If when={!!asset && !!parentOf(asset)}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {`来源 v${parentOf(asset!)?.version ?? 1}：${clip(parentOf(asset!)?.prompt ?? "", 42)}`}
              </Text>
            </If>
          </View>
        </If>
        <If when={!!asset && ancestorsOf(asset).length === 0}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这是首轮样本（v1），没有上游版本。
          </Text>
        </If>
      </Dialog>
    </If>
  );
}

function renderConfigDialog() {
  return (
    <If when={configOpen()}>
      <Dialog
        open
        testID="image-config-dialog"
        title="配置图像 Provider"
        description={
          ready()
            ? "图像网关已连接，生成会调用已配置的 Provider。"
            : "当前没有可用的图像生成服务，生成按钮暂不可用。"
        }
        onOpenChange={(event) => {
          if (!event.open) configOpen.set(false);
        }}
        footer={
          <Button
            size="sm"
            label="知道了"
            accessibilityLabel="关闭配置说明"
            onPress={() => configOpen.set(false)}
          />
        }
      >
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.relaxed), color: color.foreground }}>
          接入步骤：打开「设置 → 模型接入」，添加一个 providerType 为 image 的服务商及其模型并保存生效。之后用该模型新建会话，这个面板会自动切换到真实生成。
        </Text>
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.relaxed), color: color.mutedForeground }}>
          {ready()
            ? "真实生成通过同源图像网关（/v1/media）调用 Provider，结果 URL 与模型名来自网关；取消会中止请求。"
            : "在配置完成前，这里不会发起生成请求，也不会展示任何本地占位结果。"}
        </Text>
      </Dialog>
    </If>
  );
}

function renderComposer() {
  const edit = activeEdit();
  return (
    <View style={{ flexDirection: "column", gap: space.md }}>
      <If when={!!edit}>
        <View
          style={{
            gap: space.sm,
            padding: space.md,
            borderRadius: radius.md,
            borderWidth: 1,
            borderColor: alpha(color.brand, 0.35),
            backgroundColor: color.brandTint,
          }}
        >
          <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
            <Badge
              variant="info"
              icon="tool"
              label={`正在编辑 v${edit?.version ?? 1}`}
              accessibilityLabel={`正在编辑 v${edit?.version ?? 1}，将生成 v${(edit?.version ?? 1) + 1}`}
              testID="image-edit-chip"
            />
            <Text style={{ flex: 1, minWidth: 160, fontSize: typeScale.xs, color: color.brand }}>
              新资产会记下 parentAssetId，形成版本链；原图保留。
            </Text>
            <IconButton
              icon="x"
              size="sm"
              label="退出编辑"
              testID="image-edit-cancel"
              onPress={() => {
                editTarget.set(null);
                editInstruction.set("");
              }}
            />
          </View>
          <Field
            label="编辑指令"
            hint="描述要改什么；为空时不能生成。"
            testID="image-edit-field"
          >
            <Input
              size="sm"
              testID="image-edit-instruction"
              accessibilityLabel="编辑指令"
              value={editInstruction()}
              onChange={editInstruction.set}
              placeholder="例如：把背景换成夜景，保留主体的玻璃质感"
            />
          </Field>
        </View>
      </If>
      <NativeInput
        ref={(node) => {
          promptInput = node;
        }}
        testID="image-prompt"
        accessibilityLabel="图像提示词"
        value={prompt()}
        onChange={({ value }) => prompt.set(value)}
        multiline
        submitBehavior="newline"
        placeholder="描述画面：主体、风格、光线、构图…"
        onContentSizeChange={({ size }) => promptHeight.set(size.height)}
        style={{
          // The shell owns the box: the field itself is chrome-less and grows
          // with its content up to six lines, exactly like the chat composer.
          height: promptFieldHeight(),
          borderWidth: 0,
          backgroundColor: "transparent",
          paddingHorizontal: space.none,
          paddingVertical: space.none,
          fontSize: typeScale.base,
          lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
          color: color.foreground,
          placeholderColor: color.mutedForeground,
          caretColor: color.brand,
          selectionColor: alpha(color.brand, 0.3),
        }}
      />
      <Collapsible
        open={negativeOpen()}
        reveal={false}
        content={
          <View style={{ paddingTop: space.xs }}>
            <Input
              size="sm"
              testID="image-negative"
              accessibilityLabel="负向提示词"
              value={negative()}
              onChange={negative.set}
              placeholder="不想出现的内容，例如：文字、水印、过曝"
            />
          </View>
        }
      >
        <CollapsibleTrigger
          label={negativeOpen() ? "收起负向提示词" : "负向提示词（可选）"}
          icon="x"
          expanded={negativeOpen()}
          onPress={() => negativeOpen.update((value) => !value)}
          testID="image-negative-toggle"
        />
      </Collapsible>
      <Separator />
      <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "flex-end", gap: space.md }}>
        <Field label="比例" testID="image-aspect-field">
          <Tabs accessibilityLabel="画幅比例" testID="image-aspect">
            <Tab
              label="1:1"
              selected={aspect() === "1:1"}
              onPress={() => aspect.set("1:1")}
              testID="image-aspect-1x1"
            />
            <Tab
              label="16:9"
              selected={aspect() === "16:9"}
              onPress={() => aspect.set("16:9")}
              testID="image-aspect-16x9"
            />
            <Tab
              label="9:16"
              selected={aspect() === "9:16"}
              onPress={() => aspect.set("9:16")}
              testID="image-aspect-9x16"
            />
          </Tabs>
        </Field>
        <Field label="数量" testID="image-count-field">
          <Tabs accessibilityLabel="生成数量" testID="image-count">
            <Tab label="1" selected={count() === 1} onPress={() => count.set(1)} testID="image-count-1" />
            <Tab label="2" selected={count() === 2} onPress={() => count.set(2)} testID="image-count-2" />
            <Tab label="3" selected={count() === 3} onPress={() => count.set(3)} testID="image-count-3" />
            <Tab label="4" selected={count() === 4} onPress={() => count.set(4)} testID="image-count-4" />
          </Tabs>
        </Field>
      </View>
      <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: space.sm }}>
        <If when={mediaState() === "checking"}>
          <Badge variant="outline" icon="clock" label="检查中" testID="image-mode-checking" />
        </If>
        <If when={mediaState() === "unconfigured"}>
          <Badge
            variant="warning"
            icon="alert"
            label="未配置"
            accessibilityLabel="图像服务未配置"
            testID="image-mode-unconfigured"
          />
        </If>
        <If when={mediaState() === "ready"}>
          <Badge
            variant="success"
            icon="check"
            label="真实生成"
            accessibilityLabel="真实图像网关"
            testID="image-mode-real"
          />
        </If>
        <View style={{ flex: 1 }} />
        <If when={busy()}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Spinner size={14} accessibilityLabel="生成中" />
            <Text
              testID="image-progress"
              style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
            >
              {`生成中 · ${turns().find((turn) => turn.id === activeTurn())?.count ?? 0} 张`}
            </Text>
          </View>
          <Button
            testID="image-cancel"
            variant="destructive"
            size="sm"
            icon="stop"
            label="取消"
            accessibilityLabel="取消生成"
            onPress={cancelGeneration}
          />
        </If>
        <Button
          testID="image-generate"
          icon="spark"
          label={busy() ? "生成中" : edit ? "生成编辑" : "生成"}
          loading={busy()}
          disabled={!canGenerate()}
          accessibilityLabel={edit ? "生成编辑资产" : "生成图像"}
          onPress={generateFromComposer}
        />
      </View>
    </View>
  );
}

// ---- the exported surface -------------------------------------------------

export function ImageSurface(props: { wide: boolean }) {
  const paneWidth = () => (paneWidthPx() > 0 ? paneWidthPx() : windowSize().width);
  const empty = () => turns().length === 0;

  effect(() => {
    wideMode.set(props.wide);
  });

  effect(() => {
    if (promptTick() === 0) return;
    untracked(() => promptInput?.focus());
  });

  // One gateway probe per mount: only a configured image provider unlocks
  // generation. Every other answer (no provider, no image capability, an
  // unreachable gateway) is the configuration state, with its reason.
  effect(() => {
    if (probed) return;
    probed = true;
    void probeMedia();
  });

  cleanup(() => {
    activeController?.abort();
    activeController = undefined;
  });

  return (
    <View
      testID="image-surface"
      onLayout={(event) => paneWidthPx.set(event.bounds.size.width)}
      style={{
        flex: 1,
        minWidth: 0,
        position: "relative",
        backgroundColor: color.background,
      }}
    >
      <ConversationColumn
        draft={empty()}
        rail={false}
        windowWidthPx={paneWidth()}
        testID="image-column"
        style={{ flex: 1, minHeight: 0, flexDirection: "row" }}
      >
        <ScrollArea
          testID="image-timeline"
          accessibilityLabel="生成画廊"
          style={{ flex: 1 }}
          contentStyle={{ paddingVertical: space.xl, gap: space["2xl"] }}
        >
          <If when={!empty() && mediaState() === "unconfigured"}>
            <View
              testID="image-config-strip"
              accessibilityLabel="图像服务未配置"
              style={{
                flexDirection: "row",
                alignItems: "flex-start",
                gap: space.sm,
                padding: space.md,
                borderRadius: radius.lg,
                borderWidth: 1,
                borderColor: alpha(color.warning, 0.3),
                backgroundColor: color.warningTint,
              }}
            >
              <Badge variant="warning" icon="alert" label="未配置" />
              <View style={{ flex: 1, gap: space.xxs }}>
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
                    color: color.warning,
                  }}
                >
                  图像 Provider 尚未配置：生成按钮不可用，配置后即可生成与编辑图片。
                </Text>
                <If when={mediaNote() !== ""}>
                  <Text
                    testID="image-config-strip-reason"
                    style={{
                      fontSize: typeScale.xs,
                      lineHeight: Math.round(typeScale.xs * lineHeight.relaxed),
                      color: color.warning,
                    }}
                  >
                    {mediaNote()}
                  </Text>
                </If>
              </View>
              <Button
                variant="outline"
                size="sm"
                icon="settings"
                label="配置图像 Provider"
                accessibilityLabel="配置图像 Provider"
                testID="image-config-strip-cta"
                onPress={() => configOpen.set(true)}
              />
            </View>
          </If>
          <If when={empty()}>{renderEmpty()}</If>
          <If when={!empty()}>
            {turns().map((turn, index) => (
              <View>{renderTurn(turn, index)}</View>
            ))}
          </If>
        </ScrollArea>
      </ConversationColumn>
      <ComposerDock
        draft={empty()}
        rail={false}
        windowWidthPx={paneWidth()}
        testID="image-composer-dock"
      >
        <ComposerSurface testID="image-composer">{renderComposer()}</ComposerSurface>
      </ComposerDock>
      {renderPreview()}
      {renderConfigDialog()}
    </View>
  );
}
