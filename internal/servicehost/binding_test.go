package servicehost

// The execution-binding tests (change keel-agent-team-v1, RUNTIME-PLAN §b/c):
// the submit-time gate and the per-attempt resolution of an input's
// agentId/teamId, over the real domain validation (internal/agentteam) and a
// map-backed document store.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/store"
)

// fakeBindingStore is the BindingReader over in-memory documents.
type fakeBindingStore struct {
	agents map[string]store.AgentRecord
	teams  map[string]store.TeamRecord
	fail   error
}

func (f *fakeBindingStore) GetAgent(_ context.Context, _ uuid.UUID, agentID string) (store.AgentRecord, bool, error) {
	if f.fail != nil {
		return store.AgentRecord{}, false, f.fail
	}
	record, ok := f.agents[agentID]
	return record, ok, nil
}

func (f *fakeBindingStore) GetTeam(_ context.Context, _ uuid.UUID, teamID string) (store.TeamRecord, bool, error) {
	if f.fail != nil {
		return store.TeamRecord{}, false, f.fail
	}
	record, ok := f.teams[teamID]
	return record, ok, nil
}

func agentRecord(t *testing.T, agent agentteam.Agent, archived bool) store.AgentRecord {
	t.Helper()
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	return store.AgentRecord{TenantID: uuid.Nil, ID: agent.ID, Document: raw, Archived: archived}
}

func teamRecord(t *testing.T, team *agentteam.AgentTeam, archived bool) store.TeamRecord {
	t.Helper()
	raw, err := json.Marshal(team)
	if err != nil {
		t.Fatal(err)
	}
	return store.TeamRecord{
		TenantID:    uuid.Nil,
		ID:          team.ID,
		Revision:    int64(team.Revision()),
		ContentHash: team.Hash(),
		Document:    raw,
		Archived:    archived,
	}
}

// bindingFixture builds a store with one plain agent, one captain and one sealed
// team led by it.
type bindingFixture struct {
	store   *fakeBindingStore
	plain   agentteam.Agent
	captain agentteam.Agent
	team    *agentteam.AgentTeam
}

func newBindingFixture(t *testing.T) bindingFixture {
	t.Helper()
	plain := agentteam.Agent{
		ID:           "agent_plain",
		Name:         "Plain",
		DefaultModel: agentteam.ModelBinding{ProviderID: "tenant-gw", ModelID: "deepseek-v4.1-flash"},
		SystemPrompt: agentteam.SystemPrompt{Kind: agentteam.SystemPromptInline, Text: "KITE-RUNTIME-MARK-7 plain agent instructions"},
		Tools:        []agentteam.ToolGrant{{ToolID: "bash", Enabled: true}},
	}.Normalized()
	captain := agentteam.Agent{
		ID:   "agent_captain",
		Name: "Captain",
		SystemPrompt: agentteam.SystemPrompt{
			Kind: agentteam.SystemPromptFile, Ref: "AGENTS.md",
		},
		Tools:   []agentteam.ToolGrant{{ToolID: "bash", Enabled: true}, {ToolID: "read", Enabled: false}},
		TeamCLI: agentteam.TeamCLI{Enabled: true, Commands: []string{agentteam.TeamCLIMemberAdd}},
	}.Normalized()
	team := agentteam.NewTeam("team_kite", "Kite", agentteam.MemberFromAgent(captain))
	team.Members = []agentteam.TeamMember{agentteam.MemberFromAgent(plain)}
	team.Defaults = agentteam.TeamDefaults{Tools: []string{"read", "agent"}}
	team.Seal()

	store := &fakeBindingStore{
		agents: map[string]store.AgentRecord{
			plain.ID:   agentRecord(t, plain, false),
			captain.ID: agentRecord(t, captain, false),
		},
		teams: map[string]store.TeamRecord{team.ID: teamRecord(t, team, false)},
	}
	return bindingFixture{store: store, plain: plain, captain: captain, team: team}
}

func TestLoadBindingStandardMode(t *testing.T) {
	fixture := newBindingFixture(t)
	binding, err := LoadBinding(context.Background(), fixture.store, uuid.Nil,
		json.RawMessage(`{"providerId":"p","modelId":"m","agentId":"agent_plain"}`))
	if err != nil {
		t.Fatalf("LoadBinding: %v", err)
	}
	if !binding.Bound() || binding.Agent.ID != "agent_plain" || binding.Team != nil {
		t.Fatalf("binding = %+v", binding)
	}
	if got := binding.PromptText(); !strings.Contains(got, "KITE-RUNTIME-MARK-7") {
		t.Fatalf("prompt text = %q", got)
	}
	if got := binding.ToolIDs(); len(got) != 1 || !got["bash"] {
		t.Fatalf("tool ids = %v, want only bash", got)
	}
}

func TestLoadBindingTeamModeUsesCaptain(t *testing.T) {
	fixture := newBindingFixture(t)
	binding, err := LoadBinding(context.Background(), fixture.store, uuid.Nil,
		json.RawMessage(`{"modelId":"m","teamId":"team_kite"}`))
	if err != nil {
		t.Fatalf("LoadBinding: %v", err)
	}
	if binding.Team == nil || binding.Team.ID != "team_kite" {
		t.Fatalf("team binding = %+v", binding)
	}
	if binding.Agent.ID != "agent_captain" {
		t.Fatalf("effective agent = %q, want the captain", binding.Agent.ID)
	}
	if binding.TeamRevision != fixture.team.Revision() || binding.TeamHash != fixture.team.Hash() {
		t.Fatalf("frozen team identity = %d/%q, want %d/%q",
			binding.TeamRevision, binding.TeamHash, fixture.team.Revision(), fixture.team.Hash())
	}
	if len(binding.Members) != 1 || binding.Members[0].AgentID != "agent_plain" {
		t.Fatalf("members = %+v", binding.Members)
	}
	// The team defaults are unioned with the captain's grants; the captain's
	// explicit read=false removes the team default.
	grants := binding.ToolIDs()
	if !grants["bash"] || !grants["agent"] {
		t.Fatalf("grants = %v, want bash and agent", grants)
	}
	if grants["read"] {
		t.Fatalf("grants = %v, want read removed by the agent's explicit false", grants)
	}
}

func TestLoadBindingTeamWinsOverLooseAgent(t *testing.T) {
	fixture := newBindingFixture(t)
	// RUNTIME-PLAN ruling: a fact naming both is a team fact.
	binding, err := LoadBinding(context.Background(), fixture.store, uuid.Nil,
		json.RawMessage(`{"modelId":"m","agentId":"agent_plain","teamId":"team_kite"}`))
	if err != nil {
		t.Fatalf("LoadBinding: %v", err)
	}
	if binding.Agent.ID != "agent_captain" {
		t.Fatalf("effective agent = %q, want the captain (team wins)", binding.Agent.ID)
	}
}

func TestLoadBindingRefusals(t *testing.T) {
	fixture := newBindingFixture(t)
	// An archived agent is not startable.
	archived := agentRecord(t, fixture.plain, true)
	fixture.store.agents[archived.ID] = archived
	// An unsealed team is a draft, not a startable configuration.
	unsealed := agentteam.NewTeam("team_draft", "Draft", agentteam.MemberFromAgent(fixture.captain))
	fixture.store.teams[unsealed.ID] = teamRecord(t, unsealed, false)
	// A team whose captain cannot be found.
	orphan := agentteam.NewTeam("team_orphan", "Orphan", agentteam.MemberFromAgent(agentteam.Agent{ID: "agent_missing", Name: "Gone"}))
	orphan.Seal()
	fixture.store.teams[orphan.ID] = teamRecord(t, orphan, false)
	// An archived team.
	archivedTeam := agentteam.NewTeam("team_archived", "Archived", agentteam.MemberFromAgent(fixture.captain))
	archivedTeam.Seal()
	fixture.store.teams[archivedTeam.ID] = teamRecord(t, archivedTeam, true)

	cases := []struct {
		name      string
		selection string
		want      string
	}{
		{name: "unknown agent", selection: `{"agentId":"agent_nope"}`, want: "does not exist"},
		{name: "archived agent", selection: `{"agentId":"agent_plain"}`, want: "archived"},
		{name: "unknown team", selection: `{"teamId":"team_nope"}`, want: "does not exist"},
		{name: "archived team", selection: `{"teamId":"team_archived"}`, want: "archived"},
		{name: "unsealed team", selection: `{"teamId":"team_draft"}`, want: "not sealed"},
		{name: "team with a missing captain", selection: `{"teamId":"team_orphan"}`, want: "agent_missing"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadBinding(context.Background(), fixture.store, uuid.Nil, json.RawMessage(test.selection))
			if err == nil {
				t.Fatalf("LoadBinding(%s) = nil, want a refusal", test.selection)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadBinding(%s) = %v, want it to name %q", test.selection, err, test.want)
			}
		})
	}
}

func TestLoadBindingWithoutSelectionIsZero(t *testing.T) {
	fixture := newBindingFixture(t)
	for _, raw := range []string{"", "null", `{"modelId":"m"}`, `{"modelId":"m","providerId":"p"}`} {
		binding, err := LoadBinding(context.Background(), fixture.store, uuid.Nil, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("LoadBinding(%q): %v", raw, err)
		}
		if binding.Bound() {
			t.Fatalf("LoadBinding(%q) bounds an agent", raw)
		}
		if binding.ToolIDs() != nil {
			t.Fatalf("zero binding declares grants: %v", binding.ToolIDs())
		}
	}
}

func TestBindingResolverPromptAndHash(t *testing.T) {
	fixture := newBindingFixture(t)
	root := t.TempDir()
	const fileBody = "KITE-RUNTIME-MARK-8 file prompt body"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(fileBody+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := &bindingResolver{reader: fixture.store, root: root, log: discardLogger()}

	surface, err := resolver.ResolveBinding(context.Background(), kernel.Scope{}, json.RawMessage(`{"modelId":"m","agentId":"agent_plain"}`))
	if err != nil {
		t.Fatalf("ResolveBinding: %v", err)
	}
	if !strings.Contains(surface.Prompt, "KITE-RUNTIME-MARK-7") {
		t.Fatalf("inline prompt = %q", surface.Prompt)
	}
	sum := sha256.Sum256([]byte(surface.Prompt))
	if surface.PromptHash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("prompt hash = %q", surface.PromptHash)
	}
	if surface.TeamMode || surface.TeamID != "" {
		t.Fatalf("standard mode surface = %+v", surface)
	}
	if surface.DefaultModel == nil || surface.DefaultModel.ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("default model = %+v", surface.DefaultModel)
	}

	teamSurface, err := resolver.ResolveBinding(context.Background(), kernel.Scope{}, json.RawMessage(`{"modelId":"m","teamId":"team_kite"}`))
	if err != nil {
		t.Fatalf("ResolveBinding(team): %v", err)
	}
	if !teamSurface.TeamMode || teamSurface.TeamID != "team_kite" {
		t.Fatalf("team surface = %+v", teamSurface)
	}
	if strings.TrimSpace(teamSurface.Prompt) != fileBody {
		t.Fatalf("file prompt = %q, want %q", teamSurface.Prompt, fileBody)
	}
	if teamSurface.PromptHash == "" {
		t.Fatal("the file prompt must carry a hash")
	}
}

func TestBindingResolverRefusesEscapingPromptRef(t *testing.T) {
	fixture := newBindingFixture(t)
	root := t.TempDir()
	escape := agentteam.Agent{
		ID:           "agent_escape",
		Name:         "Escape",
		SystemPrompt: agentteam.SystemPrompt{Kind: agentteam.SystemPromptFile, Ref: "AGENTS.md"},
	}.Normalized()
	// The stored document carries a ref the save path would refuse; the resolver
	// re-checks because the root is a runtime value.
	raw, err := json.Marshal(map[string]any{
		"id":           escape.ID,
		"name":         escape.Name,
		"avatar":       escape.Avatar,
		"systemPrompt": map[string]any{"kind": "file", "ref": "../../etc/hosts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.agents[escape.ID] = store.AgentRecord{ID: escape.ID, Document: raw}
	resolver := &bindingResolver{reader: fixture.store, root: root, log: discardLogger()}
	if _, err := resolver.ResolveBinding(context.Background(), kernel.Scope{}, json.RawMessage(`{"agentId":"agent_escape"}`)); err == nil {
		t.Fatal("an escaping prompt ref must be refused")
	}
}

func TestBindingResolverFilePromptWithoutRootIsDiagnosticNonFatal(t *testing.T) {
	fixture := newBindingFixture(t)
	resolver := &bindingResolver{reader: fixture.store, root: "", log: discardLogger()}
	surface, err := resolver.ResolveBinding(context.Background(), kernel.Scope{}, json.RawMessage(`{"teamId":"team_kite"}`))
	if err != nil {
		t.Fatalf("ResolveBinding: %v", err)
	}
	if surface.Prompt != "" || surface.PromptHash != "" {
		t.Fatalf("surface = %+v, want no prompt", surface)
	}
	if !surface.TeamMode {
		t.Fatalf("surface = %+v, want the team binding to survive", surface)
	}
}

// TestRuntimeValidateSelectionBinding is the submit-time gate end to end: the
// binding resolves against the real domain rules (existence, archive, seal), an
// unknown agent is a refusal, and a fact that named no model is merged with the
// agent's default model before the model gate runs.
func TestRuntimeValidateSelectionBinding(t *testing.T) {
	fixture := newBindingFixture(t)
	mem := kernel.NewMemStore()
	kern := kernel.New(mem, kernel.Options{})
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	llmOptions := testLLM(map[string]string{"TENANT_GW_KEY": "env-key"}, "flat-key")
	cfg := Config{
		Seams:     Seams{Committer: kern, Reader: kern, Leases: kern, External: kern, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    tenant,
		Options:   loadout.Options{},
		RuntimeID: "binding-test-zzz",
		LLM:       llmOptions,
		Bindings:  BindingsOptions{Reader: fixture.store, TenantID: uuid.New()},
	}
	rt, err := Boot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	ctx := context.Background()
	// A binding with no model of its own resolves through the agent's default.
	if err := rt.ValidateSelection(ctx, json.RawMessage(`{"agentId":"agent_plain"}`)); err != nil {
		t.Fatalf("ValidateSelection(agent default): %v", err)
	}
	// An explicit model still wins (nothing to validate differently here, but the
	// fact must keep resolving).
	if err := rt.ValidateSelection(ctx, json.RawMessage(`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash","agentId":"agent_plain"}`)); err != nil {
		t.Fatalf("ValidateSelection(explicit model): %v", err)
	}
	// Unknown / archived bindings are refused before anything is stored.
	if err := rt.ValidateSelection(ctx, json.RawMessage(`{"agentId":"agent_nope"}`)); err == nil {
		t.Fatal("an unknown agentId must be refused")
	}
	archived := agentRecord(t, fixture.plain, true)
	fixture.store.agents[archived.ID] = archived
	if err := rt.ValidateSelection(ctx, json.RawMessage(`{"agentId":"agent_plain"}`)); err == nil {
		t.Fatal("an archived agentId must be refused")
	}
	// A fact with no binding at all keeps the pre-binding behaviour.
	if err := rt.ValidateSelection(ctx, json.RawMessage(`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash"}`)); err != nil {
		t.Fatalf("ValidateSelection(model only): %v", err)
	}
	// A runtime without the binding seam refuses a fact that names one: the fact
	// cannot be honoured here, and accepting it would silently run the deployment
	// prompt with the full tool surface (the degradation spec agent-team forbids).
	noBindings, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: kern, Reader: kern, Leases: kern, External: kern, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    tenant,
		Options:   loadout.Options{},
		RuntimeID: "binding-test-none",
		LLM:       llmOptions,
	})
	if err != nil {
		t.Fatalf("Boot (no bindings): %v", err)
	}
	t.Cleanup(func() { _ = noBindings.Close(context.Background()) })
	if err := noBindings.ValidateSelection(ctx, json.RawMessage(`{"agentId":"agent_nope"}`)); !errors.Is(err, ErrBindingUnavailable) {
		t.Fatalf("a runtime without the binding seam must refuse a binding fact, got %v", err)
	}
	// A model-only fact still passes: the model gate alone decides, exactly as
	// before the binding existed.
	if err := noBindings.ValidateSelection(ctx, json.RawMessage(`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash"}`)); err != nil {
		t.Fatalf("ValidateSelection(model only, no bindings): %v", err)
	}
	_ = agent.SurfaceModelBinding{}
}

// TestBindingResolverRefusesCredentialPromptFiles pins the credential boundary
// on file-kind system prompts. The deployment root is the process working
// directory — where .env.local / .env.personal / .admin-key-note live — so a
// prompt ref that resolves to one of them would ship the whole file to the
// provider as a system prompt (a credential leak that leaves the process, not
// just the machine).
func TestBindingResolverRefusesCredentialPromptFiles(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"dotenv local", ".env.local"},
		{"dotenv personal", ".env.personal"},
		{"dotenv team", ".env.team"},
		{"admin key note", ".admin-key-note"},
		{"private key", "deploy/server.pem"},
		{"key extension", "certs/service.key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newBindingFixture(t)
			root := t.TempDir()
			// Every candidate file really exists: the refusal must come from the
			// credential rule, not from a read failure.
			path := filepath.Join(root, filepath.FromSlash(tc.ref))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("KEEL_DSN=postgres://secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			agentDoc := agentteam.Agent{
				ID:   "agent_creds",
				Name: "Creds",
			}.Normalized()
			raw, err := json.Marshal(map[string]any{
				"id":           agentDoc.ID,
				"name":         agentDoc.Name,
				"avatar":       agentDoc.Avatar,
				"systemPrompt": map[string]any{"kind": "file", "ref": tc.ref},
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.store.agents[agentDoc.ID] = store.AgentRecord{ID: agentDoc.ID, Document: raw}
			resolver := &bindingResolver{reader: fixture.store, root: root, log: discardLogger()}
			_, err = resolver.ResolveBinding(context.Background(), kernel.Scope{},
				json.RawMessage(`{"agentId":"agent_creds"}`))
			if !errors.Is(err, ErrPromptIsCredential) {
				t.Fatalf("prompt ref %q resolved without the credential refusal: %v", tc.ref, err)
			}
		})
	}
}

// TestBindingResolverRefusesSymlinkEscapingRoot pins the resolved-path half of
// the containment check: a symlink inside the root that points outside it passes
// a purely lexical Rel, and only the EvalSymlinks comparison catches it.
func TestBindingResolverRefusesSymlinkEscapingRoot(t *testing.T) {
	fixture := newBindingFixture(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside the deployment root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "prompt.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	agentDoc := agentteam.Agent{ID: "agent_link", Name: "Link"}.Normalized()
	raw, err := json.Marshal(map[string]any{
		"id":           agentDoc.ID,
		"name":         agentDoc.Name,
		"avatar":       agentDoc.Avatar,
		"systemPrompt": map[string]any{"kind": "file", "ref": "prompt.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.agents[agentDoc.ID] = store.AgentRecord{ID: agentDoc.ID, Document: raw}
	resolver := &bindingResolver{reader: fixture.store, root: root, log: discardLogger()}
	_, err = resolver.ResolveBinding(context.Background(), kernel.Scope{},
		json.RawMessage(`{"agentId":"agent_link"}`))
	if !errors.Is(err, ErrPromptEscapesRoot) {
		t.Fatalf("a symlink leaving the root must be refused, got %v", err)
	}
}

// TestTeamModelOverrideIsTheSingleArbiter pins the team-level default model. The
// field exists so a team can pin one model without editing every Agent document,
// and it must actually take effect: a stored-but-unread override is a settings
// control that lies. Precedence stays 输入 selection > 团队 override > Agent 默认.
func TestTeamModelOverrideIsTheSingleArbiter(t *testing.T) {
	cases := []struct {
		name     string
		override *agentteam.ModelBinding
		agent    agentteam.ModelBinding
		want     agentteam.ModelBinding
	}{
		{
			name:     "override wins over the agent default",
			override: &agentteam.ModelBinding{ProviderID: "team-gw", ModelID: "team-model"},
			agent:    agentteam.ModelBinding{ProviderID: "agent-gw", ModelID: "agent-model"},
			want:     agentteam.ModelBinding{ProviderID: "team-gw", ModelID: "team-model"},
		},
		{
			name:     "incomplete override falls back to the agent default",
			override: &agentteam.ModelBinding{ProviderID: "team-gw"},
			agent:    agentteam.ModelBinding{ProviderID: "agent-gw", ModelID: "agent-model"},
			want:     agentteam.ModelBinding{ProviderID: "agent-gw", ModelID: "agent-model"},
		},
		{
			name:  "no override keeps the agent default",
			agent: agentteam.ModelBinding{ProviderID: "agent-gw", ModelID: "agent-model"},
			want:  agentteam.ModelBinding{ProviderID: "agent-gw", ModelID: "agent-model"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team := &agentteam.AgentTeam{Defaults: agentteam.TeamDefaults{ModelOverride: tc.override}}
			binding := RunBinding{
				Agent: agentteam.Agent{ID: "agent_captain", DefaultModel: tc.agent},
				Team:  team,
			}
			if got := binding.effectiveDefaultModel(); got.ProviderID != tc.want.ProviderID || got.ModelID != tc.want.ModelID {
				t.Fatalf("effective default = %+v, want %+v", got, tc.want)
			}
		})
	}
}
