// The settings page's「技能」section (change keel-harness-v2, W7 skills).
//
// One tenant-scoped library, three faces: this page, `keel skills`, and the
// agent's `skills manage` action. The page reads and writes the same registry
// through /v1/settings/skills*, so what an operator sees here is what the next
// run's `skills ls` answers.
//
// The states the section must draw (and does):
//   - loading: skeletons while the first read is in flight;
//   - empty: no skills yet, with the one next action (create one, or subscribe
//     a source and install from it);
//   - configured: one card per skill — name/description, trust, current and
//     pinned version, update availability, and the conflict code when the
//     registry cannot be trusted as-is (pinned, modified, source_missing,
//     name_mismatch, missing_active);
//   - busy: a write in flight disables the row's controls; the server's
//     refusal stays on screen, verbatim, with its stable code;
//   - unavailable: the 501 face ("the library is not wired"), the 403 face
//     (permission), and the network face share one readable error block with a
//     retry.
//
// Scripts are not run from here (or anywhere in the library): a resource named
// scripts/deploy.sh is bytes. The page says so once, and the actual execution
// path is the bash tool.
import { Scroll, signal, Text, View } from "nsh";
import type { SkillRecordView, SkillSourceView } from "../data/contract";
import { store } from "../data/store";
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  Dialog,
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
  Textarea,
  color,
  font,
  lineHeight,
  space,
  typeScale,
  weight,
  type SelectItem,
} from "../ui/kit";

// ---- form state (module-level, like the other settings sections) ----------

const newSkillName = signal("");
const newSkillDescription = signal("");
const newSkillBody = signal("");
const newSourceId = signal("");
const newSourceURL = signal("");
const newSourceTrust = signal("trusted");
const confirmRemoveSkill = signal<string | null>(null);
const confirmRemoveSource = signal<string | null>(null);

const trustLabel = (trust: string | undefined): string => {
  switch (trust) {
    case "trusted":
      return "受信任";
    case "untrusted":
      return "未信任";
    case "local":
      return "本地";
    default:
      return "未知来源";
  }
};

const trustVariant = (trust: string | undefined): "success" | "warning" | "secondary" => {
  if (trust === "trusted") return "success";
  if (trust === "untrusted") return "warning";
  return "secondary";
};

/** The conflict is a sentence, not a code: the operator's next action is the
 * difference between "unpin" and "fix the source". */
const conflictText = (conflict: string | undefined): string => {
  switch (conflict) {
    case "pinned":
      return "已固定：源里有新版本，但不会自动更新";
    case "modified":
      return "当前副本与版本内容不一致（可能被手动改动）";
    case "source_missing":
      return "订阅源不可用，无法检查更新";
    case "name_mismatch":
      return "源里的技能改了名字，更新被拒绝";
    case "missing_active":
      return "当前副本缺失，内容仍可从不可变版本读取";
    default:
      return "";
  }
};

const shortHash = (hash: string | undefined): string => {
  if (!hash) return "-";
  return hash.length > 12 ? hash.slice(0, 12) : hash;
};

const trustItems = (): SelectItem[] => [
  { value: "trusted", title: "受信任：来源可信，安装后可直接更新" },
  { value: "untrusted", title: "未信任：每次安装/更新都需要显式确认" },
];

function LoadingRows() {
  return (
    <View style={{ gap: space.lg }}>
      <Skeleton lines={2} accessibilityLabel="正在读取技能" testID="settings-skills-loading" />
      <Skeleton lines={2} accessibilityLabel="正在读取订阅源" />
    </View>
  );
}

function ErrorBlock(props: { text: string; code?: string | null; onRetry: () => void }) {
  return (
    <View
      testID="settings-skills-error"
      accessibilityLabel="技能库读取失败"
      style={{
        gap: space.md,
        padding: space.lg,
        borderRadius: 12,
        borderWidth: 1,
        borderColor: color.danger,
        backgroundColor: color.dangerTint,
      }}
    >
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
        技能库读取失败
      </Text>
      <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.foreground }}>
        {props.text}
      </Text>
      <If when={!!props.code}>
        <Badge variant="secondary" label={`code: ${props.code ?? ""}`} />
      </If>
      <View style={{ flexDirection: "row", gap: space.sm }}>
        <Button size="sm" variant="outline" icon="refresh" label="重试" onPress={props.onRetry} />
      </View>
    </View>
  );
}

function SkillRow(props: { skill: SkillRecordView }) {
  const skill = () => props.skill;
  const pinned = () => !!skill().pinnedHash;
  const latest = () => (skill().versions ?? [])[0];
  return (
    <View
      testID={`skill-card-${skill().name}`}
      accessibilityLabel={`技能 ${skill().name}`}
      style={{
        gap: space.md,
        padding: space.lg,
        borderRadius: 12,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.card,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}>
        <View style={{ flex: 1, gap: space.xs }}>
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
            {skill().name}
          </Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
            {skill().description || "（没有描述）"}
          </Text>
        </View>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, flexWrap: "wrap" }}>
          <Badge
            variant={trustVariant(skill().trust)}
            icon={skill().trust === "trusted" ? "check" : skill().trust === "untrusted" ? "alert" : "file"}
            label={trustLabel(skill().trust)}
          />
          <Badge variant="secondary" label={`当前 ${shortHash(skill().currentHash)}`} />
          <If when={pinned()}>
            <Badge variant="info" icon="key" label={`固定 ${shortHash(skill().pinnedHash)}`} />
          </If>
          <If when={!!skill().updateAvailable}>
            <Badge variant="warning" icon="arrow-up" label={`可更新 ${shortHash(skill().availableHash)}`} />
          </If>
          <If when={!!skill().conflict}>
            <Badge variant="destructive" icon="alert" label={conflictText(skill().conflict)} />
          </If>
        </View>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          {`${skill().versions?.length ?? 0} 个版本 · ${skill().fileCount ?? latest()?.files ?? 0} 个文件 · ${
            skill().bytes ?? latest()?.bytes ?? 0
          } 字节`}
        </Text>
        <View style={{ flex: 1 }} />
        <Button
          testID={`skill-view-${skill().name}`}
          size="sm"
          variant="ghost"
          icon="file"
          label="查看"
          accessibilityLabel={`查看技能 ${skill().name}`}
          onPress={() => void store.openSkillDetail(skill().name)}
        />
        <Button
          testID={`skill-update-${skill().name}`}
          size="sm"
          variant="outline"
          icon="arrow-up"
          label="更新"
          accessibilityLabel={`更新技能 ${skill().name}`}
          loading={store.skillsBusy()}
          disabled={!skill().updateAvailable || store.skillsBusy()}
          onPress={() => void store.updateSkill(skill().name)}
        />
        <If
          when={pinned()}
          else={
            <Button
              testID={`skill-pin-${skill().name}`}
              size="sm"
              variant="outline"
              icon="key"
              label="固定当前版本"
              accessibilityLabel={`固定技能 ${skill().name} 的当前版本`}
              disabled={store.skillsBusy()}
              onPress={() => void store.pinSkill(skill().name)}
            />
          }
        >
          <Button
            testID={`skill-unpin-${skill().name}`}
            size="sm"
            variant="outline"
            icon="x"
            label="解除固定"
            accessibilityLabel={`解除技能 ${skill().name} 的固定`}
            disabled={store.skillsBusy()}
            onPress={() => void store.unpinSkill(skill().name)}
          />
        </If>
        <Button
          testID={`skill-remove-${skill().name}`}
          size="sm"
          variant="destructive"
          icon="trash"
          label="移除"
          accessibilityLabel={`移除技能 ${skill().name}`}
          disabled={store.skillsBusy()}
          onPress={() => confirmRemoveSkill.set(skill().name)}
        />
      </View>
      <If when={(skill().diagnostics ?? []).length > 0}>
        <View style={{ gap: space.xxs }}>
          {(skill().diagnostics ?? []).map((line) => (
            <Text style={{ fontSize: typeScale.xs, color: color.warning }}>{line}</Text>
          ))}
        </View>
      </If>
    </View>
  );
}

function SourceRow(props: { source: SkillSourceView }) {
  const source = () => props.source;
  return (
    <View
      testID={`skill-source-${source().id}`}
      accessibilityLabel={`订阅源 ${source().id}`}
      style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.sm }}
    >
      <View style={{ flex: 1, gap: space.xxs }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
          {source().id}
        </Text>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, numberOfLines: 1 }}>
          {`${source().kind} · ${source().url}`}
        </Text>
      </View>
      <Badge variant={trustVariant(source().trust)} label={trustLabel(source().trust)} />
      <Button
        testID={`skill-source-remove-${source().id}`}
        size="sm"
        variant="ghost"
        label="取消订阅"
        disabled={store.skillsBusy()}
        onPress={() => confirmRemoveSource.set(source().id)}
      />
    </View>
  );
}

function DetailDialog() {
  const detail = () => store.skillsDetail();
  const open = () =>
    !!detail() || store.skillsDetailLoading() || !!store.skillsDetailError();
  return (
    <Dialog
      testID="skill-detail-dialog"
      open={open()}
      size="lg"
      title={detail()?.name ?? "技能详情"}
      description="SKILL.md 正文与资源清单。脚本不会在这里执行：需要运行时用 bash 工具，沙箱、作业与输出上限都由 bash 策略负责。"
      onOpenChange={(event) => {
        if (!event.open) store.closeSkillDetail();
      }}
      footer={
        <View style={{ flexDirection: "row", justifyContent: "flex-end" }}>
          <Button variant="ghost" label="关闭" onPress={() => store.closeSkillDetail()} />
        </View>
      }
    >
      <View style={{ gap: space.lg }}>
        <If when={store.skillsDetailLoading()}>
          <Skeleton lines={4} accessibilityLabel="正在读取技能详情" testID="skill-detail-loading" />
        </If>
        <If when={!!store.skillsDetailError()}>
          <ErrorBlock
            text={store.skillsDetailError() ?? ""}
            onRetry={() => {
              const name = store.skillsDetailName();
              if (name) void store.openSkillDetail(name);
            }}
          />
        </If>
        <If when={!!detail()}>
          <View style={{ flexDirection: "row", gap: space.xs, flexWrap: "wrap" }}>
            <Badge variant={trustVariant(detail()?.trust)} label={trustLabel(detail()?.trust)} />
            <Badge variant="secondary" label={`当前 ${shortHash(detail()?.currentHash)}`} />
            <If when={!!detail()?.pinnedHash}>
              <Badge variant="info" label={`固定 ${shortHash(detail()?.pinnedHash)}`} />
            </If>
            <If when={!!detail()?.sourceId}>
              <Badge variant="outline" label={`来源 ${detail()?.sourceId ?? ""}`} />
            </If>
          </View>
          <Scroll style={{ maxHeight: 220 }}>
            <Text
              testID="skill-detail-body"
              style={{
                fontFamily: font.mono,
                fontSize: typeScale.xs,
                lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                color: color.foreground,
                userSelect: "text",
              }}
            >
              {detail()?.body ?? ""}
            </Text>
          </Scroll>
          <Separator />
          <View style={{ gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
              资源
            </Text>
            {(detail()?.files ?? []).map((file) => (
              <View
                style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.xxs }}
              >
                <Text style={{ flex: 1, fontFamily: font.mono, fontSize: typeScale.xs, color: color.foreground }}>
                  {file.path}
                </Text>
                <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{`${file.size} 字节`}</Text>
              </View>
            ))}
          </View>
          <Separator />
          <View style={{ gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
              版本（不可变）
            </Text>
            {(detail()?.versions ?? []).map((version) => (
              <View
                style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.xxs }}
              >
                <Text style={{ flex: 1, fontFamily: font.mono, fontSize: typeScale.xs, color: color.foreground }}>
                  {version.hash}
                </Text>
                <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                  {`${version.files ?? 0} 个文件 · ${version.bytes ?? 0} 字节`}
                </Text>
                <If when={version.hash === detail()?.currentHash}>
                  <Badge variant="success" label="当前" />
                </If>
                <If when={version.hash === detail()?.pinnedHash}>
                  <Badge variant="info" label="固定" />
                </If>
              </View>
            ))}
          </View>
        </If>
      </View>
    </Dialog>
  );
}

export function SkillsSection() {
  const view = () => store.skills();
  const skills = () => view()?.skills ?? [];
  const sources = () => view()?.sources ?? [];
  const saveSkill = () => {
    const name = newSkillName().trim();
    const description = newSkillDescription().trim();
    if (!name || !description) {
      store.flash({ tone: "warn", text: "技能名字和描述都不能为空" });
      return;
    }
    void store.installSkillFromForm(name, description, newSkillBody()).then((ok) => {
      if (!ok) return;
      newSkillName.set("");
      newSkillDescription.set("");
      newSkillBody.set("");
    });
  };
  const subscribe = () => {
    const id = newSourceId().trim();
    const url = newSourceURL().trim();
    if (!id || !url) {
      store.flash({ tone: "warn", text: "订阅源的名字和目录都不能为空" });
      return;
    }
    void store.subscribeSkillSource(id, url, newSourceTrust()).then((ok) => {
      if (!ok) return;
      newSourceId.set("");
      newSourceURL.set("");
    });
  };
  const actionError = () => store.skillsActionError();

  return (
    <View style={{ gap: space["3xl"] }}>
      <Card testID="settings-skills">
        <CardHeader
          title="技能"
          description="技能是带资源的 SKILL.md：每次保存产生一个不可变版本，按内容哈希去重。技能里的脚本不会在技能库执行，运行时交给 bash 工具（沙箱、作业与输出上限都在那里）。"
          action={
            <Button
              testID="settings-skills-refresh"
              size="sm"
              variant="outline"
              icon="refresh"
              label="检查更新"
              loading={store.skillsLoading()}
              disabled={store.skillsLoading()}
              onPress={() => void store.loadSkillsPanel(true)}
            />
          }
        />
        <CardContent>
          <If when={store.skillsLoading() && !view()}>
            <LoadingRows />
          </If>
          <If when={!!store.skillsError()}>
            <ErrorBlock
              text={store.skillsError() ?? ""}
              code={store.skillsErrorCode()}
              onRetry={() => void store.loadSkillsPanel(true)}
            />
          </If>
          <If when={!store.skillsLoading() && !store.skillsError() && skills().length === 0}>
            <Empty accessibilityLabel="还没有技能">
              <EmptyMedia icon="spark" tone="brand" />
              <EmptyContent>
                <EmptyTitle label="还没有技能" />
                <EmptyDescription label="在下方向 Agent 描述一条可复用的做法；或订阅一个本地技能目录后从那里安装。" />
              </EmptyContent>
            </Empty>
          </If>
          <View style={{ gap: space.md }}>
            {skills().map((skill) => (
              <SkillRow skill={skill} />
            ))}
          </View>
          <If when={!!actionError()}>
            <View
              testID="settings-skills-action-error"
              style={{
                gap: space.xs,
                padding: space.md,
                borderRadius: 12,
                borderWidth: 1,
                borderColor: color.danger,
                backgroundColor: color.dangerTint,
              }}
            >
              <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{actionError()?.message ?? ""}</Text>
              <If when={!!actionError()?.code}>
                <Badge variant="secondary" label={`code: ${actionError()?.code ?? ""}`} />
              </If>
            </View>
          </If>
          <Separator />
          <View style={{ gap: space.md }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
              新建技能
            </Text>
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
              与 Agent 调用 skills.manage create 是同一条写入路径；保存后立即可见。
            </Text>
            <FieldGroup>
              <Field label="名字" hint="小写字母、数字与连字符，例如 deploy-flow。">
                <Input
                  testID="skills-new-name"
                  accessibilityLabel="技能名字"
                  value={newSkillName()}
                  onChange={(value) => newSkillName.set(value)}
                  placeholder="deploy-flow"
                  maxLength={64}
                  disabled={store.skillsBusy()}
                />
              </Field>
              <Field label="描述" hint="一行说明什么时候用这个技能。">
                <Input
                  testID="skills-new-description"
                  accessibilityLabel="技能描述"
                  value={newSkillDescription()}
                  onChange={(value) => newSkillDescription.set(value)}
                  placeholder="发布前检查与回滚步骤"
                  maxLength={1024}
                  disabled={store.skillsBusy()}
                />
              </Field>
              <Field label="正文" hint="SKILL.md 的 Markdown 正文（可稍后补全）。">
                <Textarea
                  testID="skills-new-body"
                  accessibilityLabel="技能正文"
                  value={newSkillBody()}
                  onChange={(value) => newSkillBody.set(value)}
                  placeholder={"1. 运行检查\n2. 记录结果"}
                  rows={4}
                  disabled={store.skillsBusy()}
                />
              </Field>
            </FieldGroup>
            <View style={{ flexDirection: "row", gap: space.sm }}>
              <Button
                testID="skills-new-save"
                icon="check"
                label="保存技能"
                loading={store.skillsBusy()}
                disabled={store.skillsBusy() || !newSkillName().trim() || !newSkillDescription().trim()}
                onPress={saveSkill}
              />
            </View>
          </View>
        </CardContent>
      </Card>

      <Card testID="settings-skill-sources">
        <CardHeader
          title="订阅源"
          description="订阅源决定更新从哪里来，并带一个信任等级：未信任源每次安装/更新都需要显式确认，安装后也不会被升级为受信任。当前构建只同步本地目录。"
        />
        <CardContent>
          <If when={sources().length === 0}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              还没有订阅源。添加一个本地目录后，可以从它安装技能并检查更新。
            </Text>
          </If>
          <View>
            {sources().map((source) => (
              <SourceRow source={source} />
            ))}
          </View>
          <Separator />
          <View style={{ gap: space.md }}>
            <FieldGroup>
              <Field label="名字" hint="订阅源的稳定 id，例如 team。">
                <Input
                  testID="skill-source-id"
                  accessibilityLabel="订阅源名字"
                  value={newSourceId()}
                  onChange={(value) => newSourceId.set(value)}
                  placeholder="team"
                  maxLength={64}
                  disabled={store.skillsBusy()}
                />
              </Field>
              <Field label="目录" hint="服务端可读的本地目录；目录本身是技能或包含 <name>/SKILL.md。">
                <Input
                  testID="skill-source-url"
                  accessibilityLabel="订阅源目录"
                  value={newSourceURL()}
                  onChange={(value) => newSourceURL.set(value)}
                  placeholder="/srv/keel-skills"
                  maxLength={512}
                  disabled={store.skillsBusy()}
                />
              </Field>
              <Field label="信任">
                <Select
                  testID="skill-source-trust"
                  accessibilityLabel="订阅源信任等级"
                  items={trustItems()}
                  value={newSourceTrust()}
                  disabled={store.skillsBusy()}
                  onChange={(value) => newSourceTrust.set(value ?? "trusted")}
                />
              </Field>
            </FieldGroup>
            <View style={{ flexDirection: "row", gap: space.sm }}>
              <Button
                testID="skill-source-subscribe"
                icon="plus"
                label="订阅本地目录"
                loading={store.skillsBusy()}
                disabled={store.skillsBusy() || !newSourceId().trim() || !newSourceURL().trim()}
                onPress={subscribe}
              />
            </View>
          </View>
        </CardContent>
      </Card>

      <DetailDialog />

      <AlertDialog
        testID="skill-remove-confirm"
        open={!!confirmRemoveSkill()}
        title={`移除技能 ${confirmRemoveSkill() ?? ""}？`}
        description="移除后该技能不再出现在 skills ls 与下一次运行的提示词里；不可变版本会在无引用后回收。"
        confirmLabel={`确认移除 ${confirmRemoveSkill() ?? ""}`}
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          const name = confirmRemoveSkill();
          confirmRemoveSkill.set(null);
          if (name) void store.removeSkill(name);
        }}
        onCancel={() => confirmRemoveSkill.set(null)}
      />
      <AlertDialog
        testID="skill-source-remove-confirm"
        open={!!confirmRemoveSource()}
        title={`取消订阅 ${confirmRemoveSource() ?? ""}？`}
        description="仍有技能以它为来源时会被拒绝：先移除那些技能，否则更新来源会消失。"
        confirmLabel={`确认取消订阅 ${confirmRemoveSource() ?? ""}`}
        cancelLabel="取消"
        onConfirm={() => {
          const id = confirmRemoveSource();
          confirmRemoveSource.set(null);
          if (id) void store.unsubscribeSkillSource(id);
        }}
        onCancel={() => confirmRemoveSource.set(null)}
      />
    </View>
  );
}
