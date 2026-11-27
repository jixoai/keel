// The slot registry (W12): declare, contribute, load, unload.
//
// The mechanism is the harness's; the slots in it belong to plugins. This
// module registers both, and owns the one lifecycle rule that makes a plugin
// disable safe: a plugin's slots and contributions disappear together, and a
// contribution whose slot vanished is reported instead of left as a dead
// entry.
//
// Reactivity: the state is one NSH signal, replaced on every change. Screens
// read `registry.state()` (or the helpers below) in a vnode getter, so a
// contribution appears and disappears with its plugin — no reload.
import { signal } from "nsh";
import { BASE_SLOTS, KEEL_UI_PLUGIN } from "./base";
import { createPluginBridge } from "./bridge";
import { formAvailability, slotTargetAvailable } from "./forms";
import { loadChunkModule } from "./loader";
import { mimeMatches } from "./mime";
import { normalizeContribution, normalizeSlot } from "./validation";
import type {
  MountedContribution,
  SlotDeclaration,
  SlotContribution,
  SlotIssue,
  SlotTarget,
  SlotView,
} from "./types";

export interface RegistryState {
  slots: SlotDeclaration[];
  contributions: MountedContribution[];
  issues: SlotIssue[];
}

export interface PluginDeclarationInput {
  pluginId: string;
  /** `provides.slots` (already unwrapped from `provides`). */
  slots?: unknown[];
  /** `contributes`. */
  contributes?: unknown[];
}

export interface RegisterResult {
  ok: boolean;
  issues: SlotIssue[];
}

const contributionKey = (pluginId: string, id: string): string => `${pluginId}\u0000${id}`;

/** One registry. The app has exactly one; tests build their own. */
export class SlotRegistry {
  private readonly stateSignal = signal<RegistryState>({ slots: [], contributions: [], issues: [] });
  private generation = 0;
  /** The load generation per contribution key, so a replaced contribution's
   * late chunk cannot overwrite its successor. */
  private readonly loads = new Map<string, number>();
  /** The model-facing tools each enabled plugin declares (the bridge's own-tool
   * allowlist). Filled by the inventory sync. */
  private readonly pluginTools = new Map<string, string[]>();
  /** One scoped bridge per plugin: a render must not mint a new host scope. */
  private readonly bridges = new Map<string, ReturnType<typeof createPluginBridge>>();

  /** The reactive state. */
  state(): RegistryState {
    return this.stateSignal();
  }

  issues(): SlotIssue[] {
    return this.stateSignal().issues;
  }

  slots(): SlotDeclaration[] {
    return this.stateSignal().slots;
  }

  slot(id: string): SlotDeclaration | undefined {
    return this.stateSignal().slots.find((slot) => slot.id === id);
  }

  contributions(): MountedContribution[] {
    return this.stateSignal().contributions;
  }

  /** Register (or replace) one plugin's whole UI declaration atomically. A
   * field error anywhere leaves the previous declaration untouched: no
   * half-contribution is ever visible. */
  registerPlugin(input: PluginDeclarationInput): RegisterResult {
    const pluginId = String(input.pluginId ?? "").trim();
    if (!pluginId) {
      const fault: SlotIssue = {
        severity: "error",
        code: "ERR_CONTRIBUTION_INVALID",
        pluginId,
        field: "pluginId",
        message: "插件 id 不能为空。",
      };
      this.recordIssues([fault]);
      return { ok: false, issues: [fault] };
    }
    const current = this.stateSignal();
    const issues: SlotIssue[] = [];
    const declaresSlots = input.slots !== undefined;

    // 1. The plugin's new slots, validated as declarations.
    const nextSlots: SlotDeclaration[] = [];
    for (let index = 0; index < (input.slots ?? []).length; index += 1) {
      const normalized = normalizeSlot((input.slots ?? [])[index], pluginId, false, index);
      issues.push(...normalized.issues);
      if (normalized.slot) nextSlots.push(normalized.slot);
    }
    if (issues.some((entry) => entry.severity === "error")) {
      this.recordIssues(issues);
      return { ok: false, issues };
    }

    // 2. Merge: the plugin's previous slots go (only when it declares slots);
    // another provider owning the same id fails closed.
    const baseSlots = declaresSlots
      ? current.slots.filter((slot) => slot.provider !== pluginId)
      : [...current.slots];
    for (const slot of nextSlots) {
      const other = baseSlots.find((entry) => entry.id === slot.id);
      if (other) {
        issues.push({
          severity: "error",
          code: "ERR_SLOT_CONFLICT",
          pluginId,
          field: `provides.slots.${slot.id}`,
          message: `槽位「${slot.id}」已由插件「${other.provider}」声明；一个槽位只能有一个声明者。`,
        });
      }
    }
    if (issues.some((entry) => entry.severity === "error")) {
      this.recordIssues(issues);
      return { ok: false, issues };
    }
    const mergedSlots = [...baseSlots, ...nextSlots];
    const slotsByID = new Map(mergedSlots.map((slot) => [slot.id, slot]));

    // 3. The plugin's contributions, validated against the merged slot catalog.
    // A slot this plugin used to provide and no longer does takes its
    // contributors with it on commit (reported, never left as a dead entry).
    const removedSlotIDs = new Set(
      current.slots
        .filter((slot) => slot.provider === pluginId && !slotsByID.has(slot.id))
        .map((slot) => slot.id),
    );
    const baseContributions = current.contributions.filter(
      (entry) => entry.declaration.pluginId !== pluginId,
    );
    const nextContributions: MountedContribution[] = [];
    /** One MIME pattern may be claimed once per plugin (the same rule the
     * backend enforces); a repeat is a manifest error, not a first-match tie. */
    const claimedAttachmentMime = new Map<string, number>();
    for (let index = 0; index < (input.contributes ?? []).length; index += 1) {
      const raw = (input.contributes ?? [])[index];
      const declaredSlot =
        raw && typeof raw === "object" && typeof (raw as { slot?: unknown }).slot === "string"
          ? slotsByID.get(String((raw as { slot: string }).slot))
          : undefined;
      const normalized = normalizeContribution(raw, pluginId, declaredSlot, index);
      issues.push(...normalized.issues);
      if (!normalized.contribution) continue;
      const contribution = normalized.contribution;
      const duplicate = baseContributions.find(
        (entry) =>
          entry.declaration.id === contribution.id &&
          entry.declaration.pluginId !== contribution.pluginId,
      );
      if (duplicate) {
        issues.push({
          severity: "error",
          code: "ERR_CONTRIBUTION_CONFLICT",
          pluginId,
          field: `contributes[${index}].id`,
          message: `贡献 id「${contribution.id}」已被插件「${duplicate.declaration.pluginId}」占用。`,
        });
        continue;
      }
      if (contribution.partKind === "attachment") {
        for (const pattern of contribution.mime ?? []) {
          const prior = claimedAttachmentMime.get(pattern);
          if (prior !== undefined) {
            issues.push({
              severity: "error",
              code: "ERR_CONTRIBUTION_INVALID",
              pluginId,
              field: `contributes[${index}].mime`,
              message: `MIME 模式「${pattern}」已被 contributes[${prior}] 认领；同一插件内不得重复。`,
            });
            continue;
          }
          claimedAttachmentMime.set(pattern, index);
        }
      }
      const slot = slotsByID.get(contribution.slot);
      if (slot?.arity === "single") {
        const occupant = baseContributions.find(
          (entry) =>
            entry.declaration.slot === contribution.slot &&
            entry.declaration.pluginId !== contribution.pluginId,
        );
        if (occupant) {
          issues.push({
            severity: "error",
            code: "ERR_SLOT_FULL",
            pluginId,
            field: `contributes[${index}].slot`,
            message: `槽位「${contribution.slot}」是单例，已由插件「${occupant.declaration.pluginId}」贡献。`,
          });
          continue;
        }
      }
      nextContributions.push({ declaration: contribution, status: "declared" });
    }
    if (issues.some((entry) => entry.severity === "error")) {
      this.recordIssues(issues);
      return { ok: false, issues };
    }

    // 4. Commit. The plugin's old contributions are disposed; its new ones
    // load asynchronously.
    const oldOwned = current.contributions.filter(
      (entry) => entry.declaration.pluginId === pluginId,
    );
    for (const entry of oldOwned) this.disposeMounted(entry);
    const orphaned = baseContributions.filter((entry) =>
      removedSlotIDs.has(entry.declaration.slot),
    );
    for (const entry of orphaned) {
      this.disposeMounted(entry);
      issues.push({
        severity: "warning",
        code: "ERR_SLOT_UNKNOWN",
        pluginId: entry.declaration.pluginId,
        field: `contributes.${entry.declaration.id}`,
        message: `槽位「${entry.declaration.slot}」随插件「${pluginId}」撤销，该贡献已一并撤销。`,
      });
    }
    this.generation += 1;
    const kept = [
      ...baseContributions.filter((entry) => !removedSlotIDs.has(entry.declaration.slot)),
      ...nextContributions,
    ];
    this.bridges.delete(pluginId);
    this.stateSignal.set({ slots: mergedSlots, contributions: kept, issues });
    for (const entry of nextContributions) {
      const slot = slotsByID.get(entry.declaration.slot);
      if (slot) void this.loadContribution(entry.declaration, slot, entry.declaration.targets);
    }
    return { ok: true, issues };
  }

  /** Declare (or replace) a plugin's slots without touching its contributions.
   * The inventory sync uses this first, so a contributor can target a slot a
   * later-scanned plugin provides. Existing contributions to a changed slot are
   * re-validation-filtered, never silently kept under a narrower declaration. */
  declareSlots(pluginId: string, rawSlots: unknown[]): RegisterResult {
    const provider = String(pluginId ?? "").trim();
    const issues: SlotIssue[] = [];
    if (!provider) {
      const fault: SlotIssue = {
        severity: "error",
        code: "ERR_SLOT_INVALID",
        pluginId: "",
        field: "pluginId",
        message: "插件 id 不能为空。",
      };
      this.recordIssues([fault]);
      return { ok: false, issues: [fault] };
    }
    const nextSlots: SlotDeclaration[] = [];
    for (let index = 0; index < rawSlots.length; index += 1) {
      const normalized = normalizeSlot(rawSlots[index], provider, false, index);
      issues.push(...normalized.issues);
      if (normalized.slot) nextSlots.push(normalized.slot);
    }
    const current = this.stateSignal();
    const baseSlots = current.slots.filter((slot) => slot.provider !== provider);
    for (const slot of nextSlots) {
      const other = baseSlots.find((entry) => entry.id === slot.id);
      if (other) {
        issues.push({
          severity: "error",
          code: "ERR_SLOT_CONFLICT",
          pluginId: provider,
          field: `provides.slots.${slot.id}`,
          message: `槽位「${slot.id}」已由插件「${other.provider}」声明；一个槽位只能有一个声明者。`,
        });
      }
    }
    if (issues.some((entry) => entry.severity === "error")) {
      this.recordIssues(issues);
      return { ok: false, issues };
    }
    const mergedSlots = [...baseSlots, ...nextSlots];
    const slotsByID = new Map(mergedSlots.map((slot) => [slot.id, slot]));
    const changed = new Set(nextSlots.map((slot) => slot.id));
    const kept: MountedContribution[] = [];
    for (const entry of current.contributions) {
      if (!changed.has(entry.declaration.slot)) {
        kept.push(entry);
        continue;
      }
      const slot = slotsByID.get(entry.declaration.slot);
      if (slot && slot.forms.includes(entry.declaration.form)) {
        const targets = entry.declaration.targets.filter((target) => slot.targets.includes(target));
        if (targets.length === entry.declaration.targets.length) {
          kept.push(entry);
          continue;
        }
      }
      this.disposeMounted(entry);
      issues.push({
        severity: "warning",
        code: "ERR_FORM_NOT_ALLOWED",
        pluginId: entry.declaration.pluginId,
        field: `contributes.${entry.declaration.id}`,
        message: `槽位「${entry.declaration.slot}」的声明变了，该贡献不再合法，已撤销。`,
      });
    }
    this.stateSignal.set({ slots: mergedSlots, contributions: kept, issues });
    return { ok: true, issues };
  }

  /** Unregister everything one plugin declared. Returns the number of entries
   * removed (slots + contributions, including other plugins' contributions to
   * the removed slots). */
  unregisterPlugin(pluginId: string): number {
    const current = this.stateSignal();
    const ownSlots = current.slots.filter((slot) => slot.provider === pluginId);
    const ownSlotIDs = new Set(ownSlots.map((slot) => slot.id));
    const removed = current.contributions.filter(
      (entry) =>
        entry.declaration.pluginId === pluginId || ownSlotIDs.has(entry.declaration.slot),
    );
    for (const entry of removed) this.disposeMounted(entry);
    const slots = current.slots.filter((slot) => slot.provider !== pluginId);
    const contributions = current.contributions.filter(
      (entry) =>
        entry.declaration.pluginId !== pluginId && !ownSlotIDs.has(entry.declaration.slot),
    );
    const issues = [...current.issues];
    for (const entry of current.contributions) {
      if (entry.declaration.pluginId !== pluginId && ownSlotIDs.has(entry.declaration.slot)) {
        issues.push({
          severity: "warning",
          code: "ERR_SLOT_UNKNOWN",
          pluginId: entry.declaration.pluginId,
          field: `contributes.${entry.declaration.id}`,
          message: `槽位「${entry.declaration.slot}」随插件「${pluginId}」卸载，该贡献已一并撤销。`,
        });
      }
    }
    this.generation += 1;
    this.stateSignal.set({ slots, contributions, issues });
    this.bridges.delete(pluginId);
    return removed.length + ownSlots.length;
  }

  /** Resolve a slot for a target: what the host actually draws. */
  resolve(slotID: string, target: SlotTarget): SlotView | null {
    const current = this.stateSignal();
    const slot = current.slots.find((entry) => entry.id === slotID);
    if (!slot) return null;
    const targetSupport = slotTargetAvailable(slot.targets, target);
    if (!targetSupport.available) {
      return {
        slot,
        contributions: [],
        unavailable: [],
        unsupported: { code: targetSupport.code ?? "ERR_TARGET_UNAVAILABLE", reason: targetSupport.reason ?? "" },
      };
    }
    const all = current.contributions
      .filter((entry) => entry.declaration.slot === slotID)
      .map((entry) => this.withAvailability(entry, target));
    const runnable = all.filter((entry) => entry.status !== "unavailable" && entry.status !== "failed");
    const unavailable = all.filter((entry) => entry.status === "unavailable" || entry.status === "failed");
    if (slot.arity === "single") {
      const first = runnable[0] ?? unavailable[0];
      return {
        slot,
        contributions: first && first.status !== "unavailable" && first.status !== "failed" ? [first] : [],
        unavailable: first && (first.status === "unavailable" || first.status === "failed") ? [first] : [],
      };
    }
    return { slot, contributions: runnable, unavailable };
  }

  /** The first contribution that handles a tool call, if any. */
  toolRenderer(toolName: string, target: SlotTarget): MountedContribution | undefined {
    const view = this.resolve("keel.ui.tool-renderer", target);
    return view?.contributions.find(
      (entry) => entry.status === "ready" && (!entry.declaration.toolName || entry.declaration.toolName === toolName),
    );
  }

  /** The first contribution for a message-part kind, if any. A contribution
   * without a part kind is a wildcard for the host's own part kinds, never for
   * attachments: an attachment renderer must opt in with
   * `part = attachment`. */
  messagePart(partKind: string, target: SlotTarget): MountedContribution | undefined {
    const view = this.resolve("keel.ui.message-part", target);
    return view?.contributions.find(
      (entry) =>
        entry.status === "ready" &&
        (entry.declaration.partKind === partKind ||
          (partKind !== "attachment" && !entry.declaration.partKind)),
    );
  }

  /** The attachment preview selection for one MIME (change
   * keel-plugin-attachment, task 3.2).
   *
   * Declaration order is the inventory's order: first `part=attachment` with a
   * MIME match that is `ready` and runnable on this target. Loading entries
   * and failed/unavailable entries are reported separately so the host can
   * explain the wait or the degradation without stealing another attachment's
   * row. An entry without `part=attachment` is never considered, and an empty
   * `mime` narrowing is the wildcard **only for an attachment claim**. */
  attachmentSelection(
    mime: string,
    target: SlotTarget,
  ): {
    ready?: MountedContribution;
    loading: MountedContribution[];
    unavailable: MountedContribution[];
  } {
    const current = this.stateSignal();
    const ordered = current.contributions
      .filter((entry) => entry.declaration.slot === "keel.ui.message-part")
      .filter((entry) => entry.declaration.partKind === "attachment")
      .filter((entry) => mimeMatches(entry.declaration.mime, mime))
      .map((entry) => this.withAvailability(entry, target));
    const ready = ordered.filter((entry) => entry.status === "ready");
    const loading = ordered.filter((entry) => entry.status === "declared" || entry.status === "loading");
    const unavailable = ordered.filter((entry) => entry.status === "failed" || entry.status === "unavailable");
    return { ready: ready[0], loading, unavailable };
  }

  /** The plugin's own scoped bridge, opened with the union of its
   * contributions' grants and its declared tools. Cached: a slot re-render
   * must not mint another host scope. */
  bridgeFor(pluginId: string, tools?: string[]): ReturnType<typeof createPluginBridge> {
    const cached = this.bridges.get(pluginId);
    if (cached) return cached;
    const contributions = this.stateSignal().contributions.filter(
      (entry) => entry.declaration.pluginId === pluginId,
    );
    const capabilities = new Set<string>(["state"]);
    for (const entry of contributions) {
      for (const capability of entry.declaration.capabilities) capabilities.add(capability);
    }
    const bridge = createPluginBridge({
      pluginId,
      capabilities: [...capabilities] as never,
      tools: tools ?? this.pluginTools.get(pluginId) ?? [],
    });
    this.bridges.set(pluginId, bridge);
    return bridge;
  }

  /** Record an enabled plugin's declared tools (the bridge's own-tool
   * allowlist). Called by the inventory sync, not by plugins. */
  setPluginTools(pluginId: string, tools: string[]): void {
    const previous = this.pluginTools.get(pluginId);
    if (previous && previous.length === tools.length && previous.every((tool, index) => tool === tools[index])) {
      return;
    }
    this.pluginTools.set(pluginId, [...tools]);
    // A grant that changed needs a fresh scope next time it is asked for.
    this.bridges.delete(pluginId);
  }

  /** Reload one plugin's chunk contributions (a retry from the failure face). */
  async reloadPlugin(pluginId: string): Promise<void> {
    const current = this.stateSignal();
    for (const entry of current.contributions) {
      if (entry.declaration.pluginId !== pluginId || entry.declaration.form !== "chunk") continue;
      const slot = current.slots.find((candidate) => candidate.id === entry.declaration.slot);
      if (!slot) continue;
      this.updateContribution(entry.declaration.pluginId, entry.declaration.id, {
        status: "declared",
        module: undefined,
        code: undefined,
        reason: undefined,
      });
      void this.loadContribution(entry.declaration, slot, entry.declaration.targets);
    }
  }

  /** Declare the first-party client plugin's base slots. Idempotent. */
  registerBaseSlots(): RegisterResult {
    return this.registerPlugin({ pluginId: KEEL_UI_PLUGIN, slots: BASE_SLOTS, contributes: [] });
  }

  /** Remove every declaration (tests, a tenant switch). */
  reset(): void {
    for (const entry of this.stateSignal().contributions) this.disposeMounted(entry);
    this.generation += 1;
    this.loads.clear();
    this.pluginTools.clear();
    this.bridges.clear();
    this.stateSignal.set({ slots: [], contributions: [], issues: [] });
  }

  // ---- internals ---------------------------------------------------------

  private withAvailability(entry: MountedContribution, target: SlotTarget): MountedContribution {
    if (entry.status === "failed") return entry;
    if (entry.status === "unavailable") return entry;
    const form = formAvailability(entry.declaration.form, target);
    if (!form.available) {
      return {
        ...entry,
        status: "unavailable",
        code: form.code,
        reason: form.reason,
      };
    }
    if (!entry.declaration.targets.includes(target)) {
      return {
        ...entry,
        status: "unavailable",
        code: "ERR_TARGET_UNAVAILABLE",
        reason: `该贡献没有声明 ${target} 目标。`,
      };
    }
    return entry;
  }

  private async loadContribution(
    declaration: SlotContribution,
    slot: SlotDeclaration,
    targets: SlotTarget[],
  ): Promise<void> {
    const key = contributionKey(declaration.pluginId, declaration.id);
    const loadID = ++this.generation;
    this.loads.set(key, loadID);
    if (declaration.form === "data") {
      this.updateContribution(declaration.pluginId, declaration.id, { status: "ready" });
      return;
    }
    if (declaration.form === "chunk") {
      this.updateContribution(declaration.pluginId, declaration.id, { status: "loading" });
      const result = await loadChunkModule(declaration.entry ?? "", declaration.pluginId);
      if (this.loads.get(key) !== loadID) return;
      if (!result.module) {
        this.updateContribution(declaration.pluginId, declaration.id, {
          status: "failed",
          code: result.code,
          reason: result.message,
        });
        return;
      }
      this.updateContribution(declaration.pluginId, declaration.id, {
        status: "ready",
        module: result.module,
        code: undefined,
        reason: undefined,
      });
      return;
    }
    // package/iframe: no module to import; the slot host draws the surface.
    // `resolve` marks them unavailable per target, so a ready status here only
    // means the declaration survived validation.
    this.updateContribution(declaration.pluginId, declaration.id, { status: "ready" });
  }

  private updateContribution(
    pluginId: string,
    id: string,
    patch: Partial<MountedContribution>,
  ): void {
    const current = this.stateSignal();
    let changed = false;
    const contributions = current.contributions.map((entry) => {
      if (entry.declaration.pluginId !== pluginId || entry.declaration.id !== id) return entry;
      changed = true;
      return { ...entry, ...patch };
    });
    if (!changed) return;
    this.stateSignal.set({ ...current, contributions });
  }

  private disposeMounted(entry: MountedContribution): void {
    const key = contributionKey(entry.declaration.pluginId, entry.declaration.id);
    this.loads.delete(key);
    try {
      entry.module?.unload?.();
    } catch {
      /* a plugin's cleanup throw must not block the unload */
    }
  }

  private recordIssues(issues: SlotIssue[]): void {
    const current = this.stateSignal();
    this.stateSignal.set({ ...current, issues: [...current.issues, ...issues] });
  }
}

/** The app's registry. Modules import this; tests build their own. */
export const slots = new SlotRegistry();
