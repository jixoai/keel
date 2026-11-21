// Package servicehost: the selection execution binding.
//
// A run's selection fact may name a Bot (standard mode) or a TeamBot (team
// mode) besides the model identity (change keel-bot-identity-access, A4.2b3a).
// This file owns the two things that binding enables and nothing else:
//
//   - the submit-time gate: an input that names an unknown/archived Bot or
//     TeamBot is refused before it is committed (the same place an
//     unexecutable model is refused), so a bad fact never enters a session's
//     history;
//   - the per-attempt resolution: which Bot answers this attempt, which system
//     prompt it contributes, which tool ids its Harness grants and which
//     default model applies.
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
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/store"
)

// BindingReader is the read seam of the tenant's identity graph: Bot, TeamBot,
// Profile, Role and the session's ownership. *store.Store satisfies it; it is
// an interface so the resolution is testable without PostgreSQL and so the run
// path never holds a table handle.
type BindingReader interface {
	GetBot(ctx context.Context, tenantID uuid.UUID, botID string) (store.BotRecord, error)
	GetTeamBot(ctx context.Context, tenantID uuid.UUID, teamBotID string) (store.TeamBotRecord, error)
	GetProfile(ctx context.Context, tenantID, profileID uuid.UUID) (store.ProfileRecord, error)
	GetRole(ctx context.Context, tenantID, roleID uuid.UUID) (store.RoleRecord, error)
	GetSessionOwnership(ctx context.Context, tenantID, sessionID uuid.UUID) (store.SessionOwnership, error)
}

// ErrBindingMissing marks a selection that names a Bot/TeamBot this tenant does
// not have (or has archived), or a session that has no Bot owner. It is a
// validation failure, not a runtime one.
var ErrBindingMissing = errors.New("selection binding is not available")

// ErrBindingUnavailable marks a selection that names an execution binding while
// this process has no binding seam wired at all. Refusing is the only honest
// answer: accepting it would store a fact the attempt cannot honour, and the run
// would degrade to the deployment prompt plus the full tool surface.
var ErrBindingUnavailable = errors.New("selection binding is not available: this server cannot resolve a bot/team bot binding")

// BindingsOptions carries the execution-binding resolver inputs: the read seam
// of the tenant's identity graph. A nil Reader disables the binding surface
// entirely (no Bot/TeamBot selection is accepted).
type BindingsOptions struct {
	Reader BindingReader
	// TenantID is the tenant the documents belong to. The serve process acts as
	// one tenant, so the resolution does not need the request scope; a zero id
	// keeps the binding disabled (nothing can be looked up).
	TenantID uuid.UUID
}

// RunBinding is one attempt's resolved execution binding: the Bot answering the
// input, the team it belongs to (team mode) and the derived surface the runner
// consumes. Zero value means "no binding".
type RunBinding struct {
	// Bot is the effective Bot document (the captain in team mode).
	Bot botteam.Bot
	// Profile and Role are the Bot's identity and soul. Role grants nothing.
	Profile botteam.Profile
	Role    botteam.Role
	// TeamBot is the team document the input selected (nil in standard mode).
	TeamBot *botteam.TeamBot
	// TeamBotRevision and TeamBotHash pin the stored team version the run froze.
	TeamBotRevision uint64
	TeamBotHash     string
	// Members are the team's non-captain seats (empty in standard mode).
	Members []botteam.RosterEntry
}

// Bound reports whether the selection named an execution identity at all.
func (b RunBinding) Bound() bool { return strings.TrimSpace(b.Bot.ID) != "" }

// PromptText returns the system prompt the binding contributes: the Role's
// instruction (identity/soul) followed by the Harness context instruction
// (operating prompt). Each part is trimmed; an exact duplicate is emitted once.
// The order is fixed so a prompt hash is stable: Role first, Harness second.
func (b RunBinding) PromptText() string {
	if !b.Bound() {
		return ""
	}
	role := strings.TrimSpace(b.Role.Instruction)
	harness := strings.TrimSpace(b.Bot.Harness.Context.Instruction)
	switch {
	case role == "":
		return harness
	case harness == "" || harness == role:
		return role
	default:
		return role + "\n\n" + harness
	}
}

// Mission returns the team mission in team mode ("" otherwise).
func (b RunBinding) Mission() string {
	if b.TeamBot == nil {
		return ""
	}
	return strings.TrimSpace(b.TeamBot.Mission)
}

// effectiveDefaultModel is the Bot's own default model. A team has no model
// override: a TeamBot is Mission + Roster and cannot silently repin a member's
// model (design §4.2). A selection that names a model outranks it (the resolver
// merges it before the model gate).
func (b RunBinding) effectiveDefaultModel() botteam.ModelRef {
	return b.Bot.DefaultModel
}

// ToolIDs returns the effective tool grant map of the binding: the Harness
// whitelist. An empty non-nil map means "the Harness grants no tool" — the
// product rule is a whitelist, not "empty means everything". The runner
// intersects it with the deployment registry (a harness entry the registry does
// not carry is unavailable and logged there).
func (b RunBinding) ToolIDs() map[string]bool {
	if !b.Bound() {
		return nil
	}
	grants := make(map[string]bool, len(b.Bot.Harness.Tools))
	for _, raw := range b.Bot.Harness.Tools {
		id := strings.TrimSpace(raw)
		if id != "" {
			grants[id] = true
		}
	}
	return grants
}

// SpawnToolName is the delegation primitive the team mode always keeps
// available (RUNTIME-PLAN R3: a captain that can never delegate is a silently
// broken product).
const SpawnToolName = "agent"

// bindingFromSelection decodes the execution binding of a selection fact. An
// empty fact, or one that names neither id, yields the zero binding. The
// retired agentId/teamId spellings are not read: the strict edge already
// refuses them as unknown keys, and this decoder must not resurrect them.
func bindingFromSelection(raw json.RawMessage) (botID, teamBotID string, err error) {
	selection, err := modelcfg.ParseSelection(raw)
	if err != nil {
		return "", "", err
	}
	botID = strings.TrimSpace(selection.BotID)
	teamBotID = strings.TrimSpace(selection.TeamBotID)
	// The plan's ruling: a fact naming both is a team fact — the team decides
	// the captain, so the loose botId cannot override it.
	if teamBotID != "" {
		botID = ""
	}
	return botID, teamBotID, nil
}

// bindingNamingSelection reports whether the fact names an execution binding at
// all (a teamBotId, or a botId with no teamBotId winning). It is the fail-closed
// test used when this process has no binding seam: such a fact cannot be
// honoured here, so it must be refused rather than stored.
func bindingNamingSelection(raw json.RawMessage) (bool, error) {
	botID, teamBotID, err := bindingFromSelection(raw)
	if err != nil {
		return false, err
	}
	return botID != "" || teamBotID != "", nil
}

// LoadBinding resolves an explicit selection fact into the run binding. A nil
// reader, or a fact naming no binding, yields the zero binding.
//
// Errors are the submit-time refusals: a missing/archived Bot, a
// missing/archived/unsealed TeamBot, or a team whose captain/member is missing.
// They are reported with the field path the settings view and the CLI render.
func LoadBinding(ctx context.Context, reader BindingReader, tenantID uuid.UUID, selection json.RawMessage) (RunBinding, error) {
	if reader == nil {
		return RunBinding{}, nil
	}
	botID, teamBotID, err := bindingFromSelection(selection)
	if err != nil {
		return RunBinding{}, err
	}
	if botID == "" && teamBotID == "" {
		return RunBinding{}, nil
	}
	if teamBotID != "" {
		return loadTeamBotBinding(ctx, reader, tenantID, teamBotID, "")
	}
	return loadBotBinding(ctx, reader, tenantID, botID, "")
}

// LoadSessionBinding resolves the binding of one attempt: an explicit selection
// wins, otherwise the session's own Bot is used. The session must be owned by a
// Bot — an unowned (pre-A4) session is not runnable (fail closed), which is the
// runtime half of "every Session belongs to exactly one Bot".
//
// A selection naming a Bot other than the session's owner is allowed: a
// delegated child session keeps its parent's ownership (A4.2a) while the
// selected member Bot supplies the prompt/tools for that attempt. Ownership is
// never moved by a selection fact.
func LoadSessionBinding(ctx context.Context, reader BindingReader, tenantID, sessionID uuid.UUID, selection json.RawMessage) (RunBinding, error) {
	if reader == nil {
		return RunBinding{}, nil
	}
	if sessionID == uuid.Nil {
		return RunBinding{}, fmt.Errorf("%w: the attempt has no session", ErrBindingMissing)
	}
	ownership, err := reader.GetSessionOwnership(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RunBinding{}, fmt.Errorf("%w: session %s does not exist", ErrBindingMissing, sessionID)
		}
		return RunBinding{}, err
	}
	if strings.TrimSpace(ownership.BotID) == "" {
		return RunBinding{}, fmt.Errorf("%w: session %s is not owned by a bot and cannot run", ErrBindingMissing, sessionID)
	}
	botID, teamBotID, err := bindingFromSelection(selection)
	if err != nil {
		return RunBinding{}, err
	}
	if teamBotID != "" {
		return loadTeamBotBinding(ctx, reader, tenantID, teamBotID, ownership.BotID)
	}
	if botID != "" {
		return loadBotBinding(ctx, reader, tenantID, botID, ownership.TeamBotID)
	}
	return loadBotBinding(ctx, reader, tenantID, ownership.BotID, ownership.TeamBotID)
}

// loadBotBinding resolves one Bot plus its Profile and Role. teamBotID is the
// session's mission snapshot (optional): when present, the team is loaded as
// context but the Bot stays the effective agent (ownership is fixed).
func loadBotBinding(ctx context.Context, reader BindingReader, tenantID uuid.UUID, botID, teamBotID string) (RunBinding, error) {
	bot, err := loadBot(ctx, reader, tenantID, botID, "botId")
	if err != nil {
		return RunBinding{}, err
	}
	profile, err := reader.GetProfile(ctx, tenantID, bot.ProfileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RunBinding{}, fmt.Errorf("%w: bot %q references a missing profile", ErrBindingMissing, bot.ID)
		}
		return RunBinding{}, err
	}
	role, err := reader.GetRole(ctx, tenantID, profile.RoleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RunBinding{}, fmt.Errorf("%w: bot %q references a missing role", ErrBindingMissing, bot.ID)
		}
		return RunBinding{}, err
	}
	binding := RunBinding{
		Bot:     bot,
		Profile: botteam.Profile{ID: profile.ID, AccountID: profile.AccountID, RoleID: profile.RoleID},
		Role:    botteam.Role{ID: role.ID, Name: role.Name, Instruction: role.Instruction},
	}
	if strings.TrimSpace(teamBotID) != "" {
		team, revision, hash, err := loadTeamBot(ctx, reader, tenantID, teamBotID)
		if err != nil {
			return RunBinding{}, err
		}
		members, err := teamMembers(ctx, reader, tenantID, team, binding.Bot.ID)
		if err != nil {
			return RunBinding{}, err
		}
		binding.TeamBot = &team
		binding.TeamBotRevision = revision
		binding.TeamBotHash = hash
		binding.Members = members
	}
	return binding, nil
}

// loadTeamBotBinding resolves a team selection: the stored TeamBot, its seal,
// its roster members, and the captain unless ownerBotID pins another Bot.
func loadTeamBotBinding(ctx context.Context, reader BindingReader, tenantID uuid.UUID, teamBotID, ownerBotID string) (RunBinding, error) {
	team, revision, hash, err := loadTeamBot(ctx, reader, tenantID, teamBotID)
	if err != nil {
		return RunBinding{}, err
	}
	captain := ownerBotID
	if captain == "" {
		for _, seat := range team.Roster {
			if seat.Role == botteam.RosterRoleCaptain {
				captain = strings.TrimSpace(seat.BotID)
				break
			}
		}
	}
	if captain == "" {
		return RunBinding{}, fmt.Errorf("%w: teamBotId %q has no captain", ErrBindingMissing, teamBotID)
	}
	binding, err := loadBotBinding(ctx, reader, tenantID, captain, "")
	if err != nil {
		return RunBinding{}, fmt.Errorf("teamBotId %q: %w", teamBotID, err)
	}
	members, err := teamMembers(ctx, reader, tenantID, team, captain)
	if err != nil {
		return RunBinding{}, fmt.Errorf("teamBotId %q: %w", teamBotID, err)
	}
	binding.TeamBot = &team
	binding.TeamBotRevision = revision
	binding.TeamBotHash = hash
	binding.Members = members
	return binding, nil
}

// loadTeamBot reads one TeamBot row, decodes the domain value (the revision and
// hash are claims Validate checks against the content) and refuses a missing,
// archived or hand-edited row.
func loadTeamBot(ctx context.Context, reader BindingReader, tenantID uuid.UUID, teamBotID string) (botteam.TeamBot, uint64, string, error) {
	teamBotID = strings.TrimSpace(teamBotID)
	if teamBotID == "" {
		return botteam.TeamBot{}, 0, "", fmt.Errorf("%w: teamBotId is empty", ErrBindingMissing)
	}
	record, err := reader.GetTeamBot(ctx, tenantID, teamBotID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return botteam.TeamBot{}, 0, "", fmt.Errorf("%w: teamBotId %q does not exist", ErrBindingMissing, teamBotID)
		}
		return botteam.TeamBot{}, 0, "", err
	}
	if record.Archived {
		return botteam.TeamBot{}, 0, "", fmt.Errorf("%w: teamBotId %q is archived", ErrBindingMissing, teamBotID)
	}
	raw, err := json.Marshal(map[string]any{
		"teamBotId": record.ID, "name": record.Name, "mission": record.Mission,
		"roster": json.RawMessage(record.Roster), "archived": record.Archived,
		"revision": record.Revision, "hash": record.ContentHash,
	})
	if err != nil {
		return botteam.TeamBot{}, 0, "", err
	}
	var team botteam.TeamBot
	if err := json.Unmarshal(raw, &team); err != nil {
		return botteam.TeamBot{}, 0, "", fmt.Errorf("teamBotId %q: stored roster is not a team: %w", teamBotID, err)
	}
	if err := team.Validate(); err != nil {
		return botteam.TeamBot{}, 0, "", fmt.Errorf("teamBotId %q: %w", teamBotID, err)
	}
	return team, team.Revision(), team.Hash(), nil
}

// teamMembers loads the team's non-captain seats and refuses a missing or
// archived member: a team whose roster names a dead Bot cannot start.
func teamMembers(ctx context.Context, reader BindingReader, tenantID uuid.UUID, team botteam.TeamBot, captain string) ([]botteam.RosterEntry, error) {
	members := make([]botteam.RosterEntry, 0, len(team.Roster))
	for _, seat := range team.Roster {
		botID := strings.TrimSpace(seat.BotID)
		if botID == "" {
			continue
		}
		if _, err := loadBot(ctx, reader, tenantID, botID, "roster.botId"); err != nil {
			return nil, err
		}
		if seat.Role == botteam.RosterRoleCaptain && botID == captain {
			continue
		}
		members = append(members, botteam.RosterEntry{BotID: botID, Role: seat.Role})
	}
	return members, nil
}

// loadBot reads one Bot row and refuses a missing, archived or invalid one.
func loadBot(ctx context.Context, reader BindingReader, tenantID uuid.UUID, botID, field string) (botteam.Bot, error) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return botteam.Bot{}, fmt.Errorf("%w: %s is empty", ErrBindingMissing, field)
	}
	record, err := reader.GetBot(ctx, tenantID, botID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return botteam.Bot{}, fmt.Errorf("%w: %s %q does not exist", ErrBindingMissing, field, botID)
		}
		return botteam.Bot{}, err
	}
	if record.Archived {
		return botteam.Bot{}, fmt.Errorf("%w: %s %q is archived", ErrBindingMissing, field, botID)
	}
	bot := botteam.Bot{
		ID: record.ID, Name: record.Name, ProfileID: record.ProfileID,
		ModelKind: botteam.ModelKind(record.ModelKind), Archived: record.Archived,
	}
	if err := json.Unmarshal(record.DefaultModel, &bot.DefaultModel); err != nil {
		return botteam.Bot{}, fmt.Errorf("%s %q: stored default model is invalid: %w", field, botID, err)
	}
	if err := json.Unmarshal(record.Harness, &bot.Harness); err != nil {
		return botteam.Bot{}, fmt.Errorf("%s %q: stored harness is invalid: %w", field, botID, err)
	}
	bot = bot.Normalized()
	if err := bot.Validate(); err != nil {
		return botteam.Bot{}, fmt.Errorf("%s %q: stored bot is invalid: %w", field, botID, err)
	}
	return bot, nil
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
//     archive, team seal, roster) — an unknown botId is a refusal, not a stored
//     fact that silently does nothing;
//   - a fact that named no model is merged with the bound Bot's default model
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
		// No binding seam: a selection naming no bot/team is fine (the model
		// gate alone applies), but one that names a binding must be refused.
		// Accepting it would persist a fact this process can never honour, and the
		// attempt would fall back to the deployment prompt and the whole tool
		// surface.
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

// bindingResolver is the agent.BindingResolver over the tenant's identity
// graph. It is frozen per loadout generation (the tenant boundary travels with
// it), and it reads the documents per attempt, so an edit applies to the next
// attempt while an attempt in flight keeps what it resolved.
type bindingResolver struct {
	reader   BindingReader
	tenantID uuid.UUID
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
	binding, err := LoadSessionBinding(ctx, r.reader, tenantID, scope.SessionID, selection)
	if err != nil {
		return agent.RunSurface{}, err
	}
	if !binding.Bound() {
		return agent.RunSurface{}, fmt.Errorf("%w: the attempt resolved no bot binding", ErrBindingMissing)
	}
	prompt := binding.PromptText()
	surface := agent.RunSurface{
		Prompt:       prompt,
		PromptHash:   promptDigest(prompt),
		ToolIDs:      binding.ToolIDs(),
		TeamMode:     binding.TeamBot != nil,
		AgentID:      binding.Bot.ID,
		Mission:      binding.Mission(),
		DefaultModel: commandModelBinding(binding.effectiveDefaultModel()),
	}
	if binding.TeamBot != nil {
		surface.TeamID = binding.TeamBot.ID
		surface.TeamRevision = binding.TeamBotRevision
		surface.TeamHash = binding.TeamBotHash
		surface.Members = memberRoster(ctx, r.reader, tenantID, binding.Members)
	}
	return surface, nil
}

// memberRoster projects the team's member seats onto the runtime's plain-data
// form. The captain needs it to delegate: the agent tool's spawn action takes a
// botId, and without the roster the only way to learn one is to guess. The label
// and role text travel with the id because they are what makes a choice
// informed.
func memberRoster(ctx context.Context, reader BindingReader, tenantID uuid.UUID, members []botteam.RosterEntry) []agent.RosterMember {
	if len(members) == 0 {
		return nil
	}
	out := make([]agent.RosterMember, 0, len(members))
	for _, m := range members {
		id := strings.TrimSpace(m.BotID)
		if id == "" {
			continue
		}
		label := ""
		if bot, err := loadBot(ctx, reader, tenantID, id, "roster.botId"); err == nil {
			label = bot.Name
		}
		out = append(out, agent.RosterMember{
			BotID:       id,
			Label:       label,
			Description: string(m.Role),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// commandModelBinding projects the domain binding onto the runtime's plain-data
// form. An incomplete binding yields nil: the input or the tenant default then
// decides the model.
func commandModelBinding(binding botteam.ModelRef) *agent.SurfaceModelBinding {
	if !binding.Complete() {
		return nil
	}
	return &agent.SurfaceModelBinding{
		ProviderID: strings.TrimSpace(binding.ProviderID),
		ModelID:    strings.TrimSpace(binding.ModelID),
	}
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
