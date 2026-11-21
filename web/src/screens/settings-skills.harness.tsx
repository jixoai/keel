// TUI harness · settings「技能」— the section alone, against an in-memory
// transport.
//
// The product's transport is same-origin `globalThis.fetch`; an in-memory
// terminal has no origin for a relative URL to resolve against, so the harness
// installs a seam on the store (`setSkillsTransport`) holding a real
// `KeelHttp` whose fetch is the stub below. What is under test is still the
// real transport parsing and the section's rendering, not a second fake
// client. The scenario comes from the launch parameters:
// `list`, `empty`, `loading` (the read never settles), `error` (500),
// `permission` (403), `refuse` (an update is refused as pinned),
// `source-in-use` (unsubscribe is refused) and `slow` (a write is observable).
import { cleanup, effect, host, mount, Scroll, View } from "nsh";
import type { SkillRecordView, SkillSourceView } from "../data/contract";
import { KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import { SkillsSection } from "./settings-skills";

const scenario = (): string => String(host.params.scenario ?? "list");

const respond = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const pending = (): Promise<Response> => new Promise<Response>(() => {});

const settleSlowly = (response: Response): Promise<Response> =>
  new Promise((resolve) => setTimeout(() => resolve(response), 300));

const hash = (seed: string): string => (seed.repeat(64)).slice(0, 64);

const seedSkills = (): SkillRecordView[] => [
  {
    name: "deploy-flow",
    description: "发布检查清单与回滚步骤",
    trust: "local",
    currentHash: hash("a"),
    installedAt: "2026-11-20T10:00:00Z",
    updatedAt: "2026-11-20T10:00:00Z",
    versions: [{ hash: hash("a"), createdAt: "2026-11-20T10:00:00Z", files: 2, bytes: 512 }],
    fileCount: 2,
    bytes: 512,
    path: "active/deploy-flow",
    updateAvailable: false,
  },
  {
    name: "team-review",
    description: "团队评审清单",
    trust: "trusted",
    sourceId: "team",
    sourceKind: "local",
    sourceUrl: "team",
    currentHash: hash("b"),
    pinnedHash: hash("b"),
    installedAt: "2026-11-19T10:00:00Z",
    updatedAt: "2026-11-19T10:00:00Z",
    versions: [
      { hash: hash("b"), createdAt: "2026-11-19T10:00:00Z", files: 1, bytes: 256 },
      { hash: hash("c"), createdAt: "2026-11-18T10:00:00Z", files: 1, bytes: 256 },
    ],
    fileCount: 1,
    bytes: 256,
    path: "active/team-review",
    updateAvailable: true,
    availableHash: hash("c"),
    conflict: "pinned",
    diagnostics: ["a pinned version holds back the update; unpin before updating"],
  },
];

const seedSources = (): SkillSourceView[] => [
  { id: "team", kind: "local", url: "team", trust: "trusted", createdAt: "2026-11-18T10:00:00Z" },
];

let skills: SkillRecordView[] = scenario() === "empty" ? [] : seedSkills();
let sources: SkillSourceView[] = scenario() === "empty" ? [] : seedSources();
let revision = 3;
let created = 0;

const stub: FetchLike = async (input, init) => {
  const method = (init?.method ?? "GET").toUpperCase();
  const path = input.split("?")[0] ?? input;
  const bodyText = typeof init?.body === "string" ? init.body : "";
  const body = bodyText ? (JSON.parse(bodyText) as Record<string, unknown>) : {};

  if (method === "GET" && path === "/v1/settings/skills") {
    if (scenario() === "loading") return pending();
    if (scenario() === "error") return respond(500, { error: "技能库暂时不可用" });
    if (scenario() === "permission") return respond(403, { error: "当前账号没有 skills.read 权限" });
    return respond(200, {
      revision,
      root: "/skills",
      checked: true,
      skills,
      sources,
      limits: { maxFiles: 256, maxBytes: 8 * 1024 * 1024, maxFileBytes: 2 * 1024 * 1024 },
      policy: {
        trustValues: ["trusted", "untrusted", "local"],
        sourceKinds: ["local"],
        scriptExecution: "bash",
      },
    });
  }

  if (method === "GET" && path === "/v1/settings/skills/sources") {
    return respond(200, { sources });
  }

  if (method === "GET" && path.startsWith("/v1/settings/skills/")) {
    const name = decodeURIComponent(path.slice("/v1/settings/skills/".length));
    const record = skills.find((item) => item.name === name);
    if (!record) return respond(404, { error: `skill ${name} was not found`, code: "not_found" });
    return respond(200, {
      ...record,
      body: `# ${name}\n\n1. 运行检查\n2. 记录结果\n`,
      files: [{ path: "SKILL.md", size: 256, hash: hash("d"), mode: 420 }],
    });
  }

  if (method === "POST" && path === "/v1/settings/skills") {
    const name = String(body.name ?? "").trim();
    if (scenario() === "refuse") {
      return respond(409, { error: `skill ${name} is pinned; unpin before installing`, code: "pinned" });
    }
    const record: SkillRecordView = {
      name,
      description: String(body.description ?? ""),
      trust: "local",
      currentHash: hash(`new-${++created}`),
      versions: [{ hash: hash(`new-${created}`), files: 1, bytes: 128 }],
      fileCount: 1,
      bytes: 128,
      path: `active/${name}`,
    };
    skills = [...skills, record];
    revision += 1;
    return scenario() === "slow" ? settleSlowly(respond(201, record)) : respond(201, record);
  }

  if (method === "POST" && path.endsWith("/update")) {
    if (scenario() === "refuse") {
      return respond(409, { error: "skill team-review is pinned at b; unpin before updating to c", code: "pinned" });
    }
    const name = decodeURIComponent(path.slice("/v1/settings/skills/".length, -"/update".length));
    const record = skills.find((item) => item.name === name);
    if (!record) return respond(404, { error: `skill ${name} was not found`, code: "not_found" });
    record.currentHash = hash(`${name}-next`);
    record.updateAvailable = false;
    record.availableHash = undefined;
    revision += 1;
    return respond(200, { status: "updated", record, previousHash: hash("b") });
  }

  if (method === "POST" && path.endsWith("/pin")) {
    const name = decodeURIComponent(path.slice("/v1/settings/skills/".length, -"/pin".length));
    const record = skills.find((item) => item.name === name);
    if (!record) return respond(404, { error: `skill ${name} was not found`, code: "not_found" });
    record.pinnedHash = String(body.hash || record.currentHash);
    revision += 1;
    return respond(200, record);
  }

  if (method === "POST" && path.endsWith("/unpin")) {
    const name = decodeURIComponent(path.slice("/v1/settings/skills/".length, -"/unpin".length));
    const record = skills.find((item) => item.name === name);
    if (!record) return respond(404, { error: `skill ${name} was not found`, code: "not_found" });
    record.pinnedHash = undefined;
    if (record.conflict === "pinned") record.conflict = undefined;
    revision += 1;
    return respond(200, record);
  }

  if (method === "DELETE" && path.startsWith("/v1/settings/skills/sources/")) {
    const id = decodeURIComponent(path.slice("/v1/settings/skills/sources/".length));
    if (scenario() === "source-in-use") {
      return respond(409, { error: `source ${id} is still the origin of skill team-review`, code: "source_in_use" });
    }
    sources = sources.filter((item) => item.id !== id);
    revision += 1;
    return respond(200, { removed: true });
  }

  if (method === "POST" && path === "/v1/settings/skills/sources") {
    const source: SkillSourceView = {
      id: String(body.id ?? ""),
      kind: String(body.kind ?? "local"),
      url: String(body.url ?? ""),
      trust: String(body.trust ?? "untrusted"),
      createdAt: "2026-11-21T10:00:00Z",
    };
    sources = [...sources, source];
    revision += 1;
    return respond(201, source);
  }

  if (method === "DELETE" && path.startsWith("/v1/settings/skills/")) {
    const name = decodeURIComponent(path.slice("/v1/settings/skills/".length));
    skills = skills.filter((item) => item.name !== name);
    revision += 1;
    return respond(200, { removed: true });
  }

  return respond(404, { error: `no route for ${method} ${input}` });
};

store.setSkillsTransport(new KeelHttp("", stub));
store.skills.set(null);

function Harness() {
  effect(() => {
    // The store is one module-level singleton across a test file's opens: clear
    // the flags a previous scenario may have left, then load this one.
    store.skillsLoading.set(false);
    store.skillsBusy.set(false);
    store.skillsActionError.set(null);
    const timer = setTimeout(() => void store.loadSkillsPanel(true), 0);
    cleanup(() => clearTimeout(timer));
  });
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <Scroll testID="skills-scroll" style={{ flex: 1, minHeight: 0 }}>
        <SkillsSection />
      </Scroll>
    </View>
  );
}

mount(Harness);
