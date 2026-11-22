// The display catalogue of permission keys — change keel-bot-identity-access,
// A4.4a.
//
// The server's registry (`internal/access/keys.go`) is the authority: it decides
// which keys exist, which scopes a grant may use and how risky a key is. This
// module is the *display* vocabulary the identity pages need before a
// `/v1/permissions` read exists: a Chinese name, a domain, the risk class and
// the closed scope set, copied from that registry. It grants nothing and
// validates nothing — a write is refused by the server, and a grant whose key is
// not listed here still renders (verbatim key, "常规" risk) instead of being
// dropped from the ledger.
//
// Keep the entries in the registry's own order (domain, then key) so a diff
// against `keys.go` is readable.
import type { PermissionScope } from "./identity";

export type CapabilityRisk = "normal" | "high" | "critical";

export interface CapabilitySpec {
  key: string;
  /** The first key segment (`session`, `bot`, …). */
  domain: string;
  /** The Chinese row label. */
  displayName: string;
  risk: CapabilityRisk;
  /** The scopes a grant for this key may use (the registry's `AllowedScopes`). */
  scopes: PermissionScope[];
}

const ALL_SCOPES: PermissionScope[] = ["own", "team", "tenant", "all"];
const TENANT_SCOPES: PermissionScope[] = ["tenant", "all"];

const spec = (
  key: string,
  displayName: string,
  risk: CapabilityRisk,
  scopes: PermissionScope[] = ALL_SCOPES,
): CapabilitySpec => ({ key, domain: key.slice(0, key.indexOf(".")), displayName, risk, scopes });

/** The registry's catalogue, in the registry's order. */
export const CAPABILITIES: CapabilitySpec[] = [
  // Sessions.
  spec("session.read", "查看会话", "normal"),
  spec("session.write", "继续会话", "normal"),
  spec("session.delete", "删除会话", "high"),
  spec("session.share", "共享会话", "high"),
  // Bots.
  spec("bot.read", "查看 Bot", "normal"),
  spec("bot.create", "创建 Bot", "normal", TENANT_SCOPES),
  spec("bot.manage", "管理 Bot", "high"),
  spec("bot.bind", "绑定 Bot 密钥", "high"),
  spec("bot.invoke", "调用 Bot", "critical"),
  // Team bots (a TeamBot is a resource, never a permission subject).
  spec("team_bot.read", "查看团队", "normal"),
  spec("team_bot.manage", "管理团队", "high"),
  // Accounts.
  spec("account.read", "查看账号", "normal", TENANT_SCOPES),
  spec("account.manage", "管理账号", "high", TENANT_SCOPES),
  spec("account.credential.reset", "重置账号密码", "critical", TENANT_SCOPES),
  spec("account.permission.grant", "授予账号权限", "critical", TENANT_SCOPES),
  // Permission groups.
  spec("permission.read", "查看权限组", "normal", TENANT_SCOPES),
  spec("permission.manage", "管理权限组", "high", TENANT_SCOPES),
  // Audit.
  spec("audit.read", "查看记录", "high", TENANT_SCOPES),
  // Secrets.
  spec("secret.read.metadata", "读取密钥列表", "normal", TENANT_SCOPES),
  spec("secret.read.value", "读取密钥值", "critical", TENANT_SCOPES),
  spec("secret.write", "写入密钥", "high", TENANT_SCOPES),
  // Plugins.
  spec("plugin.read", "查看插件", "normal", TENANT_SCOPES),
  spec("plugin.invoke", "调用插件", "high", TENANT_SCOPES),
  spec("plugin.lifecycle.install", "安装插件", "critical", TENANT_SCOPES),
  spec("plugin.lifecycle.enable", "启用插件", "critical", TENANT_SCOPES),
  spec("plugin.lifecycle.disable", "停用插件", "critical", TENANT_SCOPES),
  spec("plugin.lifecycle.uninstall", "卸载插件", "critical", TENANT_SCOPES),
  // Models.
  spec("model.read", "查看模型配置", "normal", TENANT_SCOPES),
  spec("model.configure", "配置模型", "high", TENANT_SCOPES),
  spec("model.invoke", "调用模型", "high", TENANT_SCOPES),
  // Triggers.
  spec("trigger.read", "查看触发器", "normal"),
  spec("trigger.manage", "管理触发器", "high"),
  // Roles and profiles (documents, never authority).
  spec("role.read", "查看人设", "normal", TENANT_SCOPES),
  spec("role.manage", "管理人设", "high", TENANT_SCOPES),
  spec("profile.read", "查看身份档案", "normal", TENANT_SCOPES),
  spec("profile.manage", "管理身份档案", "high", TENANT_SCOPES),
  // Application discovery.
  spec("app.read", "查看应用能力", "normal", TENANT_SCOPES),
  // Files.
  spec("file.read", "读取文件", "normal", TENANT_SCOPES),
  spec("file.write", "写入文件", "high", TENANT_SCOPES),
  // Media gateway.
  spec("media.read", "查看媒体能力", "normal", TENANT_SCOPES),
  spec("media.invoke", "调用媒体服务", "high", TENANT_SCOPES),
  // MCP servers and apps.
  spec("mcp.read", "查看集成", "normal", TENANT_SCOPES),
  spec("mcp.use", "使用集成", "high", TENANT_SCOPES),
  // Skills.
  spec("skill.read", "查看技能", "normal", TENANT_SCOPES),
  spec("skill.manage", "管理技能", "high", TENANT_SCOPES),
  // Notifications.
  spec("notification.read", "查看通知", "normal", TENANT_SCOPES),
  spec("notification.write", "确认通知", "high", TENANT_SCOPES),
  // Model tool actions (the session is the resource).
  spec("shell.exec", "执行命令", "high"),
  spec("task.read", "查看任务", "normal"),
  spec("task.write", "写任务", "normal"),
  spec("approval.read", "查看审批", "normal"),
  spec("approval.write", "发起审批", "high"),
  spec("decision.ask", "请求决策", "high"),
];

const byKey = new Map(CAPABILITIES.map((entry) => [entry.key, entry]));

/** One key's display facts; an unknown key keeps its own token and is shown as
 * 常规 rather than invented into something safer-looking. */
export function capabilityOf(key: string): CapabilitySpec {
  const known = byKey.get(key);
  if (known) return known;
  const dot = key.indexOf(".");
  return {
    key,
    domain: dot > 0 ? key.slice(0, dot) : key,
    displayName: key,
    risk: "normal",
    scopes: ALL_SCOPES,
  };
}

export const RISK_LABEL: Record<CapabilityRisk, string> = {
  normal: "常规",
  high: "敏感",
  critical: "高危",
};

/** The five scope words the UI prints; `undefined` is 未授予. */
export const SCOPE_LABEL: Record<PermissionScope, string> = {
  own: "仅自己",
  team: "本团队",
  tenant: "整个租户",
  all: "全部",
};

/** A scope word, or 未授予. */
export const scopeLabel = (scope: PermissionScope | undefined): string =>
  scope === undefined ? "未授予" : SCOPE_LABEL[scope];

/** The domains the matrix groups by, in display order. A domain that is not
 * listed still renders (its own token as the label). */
const DOMAIN_ORDER: Array<{ id: string; label: string; icon: string }> = [
  { id: "session", label: "会话", icon: "message" },
  { id: "bot", label: "Bot", icon: "bot" },
  { id: "team_bot", label: "团队", icon: "users" },
  { id: "account", label: "账号", icon: "users" },
  { id: "permission", label: "权限组", icon: "shield" },
  { id: "audit", label: "记录", icon: "activity" },
  { id: "secret", label: "密钥", icon: "key" },
  { id: "plugin", label: "插件", icon: "layers" },
  { id: "model", label: "模型", icon: "brain" },
  { id: "trigger", label: "触发器", icon: "clock" },
  { id: "role", label: "人设", icon: "spark" },
  { id: "profile", label: "身份档案", icon: "file" },
  { id: "app", label: "应用", icon: "spark" },
  { id: "file", label: "文件", icon: "file" },
  { id: "media", label: "媒体", icon: "image" },
  { id: "mcp", label: "集成", icon: "layers" },
  { id: "skill", label: "技能", icon: "spark" },
  { id: "notification", label: "通知", icon: "message" },
  { id: "shell", label: "终端", icon: "terminal" },
  { id: "task", label: "任务", icon: "check" },
  { id: "approval", label: "审批", icon: "check" },
  { id: "decision", label: "决策", icon: "brain" },
];

const domainIndex = new Map(DOMAIN_ORDER.map((domain, index) => [domain.id, index]));

export interface CapabilityDomain {
  id: string;
  label: string;
  icon: string;
  items: CapabilitySpec[];
}

/** Group one key list by domain, in display order. A key list that names a key
 * the catalogue does not know still renders under its own domain. */
export function groupCapabilities(keys: string[]): CapabilityDomain[] {
  const domains = new Map<string, CapabilitySpec[]>();
  for (const key of keys) {
    const entry = capabilityOf(key);
    const bucket = domains.get(entry.domain) ?? [];
    bucket.push(entry);
    domains.set(entry.domain, bucket);
  }
  return [...domains.entries()]
    .sort(([left], [right]) => (domainIndex.get(left) ?? 99) - (domainIndex.get(right) ?? 99))
    .map(([id, items]) => {
      const known = DOMAIN_ORDER.find((domain) => domain.id === id);
      return {
        id,
        label: known?.label ?? id,
        icon: known?.icon ?? "layers",
        items: items.sort((left, right) => CAPABILITIES.indexOf(left) - CAPABILITIES.indexOf(right)),
      };
    });
}

/** Every domain in display order, with the catalogue's own items (the matrix's
 *  full list). */
export function allCapabilityDomains(): CapabilityDomain[] {
  return groupCapabilities(CAPABILITIES.map((entry) => entry.key));
}

/** Does the catalogue know this key? (An unknown grant must not be edited into
 *  one of the catalogue's rows.) */
export const isKnownKey = (key: string): boolean => byKey.has(key);

/** The scopes a key's stamp may offer (the registry's closed set). */
export const scopesOf = (key: string): PermissionScope[] => capabilityOf(key).scopes;

/** The risk label; an unknown key is 常规, its token still visible. */
export const riskLabelOf = (key: string): string => RISK_LABEL[capabilityOf(key).risk];

export const isHighRisk = (key: string): boolean => capabilityOf(key).risk === "critical";

/** The highest scope the caller may grant for a key: their own held scope,
 * capped by the key's allowed set. `undefined` when they hold no grant. */
export function scopeCeilingOf(key: string, held: PermissionScope | undefined): PermissionScope | undefined {
  if (held === undefined) return undefined;
  const allowed = scopesOf(key);
  const heldIndex = ALL_SCOPES.indexOf(held);
  return allowed.filter((scope) => ALL_SCOPES.indexOf(scope) <= heldIndex).pop();
}

/** The sentence that explains why a scope cannot be chosen. `held` is the
 * caller's own scope for the key (`undefined`: no grant). */
export function scopeDisabledReason(
  key: string,
  scope: PermissionScope,
  held: PermissionScope | undefined,
): string | undefined {
  const allowed = scopesOf(key);
  if (!allowed.includes(scope)) {
    return scope === "own" || scope === "team"
      ? "这项能力仅支持租户级范围。"
      : "这项能力没有这个范围。";
  }
  if (held === undefined) {
    return "你没有这项能力，不能授予。";
  }
  const heldIndex = ALL_SCOPES.indexOf(held);
  if (ALL_SCOPES.indexOf(scope) > heldIndex) {
    return `最高 ${scopeLabel(held)}，超出你的权限。`;
  }
  return undefined;
}
