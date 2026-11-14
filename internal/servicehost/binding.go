// Package servicehost: the selection execution binding.
//
// A run's selection fact may name an Agent (standard mode) or an AgentTeam
// (team mode) besides the model identity (change keel-agent-team-v1, RUNTIME-PLAN
// §b/c). This file owns the two things that binding enables and nothing else:
//
//   - the submit-time gate: an input that names an unknown, archived or
//     unsealed binding is refused before it is committed (the same place an
//     unexecutable model is refused), so a bad fact never enters a session's
//     history;
//   - the per-attempt resolution: which agent document answers this attempt,
//     which system prompt it contributes and which tool ids it grants.
//
// The documents stay store rows; this package is the only one that maps them
// onto the run path (internal/agent's resolver types are plain data, and
// internal/store must not import the domain package).
package servicehost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/store"
)

// BindingReader is the read seam of the tenant's Agent and AgentTeam documents.
// *store.Store satisfies it; it is an interface so the resolution is testable
// without PostgreSQL and so the run path never holds a table handle.
type BindingReader interface {
	GetAgent(ctx context.Context, tenantID uuid.UUID, agentID string) (store.AgentRecord, bool, error)
	GetTeam(ctx context.Context, tenantID uuid.UUID, teamID string) (store.TeamRecord, bool, error)
}

// ErrBindingMissing marks a selection that names an agent/team this tenant does
// not have (or has archived). It is a validation failure, not a runtime one.
var ErrBindingMissing = errors.New("selection binding is not available")

// ErrBindingUnavailable marks a selection that names an execution binding while
// this process has no binding seam wired at all. Refusing is the only honest
// answer: accepting it would store a fact the attempt cannot honour, and the run
// would degrade to the deployment prompt plus the full tool surface.
var ErrBindingUnavailable = errors.New("selection binding is not available: this server cannot resolve an agent/team binding")

// BindingsOptions carries the execution-binding resolver inputs: the read seam
// of the tenant's Agent/AgentTeam documents and the root a file-kind system
// prompt resolves against. A nil Reader disables the binding surface entirely
// (the pre-binding behaviour: deployment prompt, whole tool registry).
type BindingsOptions struct {
	Reader BindingReader
	// TenantID is the tenant the documents belong to. The serve process acts as
	// one tenant, so the resolution does not need the request scope; a zero id
	// keeps the binding disabled (nothing can be looked up).
	TenantID uuid.UUID
	// Root is the directory a prompt-file reference (AGENTS.md) resolves
	// against. Empty refuses file prompts with a diagnostic.
	Root string
}

// RunBinding is one attempt's resolved execution binding: the agent answering
// the input, the team it belongs to (team mode), and the derived surface the
// runner consumes. Zero value means "no binding": the run keeps the deployment
// prompt and the whole tool registry.
type RunBinding struct {
	// Agent is the effective agent document (the captain in team mode).
	Agent agentteam.Agent
	// Team is the team document the input selected (nil in standard mode).
	Team *agentteam.AgentTeam
	// TeamRevision and TeamHash pin the stored team version the run froze.
	TeamRevision uint64
	TeamHash     string
	// Members are the team's member seats (empty in standard mode).
	Members []agentteam.TeamMember
}

// Bound reports whether the selection named an execution identity at all.
func (b RunBinding) Bound() bool { return strings.TrimSpace(b.Agent.ID) != "" }

// PromptText returns the system prompt the binding contributes: the inline text,
// or "" when the agent declares no inline prompt (the caller renders the
// deployment prompt then). A file reference is resolved by the caller, which
// owns the workspace root.
func (b RunBinding) PromptText() string {
	if !b.Bound() {
		return ""
	}
	prompt := b.Agent.SystemPrompt
	if strings.TrimSpace(prompt.Kind) != agentteam.SystemPromptInline {
		return ""
	}
	return strings.TrimSpace(prompt.Text)
}

// effectiveDefaultModel is the single arbiter between the team-level override and
// the agent's own default model. `team.defaults.modelOverride` exists so a team
// can pin one model without editing every Agent document; when it is complete it
// wins, otherwise the agent's default applies. A selection that names a model
// still outranks both (the resolver merges it before the model gate).
func (b RunBinding) effectiveDefaultModel() agentteam.ModelBinding {
	if b.Team != nil {
		if override := b.Team.Defaults.ModelOverride; override != nil && override.Complete() {
			return *override
		}
	}
	return b.Agent.DefaultModel
}

// ToolIDs returns the effective tool grant map of the binding: the agent's own
// grants plus the team's defaults, with an explicit agent-level disabled grant
// winning over the team default (the user's explicit decision).
//
// A nil map means "the binding declares no grants at all": the run keeps the
// whole registry (spec: 工具与系统提示词为空时使用默认值). An empty non-nil map
// means "the binding declares grants and none is enabled".
func (b RunBinding) ToolIDs() map[string]bool {
	if !b.Bound() {
		return nil
	}
	teamTools := 0
	if b.Team != nil {
		teamTools = len(b.Team.Defaults.Tools)
	}
	declared := len(b.Agent.Tools) > 0 || teamTools > 0
	if !declared {
		return nil
	}
	grants := make(map[string]bool, len(b.Agent.Tools)+teamTools)
	if b.Team != nil {
		for _, id := range b.Team.Defaults.Tools {
			id = strings.TrimSpace(id)
			if id != "" {
				grants[id] = true
			}
		}
	}
	// The agent's own grants are applied second: an enabled grant re-affirms the
	// team default, a disabled one removes it (explicit decision wins).
	for _, grant := range b.Agent.Tools {
		id := strings.TrimSpace(grant.ToolID)
		if id == "" {
			continue
		}
		grants[id] = grant.Enabled
	}
	return grants
}

// SpawnToolName is the delegation primitive the team mode always keeps
// available (RUNTIME-PLAN R3: a captain that can never delegate is a silently
// broken product).
const SpawnToolName = "spawn_subagent"

// bindingFromSelection decodes the execution binding of a selection fact. An
// empty fact, or one that names neither id, yields the zero binding.
func bindingFromSelection(raw json.RawMessage) (agentID, teamID string, err error) {
	selection, err := modelcfg.ParseSelection(raw)
	if err != nil {
		return "", "", err
	}
	agentID = strings.TrimSpace(selection.AgentID)
	teamID = strings.TrimSpace(selection.TeamID)
	// The plan's ruling: a fact naming both is a team fact — the team decides
	// the captain, so the loose agentId cannot override it.
	if teamID != "" {
		agentID = ""
	}
	return agentID, teamID, nil
}

// bindingNamingSelection reports whether the fact names an execution binding at
// all (a teamId, or an agentId with no teamId winning). It is the fail-closed
// test used when this process has no binding seam: such a fact cannot be
// honoured here, so it must be refused rather than stored.
func bindingNamingSelection(raw json.RawMessage) (bool, error) {
	agentID, teamID, err := bindingFromSelection(raw)
	if err != nil {
		return false, err
	}
	return agentID != "" || teamID != "", nil
}

// LoadBinding resolves a selection fact into the run binding. A nil reader, or a
// fact naming no binding, yields the zero binding (the pre-existing behaviour).
//
// Errors are the submit-time refusals: a missing/archived agent, a
// missing/archived/unsealed team, or a team whose captain is missing. They are
// reported with the field path the settings view and the CLI render, so the
// caller can answer 400 with the same vocabulary an agent save uses.
func LoadBinding(ctx context.Context, reader BindingReader, tenantID uuid.UUID, selection json.RawMessage) (RunBinding, error) {
	if reader == nil {
		return RunBinding{}, nil
	}
	agentID, teamID, err := bindingFromSelection(selection)
	if err != nil {
		return RunBinding{}, err
	}
	if agentID == "" && teamID == "" {
		return RunBinding{}, nil
	}
	if teamID != "" {
		return loadTeamBinding(ctx, reader, tenantID, teamID)
	}
	agent, err := loadAgent(ctx, reader, tenantID, agentID, "agentId")
	if err != nil {
		return RunBinding{}, err
	}
	return RunBinding{Agent: agent}, nil
}

// loadTeamBinding resolves a team selection: the stored team, its seal, and its
// captain agent (the effective agent of the attempt).
func loadTeamBinding(ctx context.Context, reader BindingReader, tenantID uuid.UUID, teamID string) (RunBinding, error) {
	record, found, err := reader.GetTeam(ctx, tenantID, teamID)
	if err != nil {
		return RunBinding{}, err
	}
	if !found {
		return RunBinding{}, fmt.Errorf("%w: teamId %q does not exist", ErrBindingMissing, teamID)
	}
	var team agentteam.AgentTeam
	if err := json.Unmarshal(record.Document, &team); err != nil {
		return RunBinding{}, fmt.Errorf("teamId %q: stored document is not a team: %w", teamID, err)
	}
	if team.ID == "" {
		team.ID = record.ID
	}
	if record.Archived {
		team.Archived = true
	}
	if team.Archived {
		return RunBinding{}, fmt.Errorf("%w: teamId %q is archived", ErrBindingMissing, teamID)
	}
	// The start gate: a team must be sealed so the manifest can freeze its
	// revision and hash, and every referenced agent must exist. The existence
	// lookup reuses this reader, which is the tenant boundary.
	if err := team.ValidateForStart(func(id string) bool {
		_, found, err := reader.GetAgent(ctx, tenantID, id)
		return err == nil && found
	}); err != nil {
		return RunBinding{}, err
	}
	captain, err := loadAgentFromTeam(ctx, reader, tenantID, team.Captain)
	if err != nil {
		return RunBinding{}, err
	}
	return RunBinding{
		Agent:        captain,
		Team:         &team,
		TeamRevision: team.Revision(),
		TeamHash:     team.Hash(),
		Members:      append([]agentteam.TeamMember(nil), team.Members...),
	}, nil
}

// loadAgentFromTeam resolves one team seat: an inline definition wins (it is the
// team's own copy), otherwise the referenced library agent is read.
func loadAgentFromTeam(ctx context.Context, reader BindingReader, tenantID uuid.UUID, seat agentteam.TeamMember) (agentteam.Agent, error) {
	if seat.Inline != nil {
		inline := seat.Inline.Normalized()
		if inline.ID == "" {
			inline.ID = strings.TrimSpace(seat.AgentID)
		}
		return inline, nil
	}
	return loadAgent(ctx, reader, tenantID, seat.AgentID, "captain.agentId")
}

// loadAgent reads one agent document and refuses a missing or archived one.
func loadAgent(ctx context.Context, reader BindingReader, tenantID uuid.UUID, agentID, field string) (agentteam.Agent, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return agentteam.Agent{}, fmt.Errorf("%w: %s is empty", ErrBindingMissing, field)
	}
	record, found, err := reader.GetAgent(ctx, tenantID, agentID)
	if err != nil {
		return agentteam.Agent{}, err
	}
	if !found {
		return agentteam.Agent{}, fmt.Errorf("%w: %s %q does not exist", ErrBindingMissing, field, agentID)
	}
	var agent agentteam.Agent
	if err := json.Unmarshal(record.Document, &agent); err != nil {
		return agentteam.Agent{}, fmt.Errorf("%s %q: stored document is not an agent: %w", field, agentID, err)
	}
	if agent.ID == "" {
		agent.ID = record.ID
	}
	if record.Archived {
		agent.Archived = true
	}
	if agent.Archived {
		return agentteam.Agent{}, fmt.Errorf("%w: %s %q is archived", ErrBindingMissing, field, agentID)
	}
	return agent, nil
}

// ValidateSelectionBinding is the submit-time gate of the execution binding: it
// runs the same resolution an attempt runs, so the edge can never accept a
// binding the next attempt would refuse (the rule ValidateSelection follows for
// model identities).
func ValidateSelectionBinding(ctx context.Context, reader BindingReader, tenantID uuid.UUID, selection json.RawMessage) error {
	if reader == nil {
		return nil
	}
	if len(strings.TrimSpace(string(selection))) == 0 {
		return nil
	}
	_, err := LoadBinding(ctx, reader, tenantID, selection)
	return err
}

// ValidateSelection is the submit-time gate of an input selection: the execution
// binding and the model identity, in the one composition the next attempt runs.
//
//   - the binding resolves exactly as an attempt resolves it (existence,
//     archive, team seal) — an unknown agentId is a refusal, not a stored fact
//     that silently does nothing;
//   - a fact that named no model is merged with the bound agent's default model
//     before the model gate, so a selection the edge accepts is a selection the
//     attempt can execute.
func (r *Runtime) ValidateSelection(ctx context.Context, selection json.RawMessage) error {
	if r == nil {
		return errors.New("servicehost: runtime closed")
	}
	service := r.Models()
	if service == nil {
		return errors.New("selection is not supported by this server")
	}
	if r.cfg.Bindings.Reader == nil || r.cfg.Bindings.TenantID == uuid.Nil {
		// No binding seam: a selection naming no agent/team is fine (the model
		// gate alone applies), but one that names a binding must be refused.
		// Accepting it would persist a fact this process can never honour, and the
		// attempt would fall back to the deployment prompt and the full tool
		// surface — the silent degradation spec agent-team forbids.
		named, err := bindingNamingSelection(selection)
		if err != nil {
			return err
		}
		if named {
			return ErrBindingUnavailable
		}
		return service.ValidateSelection(selection)
	}
	binding, err := LoadBinding(ctx, r.cfg.Bindings.Reader, r.cfg.Bindings.TenantID, selection)
	if err != nil {
		return err
	}
	if !binding.Bound() {
		return service.ValidateSelection(selection)
	}
	merged := agent.MergeSelectionDefaultModel(selection, commandModelBinding(binding.effectiveDefaultModel()))
	return service.ValidateSelection(merged)
}

// --- the run-path resolver ------------------------------------------------

// bindingResolver is the agent.BindingResolver over the tenant's documents. It
// is frozen per loadout generation (the tenant boundary travels with it), and it
// reads the documents per attempt, so an edit applies to the next attempt while
// an attempt in flight keeps what it resolved.
type bindingResolver struct {
	reader   BindingReader
	tenantID uuid.UUID
	// root is the directory a file-kind system prompt reference resolves
	// against (the deployment's working root). Empty refuses file prompts with a
	// diagnostic instead of guessing a path.
	root string
	log  *slog.Logger
}

var _ agent.BindingResolver = (*bindingResolver)(nil)

// ResolveBinding implements agent.BindingResolver.
func (r *bindingResolver) ResolveBinding(ctx context.Context, scope kernel.Scope, selection json.RawMessage) (agent.RunSurface, error) {
	if r == nil || r.reader == nil {
		return agent.RunSurface{}, nil
	}
	tenantID := scope.TenantID
	if tenantID == uuid.Nil {
		tenantID = r.tenantID
	}
	binding, err := LoadBinding(ctx, r.reader, tenantID, selection)
	if err != nil {
		return agent.RunSurface{}, err
	}
	if !binding.Bound() {
		return agent.RunSurface{}, nil
	}
	prompt, err := r.promptText(binding)
	if err != nil {
		return agent.RunSurface{}, err
	}
	surface := agent.RunSurface{
		Prompt:       prompt,
		PromptHash:   promptDigest(prompt),
		ToolIDs:      binding.ToolIDs(),
		TeamMode:     binding.Team != nil,
		AgentID:      binding.Agent.ID,
		DefaultModel: commandModelBinding(binding.effectiveDefaultModel()),
	}
	if binding.Team != nil {
		surface.TeamID = binding.Team.ID
		surface.TeamRevision = binding.TeamRevision
		surface.TeamHash = binding.TeamHash
		surface.Members = memberRoster(binding.Members)
	}
	return surface, nil
}

// memberRoster projects the team's member seats onto the runtime's plain-data
// form. The captain needs it to delegate: spawn_subagent takes an agentId, and
// without the roster the only way to learn one is to guess. The label and role
// text travel with the id because they are what makes a choice informed.
func memberRoster(members []agentteam.TeamMember) []agent.RosterMember {
	if len(members) == 0 {
		return nil
	}
	out := make([]agent.RosterMember, 0, len(members))
	for _, m := range members {
		id := strings.TrimSpace(m.AgentID)
		if id == "" {
			continue
		}
		label := strings.TrimSpace(m.Label)
		if label == "" && m.Inline != nil {
			label = strings.TrimSpace(m.Inline.Name)
		}
		out = append(out, agent.RosterMember{
			AgentID:     id,
			Label:       label,
			Description: strings.TrimSpace(m.Description),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// promptText resolves the binding's system prompt: inline text as-is, a file
// reference read from the deployment root. A file that cannot be read is a
// diagnostic and no prompt (the same posture skills discovery takes: a broken
// prompt file must not stop the run).
func (r *bindingResolver) promptText(binding RunBinding) (string, error) {
	prompt := binding.Agent.SystemPrompt
	kind := strings.TrimSpace(prompt.Kind)
	if kind != agentteam.SystemPromptFile {
		return binding.PromptText(), nil
	}
	ref := strings.TrimSpace(prompt.Ref)
	if ref == "" {
		return "", nil
	}
	if strings.TrimSpace(r.root) == "" {
		r.log.Warn("agent system prompt file ignored: no deployment root", "agent", binding.Agent.ID, "ref", ref)
		return "", nil
	}
	path := filepath.Join(r.root, filepath.FromSlash(ref))
	// The ref grammar is validated at save time (agentteam.safePromptRef); the
	// containment check is repeated here because the root is a runtime value.
	// It runs on the RESOLVED path: a lexical check alone lets a symlink inside
	// the root read anything on the host, and the deployment root is the process
	// working directory — where credential files (.env.local, .env.personal,
	// .admin-key-note) actually live.
	resolved, err := containedRealPath(r.root, path)
	if err != nil {
		return "", fmt.Errorf("agent %q: system prompt ref %q %w", binding.Agent.ID, ref, err)
	}
	if err := refuseCredentialPath(resolved); err != nil {
		// A prompt that resolves to a credential file is an operator error at
		// best and an exfiltration attempt at worst; it must fail, not degrade:
		// the alternative sends the file to the provider as a system prompt.
		return "", fmt.Errorf("agent %q: system prompt ref %q %w", binding.Agent.ID, ref, err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		r.log.Warn("agent system prompt file unreadable", "agent", binding.Agent.ID, "ref", ref, "err", err)
		return "", nil
	}
	return strings.TrimSpace(string(raw)), nil
}

// ErrPromptEscapesRoot reports a prompt ref that resolves outside the
// deployment root (lexically or through a symlink).
var ErrPromptEscapesRoot = errors.New("escapes the deployment root")

// ErrPromptIsCredential reports a prompt ref that resolves to a credential-shaped
// file. Credentials never belong in a system prompt: the prompt leaves the
// process as a provider request.
var ErrPromptIsCredential = errors.New("resolves to a credential file and is refused")

// containedRealPath resolves path (following symlinks) and requires the result
// to stay under root (itself resolved the same way). Comparing resolved paths is
// what makes the check meaningful: a lexical Rel passes for a symlink that
// points anywhere.
//
// The lexical check runs first and unconditionally: a ref like "../../etc/hosts"
// must be refused by grammar alone, even when the target does not exist (a
// missing file is the caller's diagnostic, never a containment pass).
func containedRealPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrPromptEscapesRoot
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("deployment root %q: %w", root, err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		// A missing file is the caller's diagnostic (it warns and uses no
		// prompt); an unreadable *resolved* path is not a containment failure.
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	realRel, err := filepath.Rel(realRoot, realPath)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", ErrPromptEscapesRoot
	}
	return realPath, nil
}

// credentialFileNames are the exact basenames that carry deployment credentials
// in this project. They are matched case-insensitively; dotenv siblings with a
// different suffix (.env.local, .env.personal, .env.team) match the prefix rule
// below.
var credentialFileNames = []string{".admin-key-note", ".npmrc", ".netrc", "id_rsa", "id_ed25519"}

// credentialFilePrefixes are basename prefixes whose matches are all treated as
// credential material.
var credentialFilePrefixes = []string{".env"}

// credentialFileExts are extensions that conventionally carry private key
// material.
var credentialFileExts = []string{".pem", ".key", ".p12", ".pfx"}

// refuseCredentialPath rejects a prompt file that is credential-shaped. The
// check is on the resolved basename so a symlink or a nested path cannot launder
// the name.
func refuseCredentialPath(path string) error {
	base := strings.ToLower(filepath.Base(path))
	for _, prefix := range credentialFilePrefixes {
		if strings.HasPrefix(base, prefix) {
			return ErrPromptIsCredential
		}
	}
	for _, name := range credentialFileNames {
		if base == name {
			return ErrPromptIsCredential
		}
	}
	ext := strings.ToLower(filepath.Ext(base))
	for _, want := range credentialFileExts {
		if ext == want {
			return ErrPromptIsCredential
		}
	}
	return nil
}

// commandModelBinding projects the domain binding onto the runtime's plain-data
// form. An inheriting (absent) binding yields nil: the input or the tenant
// default then decides the model.
func commandModelBinding(binding agentteam.ModelBinding) *agent.SurfaceModelBinding {
	if !binding.Complete() {
		return nil
	}
	out := &agent.SurfaceModelBinding{
		ProviderID: strings.TrimSpace(binding.ProviderID),
		ModelID:    strings.TrimSpace(binding.ModelID),
	}
	if len(binding.Tuning) > 0 {
		out.Tuning = binding.Tuning
	}
	return out
}

// promptDigest is the stable identity of a system prompt in the run summary. It
// is a plain SHA-256 of the bytes (no domain separation needed: the field is
// named promptHash and the summary is the only carrier).
func promptDigest(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return "sha256:" + hex.EncodeToString(sum[:])
}
