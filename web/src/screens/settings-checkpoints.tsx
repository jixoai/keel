// The settings page's「检查点」section (change keel-checkpoint).
//
// A checkpoint is a projection over the event log, not a second transcript.
// This section edits the one policy a session is allowed to have: a narrowing
// override of the tenant ceiling. It reads and writes
// /v1/sessions/{id}/checkpoint-policy through the same transport as the rest of
// the settings page.
//
// The states the section draws (and does):
//   - loading: skeletons while the first read is in flight;
//   - empty: no visible session yet, with the one next action (create/open one);
//   - configured: tenant policy, stored override, effective policy and every
//     field the tenant clamped; editable inputs start from the stored override
//     (empty means inherit), never from the clamped effective value;
//   - busy: a save in flight disables the form; the server's refusal stays on
//     screen with its status/code;
//   - unavailable: the 501/503 "not wired", the 403 permission face and the
//     network face share one readable error block with a retry.
//
// TUI: the section is a readable text summary first (the effective policy in
// one sentence), then the same fields; no hover, drag or color-only state is
// required. The TUI test presses the section rail and asserts the summary.
import { effect, signal, Text, View } from "nsh";
import type { CheckpointPolicyNarrowing, CheckpointPolicyUpdate, CheckpointPolicyValues, CheckpointPolicyView } from "../data/contract";
import { store } from "../data/store";
import { can } from "../data/permissions";
import { sessionTitle } from "../ui/session-title";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Field,
  FieldGroup,
  Input,
  Select,
  Separator,
  Skeleton,
  color,
  lineHeight,
  space,
  typeScale,
  weight,
  type SelectItem,
} from "../ui/kit";
import { fieldHeightProp, isMobile, touchStyle } from "./books/common";

// ---- the draft (module-level, like every other settings section) ----------

/** Booleans are tri-state on the wire: an empty value inherits the tenant
 * policy; "true"/"false" store an explicit narrowing. */
const draftEnabled = signal("");
const draftManual = signal("");
const draftThreshold = signal("");
const draftRetain = signal("");
const draftMax = signal("");
const draftTTL = signal("");

const triStateItems: SelectItem[] = [
  { value: "", title: "继承租户策略" },
  { value: "true", title: "开启" },
  { value: "false", title: "关闭" },
];

/** One policy in one sentence: the same text serves tenant and effective so a
 * reader can compare them line by line (TUI included — no color carries it). */
const policySummary = (p: CheckpointPolicyValues): string => {
  const toggle = (value: boolean) => (value ? "开" : "关");
  return [
    `自动压缩 ${toggle(p.enabled)}`,
    `手动压缩 ${toggle(p.manualCompact)}`,
    `保留 ${p.retainTokens} tokens`,
    `最多 ${p.maxCheckpoints === 0 ? "不限" : `${p.maxCheckpoints} 个`}`,
    `TTL ${p.ttlHours === 0 ? "不过期" : `${p.ttlHours} 小时`}`,
    p.thresholdTokens === 0 ? "阈值按窗口推导" : `阈值 ${p.thresholdTokens} tokens`,
  ].join(" · ");
};

/** The stored session document, field by field. An omitted field inherits the
 * tenant value; the override is shown separately from the clamped effective
 * policy so a reader sees the request and the result. */
const overrideSummary = (override?: CheckpointPolicyUpdate): string => {
  if (!override) return "未设置（完全继承租户策略）";
  const toggle = (value: boolean) => (value ? "开" : "关");
  const parts: string[] = [];
  if (override.enabled !== undefined) parts.push(`自动压缩 ${toggle(override.enabled)}`);
  if (override.manualCompact !== undefined) parts.push(`手动压缩 ${toggle(override.manualCompact)}`);
  if (override.thresholdTokens !== undefined) parts.push(`阈值 ${override.thresholdTokens} tokens`);
  if (override.retainTokens !== undefined) parts.push(`保留 ${override.retainTokens} tokens`);
  if (override.maxCheckpoints !== undefined) {
    parts.push(override.maxCheckpoints === 0 ? "最多 不限" : `最多 ${override.maxCheckpoints} 个`);
  }
  if (override.ttlHours !== undefined) {
    parts.push(override.ttlHours === 0 ? "TTL 不过期" : `TTL ${override.ttlHours} 小时`);
  }
  return parts.length > 0 ? parts.join(" · ") : "未设置（完全继承租户策略）";
};

const fieldLabel: Record<string, string> = {
  enabled: "自动压缩",
  manualCompact: "手动压缩",
  thresholdTokens: "压缩阈值",
  retainTokens: "保留 token",
  maxCheckpoints: "最大检查点数",
  // The server's narrowing vocabulary is "ttl" (the wire document says
  // "ttlHours"); keep both keys so a reason is never printed as a raw id.
  ttl: "过期时间",
  ttlHours: "过期时间",
};

const narrowedText = (row: CheckpointPolicyNarrowing): string =>
  `${fieldLabel[row.field] ?? row.field}：${row.reason || "已被租户上限收窄"}`;

const numberValue = (text: string): number | undefined => {
  const trimmed = text.trim();
  if (!trimmed) return undefined;
  const value = Number(trimmed);
  if (!Number.isFinite(value) || value < 0) return undefined;
  return Math.floor(value);
};

const numberDraft = (value: number | undefined): string => (value === undefined ? "" : String(value));

/** The PUT body: only fields the reader filled in. An empty object clears the
 * override and returns the session to the tenant policy. */
const draftUpdate = (): CheckpointPolicyUpdate => {
  const update: CheckpointPolicyUpdate = {};
  if (draftEnabled() === "true") update.enabled = true;
  if (draftEnabled() === "false") update.enabled = false;
  if (draftManual() === "true") update.manualCompact = true;
  if (draftManual() === "false") update.manualCompact = false;
  const threshold = numberValue(draftThreshold());
  const retain = numberValue(draftRetain());
  const max = numberValue(draftMax());
  const ttl = numberValue(draftTTL());
  if (threshold !== undefined) update.thresholdTokens = threshold;
  if (retain !== undefined) update.retainTokens = retain;
  if (max !== undefined) update.maxCheckpoints = max;
  if (ttl !== undefined) update.ttlHours = ttl;
  return update;
};

const loadDraft = (view: CheckpointPolicyView | null) => {
  const override = view?.override;
  draftEnabled.set(override?.enabled === undefined ? "" : override.enabled ? "true" : "false");
  draftManual.set(override?.manualCompact === undefined ? "" : override.manualCompact ? "true" : "false");
  draftThreshold.set(numberDraft(override?.thresholdTokens));
  draftRetain.set(numberDraft(override?.retainTokens));
  draftMax.set(numberDraft(override?.maxCheckpoints));
  draftTTL.set(numberDraft(override?.ttlHours));
};

// ---- the faces ------------------------------------------------------------

function PolicyRows(props: { view: CheckpointPolicyView }) {
  const view = () => props.view;
  return (
    <View style={{ gap: space.sm }} testID="settings-checkpoint-effective">
      <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
        {`生效策略：${policySummary(view().effective)}`}
      </Text>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {`租户默认：${policySummary(view().tenant)}`}
      </Text>
      <Text
        style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
        testID="settings-checkpoint-override"
      >
        {`会话覆盖：${overrideSummary(view().override)}`}
      </Text>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {`来源：${view().source === "session" ? "会话覆盖（已按租户上限计算）" : "租户默认"} · 版本 ${
          view().effective.policyVersion || "—"
        }`}
      </Text>
      <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
        <Badge variant="secondary" label={`租户：最多 ${view().tenant.maxCheckpoints || "不限"} · 保留 ${view().tenant.retainTokens}`} />
        <Badge
          variant={view().effective.enabled ? "success" : "secondary"}
          label={`自动压缩 ${view().effective.enabled ? "开" : "关"}`}
        />
        <Badge
          variant={view().effective.manualCompact ? "success" : "secondary"}
          label={`手动压缩 ${view().effective.manualCompact ? "开" : "关"}`}
        />
      </View>
      <If when={view().narrowed.length > 0}>
        <View style={{ gap: space.xs }} testID="settings-checkpoint-narrowed">
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.warning }}>
            被租户上限收窄
          </Text>
          {view().narrowed.map((row) => (
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
              {narrowedText(row)}
            </Text>
          ))}
        </View>
      </If>
    </View>
  );
}

function ErrorBlock() {
  const status = () => store.checkpointPolicyErrorStatus();
  const title = () =>
    status() === 501 || status() === 503
      ? "检查点策略服务未接线"
      : status() === 403
        ? "没有读取该会话策略的权限"
        : status() === 404
          ? "会话不存在或已不可见"
          : "检查点策略读取失败";
  return (
    <View
      testID="settings-checkpoint-error"
      accessibilityLabel="检查点策略读取失败"
      style={{
        gap: space.md,
        padding: space.lg,
        borderRadius: 12,
        borderWidth: 1,
        borderColor: color.danger,
        backgroundColor: color.dangerTint,
      }}
    >
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>{title()}</Text>
      <Text
        style={{
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.normal),
          color: color.foreground,
        }}
      >
        {store.checkpointPolicyError() ?? ""}
      </Text>
      <If when={status() !== null}>
        <Badge variant="secondary" label={`status: ${status()}`} />
      </If>
      <View style={{ flexDirection: "row", gap: space.sm }}>
        <Button
          size="sm"
          variant="outline"
          icon="refresh"
          label="重试"
          testID="settings-checkpoint-retry"
          onPress={() => void store.loadCheckpointPolicy()}
          style={touchStyle()}
        />
      </View>
    </View>
  );
}

function SessionsEmpty() {
  return (
    <Empty testID="settings-checkpoint-empty" accessibilityLabel="还没有可配置的会话">
      <EmptyMedia icon="message" tone="brand" />
      <EmptyTitle label="还没有可配置的会话" />
      <EmptyDescription label="检查点策略按会话生效。先在工作区新建或打开一个会话，再回到这里收紧保留窗口或过期时间。" />
      <EmptyContent>
        <Button
          size="sm"
          variant="outline"
          icon="refresh"
          label="刷新会话列表"
          onPress={() => {
            // Read the list first: the policy needs a session to project.
            void (async () => {
              await store.refreshSessions();
              await store.loadCheckpointPolicy();
            })();
          }}
          style={touchStyle()}
        />
        <Button size="sm" label="返回工作区" onPress={() => store.closeSettings()} style={touchStyle()} />
      </EmptyContent>
    </Empty>
  );
}

function PermissionEmpty(props: { write: boolean }) {
  return (
    <Empty testID="settings-checkpoint-permission" accessibilityLabel="没有检查点策略权限">
      <EmptyMedia icon="lock" tone="neutral" />
      <EmptyTitle label={props.write ? "只读：缺少 session.write" : "缺少 session.read"} />
      <EmptyDescription
        label={
          props.write
            ? "你可以查看当前生效策略，但保存收窄需要 session.write；服务端仍会独立校验每一次写入。"
            : "该账号没有读取会话策略的权限；请联系管理员授予 session.read。"
        }
      />
    </Empty>
  );
}

// ---- the section ----------------------------------------------------------

export function CheckpointsSection() {
  // One read per entry and per session change. The draft follows the stored
  // override (not the clamped effective policy): leaving a field empty is how
  // a reader keeps inheriting the tenant value.
  effect(() => {
    const view = store.checkpointPolicy();
    loadDraft(view);
  });

  const view = () => store.checkpointPolicy();
  // A session is known but the first projection has not landed: keep the
  // skeleton instead of a silently empty section (the effect runs below).
  const loading = () =>
    view() === null &&
    !store.checkpointPolicyError() &&
    (store.checkpointPolicyLoading() || store.sessions().length > 0);
  const writable = () => can("session.write");
  const hasSessions = () => store.sessions().length > 0;
  const sessionItems = (): SelectItem[] =>
    store.sessions().map((row) => ({
      value: row.sessionId,
      title: sessionTitle(row),
    }));

  const save = async () => {
    await store.saveCheckpointPolicy(draftUpdate());
  };
  const reset = async () => {
    if (await store.saveCheckpointPolicy({})) loadDraft(store.checkpointPolicy());
  };

  return (
    <Card testID="settings-checkpoint-section">
      <CardHeader
        title="检查点"
        description="检查点是压缩后的可恢复投影，事件日志仍是唯一真值。这里按会话收窄自动/手动压缩、保留窗口、检查点数量与过期时间；只会比租户上限更严，不会更宽。"
      />
      <CardContent>
        <View style={{ gap: space.lg }}>
          <If when={!can("session.read")} else={<View />}>
            <PermissionEmpty write={false} />
          </If>
          <If when={can("session.read")}>
            <If when={!hasSessions() && !loading()} else={<View />}>
              <SessionsEmpty />
            </If>
            <If when={hasSessions() || loading()}>
              <View style={{ flexDirection: isMobile() ? "column" : "row", gap: space.md, alignItems: isMobile() ? "stretch" : "center" }}>
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Field label="会话" hint="策略按会话生效；默认打开当前会话，也可以换一个。">
                    <Select
                      testID="settings-checkpoint-session"
                      accessibilityLabel="选择检查点策略会话"
                      items={sessionItems()}
                      value={store.checkpointPolicySessionId()}
                      placeholder="选择会话"
                      size="field"
                      height={fieldHeightProp()}
                      disabled={store.checkpointPolicyLoading()}
                      onChange={(value) => {
                        if (value) store.selectCheckpointPolicySession(value);
                      }}
                    />
                  </Field>
                </View>
                <Button
                  testID="settings-checkpoint-refresh"
                  size="sm"
                  variant="outline"
                  icon="refresh"
                  label="刷新策略"
                  loading={store.checkpointPolicyLoading()}
                  disabled={store.checkpointPolicyLoading()}
                  onPress={() => void store.loadCheckpointPolicy()}
                  style={touchStyle()}
                />
              </View>
            </If>
            <If when={loading()}>
              <Skeleton lines={5} accessibilityLabel="正在读取检查点策略" testID="settings-checkpoint-loading" />
            </If>
            <If when={!!store.checkpointPolicyError() && !loading() && !view()}>
              <ErrorBlock />
            </If>
            <If when={!!view() && !loading()}>
              <View style={{ gap: space.lg }}>
                <PolicyRows view={view()!} />
                <Separator />
                <If when={!writable()} else={<View />}>
                  <PermissionEmpty write />
                </If>
                <View style={{ gap: space.md }} testID="settings-checkpoint-form">
                  <FieldGroup>
                    <Field
                      label="自动压缩"
                      hint={`租户当前：${view()!.tenant.enabled ? "开启" : "关闭"}；会话只能关闭，不能打开租户关闭的能力。`}
                    >
                      <Select
                        testID="settings-checkpoint-enabled"
                        accessibilityLabel="自动压缩"
                        items={triStateItems}
                        value={draftEnabled()}
                        onChange={(value) => draftEnabled.set(value ?? "")}
                        disabled={!writable() || store.checkpointPolicySaving()}
                        size="field"
                        height={fieldHeightProp()}
                      />
                    </Field>
                    <Field
                      label="手动压缩"
                      hint={`租户当前：${view()!.tenant.manualCompact ? "允许" : "禁止"}；与自动压缩开关相互独立。`}
                    >
                      <Select
                        testID="settings-checkpoint-manual"
                        accessibilityLabel="手动压缩"
                        items={triStateItems}
                        value={draftManual()}
                        onChange={(value) => draftManual.set(value ?? "")}
                        disabled={!writable() || store.checkpointPolicySaving()}
                        size="field"
                        height={fieldHeightProp()}
                      />
                    </Field>
                    <Field
                      label="压缩阈值（tokens）"
                      hint={`留空继承租户；0 表示由上下文窗口推导。当前生效：${
                        view()!.effective.thresholdTokens || "按窗口推导"
                      }。`}
                    >
                      <Input
                        testID="settings-checkpoint-threshold"
                        accessibilityLabel="压缩阈值 tokens"
                        value={draftThreshold()}
                        placeholder="留空继承"
                        size="field"
                        height={fieldHeightProp()}
                        disabled={!writable() || store.checkpointPolicySaving()}
                        onChange={(value) => draftThreshold.set(value)}
                      />
                    </Field>
                    <Field
                      label="保留 token"
                      hint={`压缩后保留的近似 tail。当前生效：${view()!.effective.retainTokens}；0 表示不额外保留。`}
                    >
                      <Input
                        testID="settings-checkpoint-retain"
                        accessibilityLabel="保留 token"
                        value={draftRetain()}
                        placeholder="留空继承"
                        size="field"
                        height={fieldHeightProp()}
                        disabled={!writable() || store.checkpointPolicySaving()}
                        onChange={(value) => draftRetain.set(value)}
                      />
                    </Field>
                    <Field
                      label="最大检查点数"
                      hint={`当前生效：${
                        view()!.effective.maxCheckpoints || "不限"
                      }；0 表示不限（会被租户上限收窄）。`}
                    >
                      <Input
                        testID="settings-checkpoint-max"
                        accessibilityLabel="最大检查点数"
                        value={draftMax()}
                        placeholder="留空继承"
                        size="field"
                        height={fieldHeightProp()}
                        disabled={!writable() || store.checkpointPolicySaving()}
                        onChange={(value) => draftMax.set(value)}
                      />
                    </Field>
                    <Field
                      label="过期时间（小时）"
                      hint={`当前生效：${
                        view()!.effective.ttlHours === 0 ? "不过期" : `${view()!.effective.ttlHours} 小时`
                      }；最近一个有效检查点始终保留。`}
                    >
                      <Input
                        testID="settings-checkpoint-ttl"
                        accessibilityLabel="过期时间小时"
                        value={draftTTL()}
                        placeholder="留空继承"
                        size="field"
                        height={fieldHeightProp()}
                        disabled={!writable() || store.checkpointPolicySaving()}
                        onChange={(value) => draftTTL.set(value)}
                      />
                    </Field>
                  </FieldGroup>
                  <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.sm }}>
                    <Button
                      testID="settings-checkpoint-save"
                      label="保存会话策略"
                      icon="check"
                      loading={store.checkpointPolicySaving()}
                      disabled={!writable() || store.checkpointPolicySaving()}
                      onPress={() => void save()}
                      style={touchStyle()}
                    />
                    <Button
                      testID="settings-checkpoint-reset"
                      label="恢复租户默认"
                      icon="refresh"
                      variant="outline"
                      disabled={!writable() || store.checkpointPolicySaving() || view()!.source === "tenant"}
                      onPress={() => void reset()}
                      style={touchStyle()}
                    />
                  </View>
                  <If when={store.checkpointPolicySaving()}>
                    <Text
                      testID="settings-checkpoint-saving"
                      style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
                    >
                      正在保存会话策略…
                    </Text>
                  </If>
                  {/* A save refusal keeps the current view so the reader can
                      correct the field; its HTTP status stays visible too. */}
                  <If when={!!store.checkpointPolicyError()}>
                    <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs, alignItems: "center" }}>
                      <Text
                        role="alert"
                        accessibilityLabel="检查点策略保存失败"
                        testID="settings-checkpoint-save-error"
                        style={{ fontSize: typeScale.sm, color: color.danger }}
                      >
                        {store.checkpointPolicyError()}
                      </Text>
                      <If when={store.checkpointPolicyErrorStatus() !== null}>
                        <Badge
                          variant="secondary"
                          label={`status: ${store.checkpointPolicyErrorStatus()}`}
                        />
                      </If>
                    </View>
                  </If>
                </View>
              </View>
            </If>
          </If>
        </View>
      </CardContent>
    </Card>
  );
}
