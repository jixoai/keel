// The resource share dialog — change keel-bot-identity-access, A6.
//
// One dialog serves the three shareable resources (session / Bot / TeamBot):
// it lists the current viewer/editor tuples, adds an account or permission
// group at 可查看/可编辑, and revokes with a destructive confirmation. The target
// arrives from `data/shares.ts`; the dialog never invents a tuple and re-reads
// the server's list after every write.
//
// The copy is the product's: a TeamBot share is visibility only (it never
// grants start or manage), while a session/Bot editor may write/drive. The
// dialog is composed from the kit's Dialog/AlertDialog/Select/Badge/Button; no
// local lookalike.
import { Text, View } from "nsh";
import {
  AlertDialog,
  Badge,
  Button,
  Dialog,
  Select,
  Spinner,
  color,
  radius,
  space,
  typeScale,
  weight,
} from "../ui/kit";
import { Icon } from "../ui/icons";
import {
  relationLabel,
  shareSubjectLabel,
  shares,
  type ShareCandidate,
} from "../data/shares";
import type { ShareRelation, ShareView } from "../data/identity";

const subjectTypeLabel = (share: ShareView): string =>
  share.subjectType === "account" ? "账号" : share.subjectType === "group" ? "权限组" : "团队";

const targetDescription = (): string => {
  const target = shares.target();
  if (!target) return "";
  return target.visibilityOnly
    ? "共享只影响可见性：对方能在索引里看到这个团队与成员；启动和修改仍然需要管理权。"
    : "被共享的人会在自己的工作区看到它；可编辑还能继续对话或修改。你随时可以撤销。";
};

const accountOptions = (): { value: string; title: string }[] =>
  shares.accounts().map((candidate) => ({
    value: candidate.id,
    title: candidate.detail ? `${candidate.label}（${candidate.detail}）` : candidate.label,
  }));

const groupOptions = (): { value: string; title: string }[] =>
  shares.groups().map((candidate: ShareCandidate) => ({ value: candidate.id, title: candidate.label }));

function ShareRow(props: { share: () => ShareView }) {
  const share = () => props.share();
  const revoking = () => shares.revoking() === `${share().subjectType}:${share().subjectId}:${share().relation}`;
  return (
    <View
      testID={`share-row-${share().subjectId}-${share().relation}`}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        paddingVertical: space.sm,
        borderBottomWidth: 1,
        borderColor: color.border,
      }}
    >
      <View
        style={{
          width: 30,
          height: 30,
          borderRadius: radius.md,
          alignItems: "center",
          justifyContent: "center",
          backgroundColor: color.muted,
        }}
      >
        <Icon name={share().subjectType === "group" ? "users" : "user-plus"} size={14} tint={color.mutedForeground} />
      </View>
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: space.xxs }}>
        <Text
          style={{
            fontSize: typeScale.base,
            fontWeight: weight.medium,
            color: color.foreground,
            numberOfLines: 1,
            textOverflow: "ellipsis",
          }}
        >
          {shareSubjectLabel(share())}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {subjectTypeLabel(share())}
          {share().username && share().username !== share().displayName ? ` · ${share().username}` : ""}
        </Text>
      </View>
      <Badge variant={share().relation === "editor" ? "info" : "secondary"} label={relationLabel(share().relation)} />
      <Button
        testID={`share-revoke-${share().subjectId}-${share().relation}`}
        variant="outline"
        size="sm"
        icon="x"
        label="撤销"
        accessibilityLabel={`撤销对 ${shareSubjectLabel(share())} 的共享`}
        loading={revoking()}
        disabled={shares.saving() || revoking()}
        onPress={() => shares.requestRevoke(share())}
      />
    </View>
  );
}

function ShareList() {
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
        已共享给
      </Text>
      <If when={shares.loading() && shares.shares().length === 0}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingVertical: space.md }}>
          <Spinner size={12} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取共享…</Text>
        </View>
      </If>
      <If when={!shares.loading() && shares.error() !== null}>
        <View
          testID="share-error"
          style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingVertical: space.sm }}
        >
          <Icon name="alert" size={14} tint={color.destructive} />
          <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.destructive }}>
            {shares.error()?.status === 403
              ? `没有读取共享的权限${shares.error()?.requiredKey ? `（需要 ${shares.error()?.requiredKey}）` : ""}。`
              : shares.error()?.message}
          </Text>
          <Button variant="outline" size="sm" icon="refresh" label="重试" onPress={() => void shares.load()} />
        </View>
      </If>
      <If when={!shares.loading() && shares.error() === null && shares.shares().length === 0}>
        <Text testID="share-empty" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          还没有共享给任何人。添加一个账号或权限组开始。
        </Text>
      </If>
      <If when={shares.error() === null && shares.shares().length > 0}>
        <View testID="share-list" style={{ flexDirection: "column" }}>
          <Each of={shares.shares()} key={(share) => `${share.subjectType}:${share.subjectId}:${share.relation}`}>
            {(share) => <ShareRow share={share} />}
          </Each>
        </View>
      </If>
    </View>
  );
}

function ShareAdd() {
  const options = () => (shares.subjectType() === "account" ? accountOptions() : groupOptions());
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
        添加共享
      </Text>
      <If when={shares.directoryForbidden()}>
        <View
          testID="share-directory-note"
          style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}
        >
          <Icon name="lock" size={14} tint={color.mutedForeground} />
          <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>
            你的账号不能浏览成员目录，暂时无法在这里添加新共享；已有共享仍可撤销。需要「查看账号」与「查看权限组」能力。
          </Text>
        </View>
      </If>
      <If when={!shares.directoryForbidden()}>
        <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap" }}>
          <Select
            testID="share-subject-type"
            value={shares.subjectType()}
            onChange={(value) => {
              if (value === "account" || value === "group") shares.setSubjectType(value);
            }}
            items={[
              { value: "account", title: "账号" },
              { value: "group", title: "权限组" },
            ]}
            accessibilityLabel="共享给账号还是权限组"
            style={{ width: 120 }}
          />
          <Select
            testID="share-subject"
            value={shares.subjectId()}
            onChange={shares.subjectId.set}
            items={options()}
            searchable
            searchPlaceholder={shares.subjectType() === "account" ? "搜索账号" : "搜索权限组"}
            placeholder={shares.directoryLoading() ? "读取目录…" : "选择对象"}
            disabled={shares.directoryLoading() || options().length === 0}
            accessibilityLabel="选择共享对象"
            style={{ flex: 1, minWidth: 180 }}
          />
          <Select
            testID="share-relation"
            value={shares.relation()}
            onChange={(value) => {
              if (value === "viewer" || value === "editor") shares.relation.set(value as ShareRelation);
            }}
            items={[
              { value: "viewer", title: "可查看" },
              { value: "editor", title: "可编辑" },
            ]}
            accessibilityLabel="共享关系"
            style={{ width: 120 }}
          />
          <Button
            testID="share-add"
            icon="plus"
            label="添加"
            loading={shares.saving()}
            disabled={!shares.subjectId() || shares.saving()}
            onPress={() => void shares.add()}
          />
        </View>
        <If when={shares.directoryError() !== null}>
          <Text style={{ fontSize: typeScale.sm, color: color.destructive }}>{shares.directoryError()}</Text>
        </If>
        <If when={!shares.directoryLoading() && options().length === 0 && shares.directoryError() === null}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {shares.subjectType() === "account" ? "没有其他可共享的账号。" : "还没有权限组。"}
          </Text>
        </If>
      </If>
      <If when={shares.actionError() !== null}>
        <Text testID="share-action-error" style={{ fontSize: typeScale.sm, color: color.destructive }}>
          {shares.actionError()}
        </Text>
      </If>
      <If when={shares.notice() !== null}>
        <View testID="share-notice" style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
          <Icon name="check" size={12} tint={color.success} />
          <Text style={{ fontSize: typeScale.sm, color: color.success }}>{shares.notice()}</Text>
        </View>
      </If>
    </View>
  );
}

/** The one dialog, mounted at the app root so the workspace header and the
 * books chapters share it. */
export function ShareDialog() {
  const target = () => shares.target();
  return (
    <>
      <If when={target() !== null}>
        <Dialog
          testID="share-dialog"
          open
          size="lg"
          title={target() ? `共享「${target()!.name}」` : "共享"}
          description={targetDescription()}
          onOpenChange={(event) => {
            if (!event.open) shares.close();
          }}
          footer={
            <Button
              testID="share-dialog-close"
              variant="outline"
              label="关闭"
              accessibilityLabel="关闭共享对话框"
              onPress={() => shares.close()}
            />
          }
        >
          <View style={{ flexDirection: "column", gap: space.lg }}>
            <ShareList />
            <ShareAdd />
          </View>
        </Dialog>
      </If>
      <If when={target() !== null}>
        <AlertDialog
          testID="share-revoke-dialog"
          open={shares.revokeCandidate() !== null}
          title="撤销这条共享？"
          description={
            shares.revokeCandidate()
              ? `「${shareSubjectLabel(shares.revokeCandidate()!)}」将立即失去这条共享；对方的下一次请求会被拒绝。`
              : ""
          }
          confirmLabel="撤销"
          cancelLabel="取消"
          destructive
          onConfirm={() => void shares.confirmRevoke()}
          onCancel={() => shares.cancelRevoke()}
        />
      </If>
    </>
  );
}
