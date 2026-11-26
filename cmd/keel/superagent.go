package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/toolsurface"
)

// superagentConfig is the environment-gated assembly of the first real
// super-agent deployment. The mode stays disabled (the core change's default)
// until a Source and the two role Bots are configured.
type superagentConfig struct {
	Enabled             bool
	Source              string
	Repo                string
	Token               string
	InitialCursor       string
	OrchestratorBot     string
	WorkerBot           string
	AllowedActors       map[string]bool
	Interval            time.Duration
	Slots               int
	HumanSlots          int
	OrchestratorSession uuid.UUID
}

const superagentModeID = collab.ModeSuperagent

// superagentConfigFromEnv reads the deployment's super-agent configuration.
// KEEL_SUPERAGENT_SOURCE is the single switch: empty leaves the mode disabled.
func superagentConfigFromEnv(getenv func(string) string) (superagentConfig, error) {
	source := strings.ToLower(strings.TrimSpace(getenv("KEEL_SUPERAGENT_SOURCE")))
	cfg := superagentConfig{Source: source, Interval: 30 * time.Second, Slots: 2, HumanSlots: 1}
	if source == "" {
		return cfg, nil
	}
	switch source {
	case superagent.SourceGitHub, superagent.SourceFixture:
	default:
		return cfg, fmt.Errorf("KEEL_SUPERAGENT_SOURCE %q is not github or fixture", source)
	}
	cfg.Enabled = true
	cfg.OrchestratorBot = strings.TrimSpace(getenv("KEEL_SUPERAGENT_ORCHESTRATOR_BOT"))
	cfg.WorkerBot = strings.TrimSpace(getenv("KEEL_SUPERAGENT_WORKER_BOT"))
	if cfg.OrchestratorBot == "" || cfg.WorkerBot == "" {
		return cfg, errors.New("KEEL_SUPERAGENT_ORCHESTRATOR_BOT and KEEL_SUPERAGENT_WORKER_BOT are required when the super-agent source is enabled")
	}
	cfg.AllowedActors = splitCSV(getenv("KEEL_SUPERAGENT_ALLOWED_ACTORS"))
	if source == superagent.SourceGitHub {
		cfg.Repo = strings.TrimSpace(getenv("KEEL_SUPERAGENT_GITHUB_REPO"))
		if cfg.Repo == "" {
			return cfg, errors.New("KEEL_SUPERAGENT_GITHUB_REPO is required for the github source")
		}
		cfg.Token = strings.TrimSpace(getenv("KEEL_SUPERAGENT_GITHUB_TOKEN"))
		if cfg.Token == "" {
			cfg.Token = strings.TrimSpace(getenv("GITHUB_TOKEN"))
		}
		cfg.InitialCursor = strings.TrimSpace(getenv("KEEL_SUPERAGENT_GITHUB_SINCE"))
	}
	if raw := strings.TrimSpace(getenv("KEEL_SUPERAGENT_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Second {
			return cfg, fmt.Errorf("KEEL_SUPERAGENT_INTERVAL must be a duration >= 1s")
		}
		cfg.Interval = parsed
	}
	if raw := strings.TrimSpace(getenv("KEEL_SUPERAGENT_SLOTS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return cfg, fmt.Errorf("KEEL_SUPERAGENT_SLOTS must be a positive integer")
		}
		cfg.Slots = parsed
	}
	if raw := strings.TrimSpace(getenv("KEEL_SUPERAGENT_HUMAN_SLOTS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return cfg, fmt.Errorf("KEEL_SUPERAGENT_HUMAN_SLOTS must be a non-negative integer")
		}
		cfg.HumanSlots = parsed
	}
	if raw := strings.TrimSpace(getenv("KEEL_SUPERAGENT_ORCHESTRATOR_SESSION")); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return cfg, fmt.Errorf("KEEL_SUPERAGENT_ORCHESTRATOR_SESSION is not a UUID: %w", err)
		}
		cfg.OrchestratorSession = parsed
	}
	return cfg, nil
}

// newSuperagentRuntime assembles the orchestrator and enables the mode. It
// returns nil when no Source is configured, which is the default deployment.
func newSuperagentRuntime(ctx context.Context, tenantID uuid.UUID, st *store.Store, registry *collab.ModeRegistry, bridge *accesswire.Bridge, subagents *harnessext.SubagentRunner, starter *harnessext.TurnStarter, log *slog.Logger) (*superagent.Orchestrator, error) {
	cfg, err := superagentConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	if st == nil || registry == nil || bridge == nil {
		return nil, errors.New("super-agent needs the store, mode registry and access bridge")
	}
	if subagents == nil {
		return nil, errors.New("super-agent needs the sub-agent runner to create worker sessions")
	}
	if tenantID == uuid.Nil || registry.TenantID() != tenantID {
		return nil, fmt.Errorf("super-agent tenant mismatch: assembly tenant %s, registry tenant %s", tenantID, registry.TenantID())
	}
	orchestratorOwner, archived, err := bridge.BotOwnerAccount(ctx, tenantID, cfg.OrchestratorBot)
	if err != nil {
		return nil, fmt.Errorf("super-agent orchestrator bot: %w", err)
	}
	if archived {
		return nil, fmt.Errorf("super-agent orchestrator bot %q is archived", cfg.OrchestratorBot)
	}
	if _, archived, err := bridge.BotOwnerAccount(ctx, tenantID, cfg.WorkerBot); err != nil {
		return nil, fmt.Errorf("super-agent worker bot: %w", err)
	} else if archived {
		return nil, fmt.Errorf("super-agent worker bot %q is archived", cfg.WorkerBot)
	}
	// The worker's model tools are a Harness whitelist: report/ask are product
	// extras and task.write is the worker boundary. A Bot that does not grant
	// them can still work through the HTTP routes, so this is a loud deployment
	// warning rather than a startup refusal.
	warnSuperagentWorkerGrants(ctx, st, tenantID, cfg.WorkerBot, log)
	if _, err := bridge.BotHarnessPermissions(ctx, tenantID, cfg.OrchestratorBot, orchestratorOwner); err != nil {
		return nil, fmt.Errorf("super-agent orchestrator harness: %w", err)
	}

	sessionID := cfg.OrchestratorSession
	if sessionID == uuid.Nil {
		sessionID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.superagent.orchestrator|"+tenantID.String()+"|"+cfg.OrchestratorBot))
	}
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: tenantID, SessionID: sessionID, BotID: cfg.OrchestratorBot,
		AccountID: orchestratorOwner, CreatedByAccountID: orchestratorOwner,
	}); err != nil {
		return nil, fmt.Errorf("super-agent orchestrator session: %w", err)
	}
	orchRef := collab.SessionRef{
		TenantID: tenantID, SessionID: sessionID, BotID: cfg.OrchestratorBot,
		Depth: 0, State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}
	if _, err := st.Session(ctx, tenantID, sessionID); err != nil {
		if !errors.Is(err, collab.ErrSessionNotFound) {
			return nil, fmt.Errorf("super-agent session index: %w", err)
		}
		if err := st.Put(ctx, orchRef); err != nil {
			return nil, fmt.Errorf("super-agent session index: %w", err)
		}
	} else if err := st.SetState(ctx, tenantID, sessionID, collab.SessionActive); err != nil {
		return nil, fmt.Errorf("super-agent session state: %w", err)
	}
	// A restart kills the previous process's worker runners and watchers. Mark
	// their durable index rows interrupted before Recover so a stale "active"
	// row cannot hold a slot forever. The orchestrator root is excluded.
	interrupted, err := st.InterruptActiveCollabSessions(ctx, tenantID, superagentModeID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("super-agent interrupt stale workers: %w", err)
	}
	if interrupted > 0 {
		log.Info("super-agent marked stale worker sessions interrupted", "count", interrupted)
	}

	var source collab.Source
	var channel collab.Channel
	switch cfg.Source {
	case superagent.SourceGitHub:
		github, err := superagent.NewGitHubSource(superagent.GitHubConfig{
			TenantID: tenantID, Repo: cfg.Repo, Token: cfg.Token, InitialCursor: cfg.InitialCursor,
		})
		if err != nil {
			return nil, err
		}
		source, channel = github, github
	case superagent.SourceFixture:
		fixture := superagent.NewFixtureSource(nil)
		source, channel = fixture, fixture
	default:
		return nil, fmt.Errorf("unsupported super-agent source %q", cfg.Source)
	}

	audit := collabAuditSink{store: st, log: log}
	outbox, err := superagent.NewOutbox(superagent.OutboxOptions{
		Store: st, Channel: channel, Audit: audit,
	})
	if err != nil {
		return nil, err
	}
	principal, err := superagentPrincipal(ctx, bridge, tenantID, cfg.OrchestratorBot, orchestratorOwner)
	if err != nil {
		return nil, err
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID:    tenantID,
		Modes:       registry,
		Work:        st,
		Sessions:    st,
		Permissions: superagentPermissions{bridge: bridge, principal: principal},
		Audit:       audit,
		Launcher:    harnessext.CollabSubagentLauncher{Runner: func() *harnessext.SubagentRunner { return subagents }, ModeID: superagentModeID},
		Attention:   outbox,
	})
	if err != nil {
		return nil, err
	}
	inputs := superagent.InputSink(superagent.NoopInputSink{})
	if starter != nil {
		inputs = harnessext.SuperagentInput{Starter: starter}
	} else {
		log.Warn("super-agent assembled without a turn starter; follow-ups and answers are recorded but not delivered")
	}
	if len(cfg.AllowedActors) == 0 {
		log.Warn("super-agent has no allowed actors; every source event will be ignored")
	}
	policy := &superagent.AllowlistPolicy{
		Actors: cfg.AllowedActors, WorkerBot: cfg.WorkerBot, PolicyVersion: "env-allowlist-v1",
	}
	orchestrator, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: tenantID, ModeID: superagentModeID, Store: st, Host: host,
		Source: source, Channel: channel, Policy: policy, Inputs: inputs,
		Sessions: st, Audit: audit, Outbox: outbox,
		WorkerWatcher:       superagentWorkerWatcher{ctx: ctx, store: st, runner: subagents, log: log},
		WorkerInspector:     superagentWorkerInspector{runs: storeSessions{Store: st}, runner: subagents},
		WorkerTreeCanceller: superagentTreeCanceller{runner: subagents},
		Slots:               superagent.NewSlots(st, superagent.SlotPolicy{Max: cfg.Slots, HumanReserved: cfg.HumanSlots}),
		OrchestratorSession: sessionID,
		OrchestratorBot:     cfg.OrchestratorBot,
		WorkerBot:           cfg.WorkerBot,
		Logger:              log,
	})
	if err != nil {
		return nil, err
	}
	if err := registry.SetEnabled(superagentModeID, true); err != nil {
		return nil, fmt.Errorf("enable %s mode: %w", superagentModeID, err)
	}
	go orchestrator.Run(ctx, cfg.Interval)
	log.Info("super-agent queue enabled",
		"source", cfg.Source, "repo", cfg.Repo, "orchestratorBot", cfg.OrchestratorBot,
		"workerBot", cfg.WorkerBot, "workers", cfg.Slots, "humanReserved", cfg.HumanSlots,
		"orchestratorSession", sessionID.String())
	return orchestrator, nil
}

// superagentWorkerWatcher projects a spawned worker sub-agent's terminal run
// state into the core collab SessionIndex. The orchestrator's durable slot
// reconciler then releases the Assignment; this watcher is not another slot
// counter and never creates or authorizes work.
type superagentWorkerWatcher struct {
	ctx    context.Context
	store  *store.Store
	runner *harnessext.SubagentRunner
	log    *slog.Logger
}

func (w superagentWorkerWatcher) Watch(_ context.Context, tenantID, sessionID uuid.UUID) {
	if w.runner == nil || w.store == nil || sessionID == uuid.Nil {
		return
	}
	go func() {
		// The mode's MaxDurationMs is one hour; two hours leaves room for the
		// runner's own settling while still bounding a lost worker.
		result, err := w.runner.Wait(w.ctx, tenantID, sessionID, harnessext.Budget{TimeoutMs: int64((2 * time.Hour) / time.Millisecond)}, "")
		if err != nil && result.Status == "" {
			if w.log != nil {
				w.log.Warn("super-agent worker watcher failed", "session", sessionID, "error", err)
			}
			return
		}
		state := collab.SessionDone
		if result.Status == "cancelled" {
			state = collab.SessionCancelled
		}
		if err := w.store.SetState(w.ctx, tenantID, sessionID, state); err != nil && w.log != nil {
			w.log.Warn("super-agent worker state projection failed", "session", sessionID, "error", err)
		}
	}()
}

// superagentOperator is the independent operator seam for console resume/stop
// and answers. It requires the caller's own task.write grant; the answer body
// never supplies the acting identity.
type superagentOperator struct {
	bridge *accesswire.Bridge
}

func (o superagentOperator) AuthorizeOperator(ctx context.Context, tenantID, ticketID uuid.UUID, actor, action string) error {
	if o.bridge == nil {
		return superagent.ErrOperatorUnavailable
	}
	principal, err := o.bridge.Principal(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("%w: operator authentication required", collab.ErrPermissionDenied)
	}
	effective, err := o.bridge.Effective(ctx, principal)
	if err != nil {
		return err
	}
	scope, held := effective.ScopeOf(access.KeyTaskWrite)
	if !held || !scope.Covers(access.ScopeTenant) {
		return fmt.Errorf("%w: operator action %s needs task.write at tenant scope on ticket %s", collab.ErrPermissionDenied, action, ticketID)
	}
	return nil
}

// superagentTreeCanceller stops a worker's bare sub-agent descendants through
// the runner's own tree. The core Host cancels the worker session; the two
// seams together cover every descendant.
type superagentTreeCanceller struct {
	runner *harnessext.SubagentRunner
}

func (c superagentTreeCanceller) CancelTree(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error {
	if c.runner == nil {
		return nil
	}
	result, err := c.runner.CancelSubtree(ctx, tenantID, sessionID, reason)
	if err != nil {
		return err
	}
	if len(result.FailedIDs) > 0 {
		return fmt.Errorf("superagent: %d worker descendants refused cancellation", len(result.FailedIDs))
	}
	return nil
}

// superagentWorkerInspector resolves the product run truth for a worker
// session. The in-process runner record is the freshest source while the
// process lives; the durable run row is the fallback after a restart.
type superagentWorkerInspector struct {
	runs   harnessext.RunStatusReader
	runner *harnessext.SubagentRunner
}

func (i superagentWorkerInspector) WorkerState(ctx context.Context, tenantID, sessionID uuid.UUID) (superagent.WorkerState, error) {
	if i.runner != nil {
		if record, ok := i.runner.Get(sessionID); ok {
			switch record.Status {
			case "done":
				return superagent.WorkerDone, nil
			case "failed", "budget_exhausted":
				return superagent.WorkerFailed, nil
			case "cancelled":
				return superagent.WorkerCancelled, nil
			case "running":
				return superagent.WorkerActive, nil
			}
		}
	}
	if i.runs == nil {
		return superagent.WorkerMissing, nil
	}
	state, err := i.runs.RunState(ctx, tenantID, sessionID)
	if errors.Is(err, harnessext.ErrNoRun) {
		return superagent.WorkerMissing, nil
	}
	if err != nil {
		return "", err
	}
	switch state.Status {
	case "queued", "pending", "running":
		return superagent.WorkerActive, nil
	case "done":
		return superagent.WorkerDone, nil
	case "failed":
		return superagent.WorkerFailed, nil
	default:
		return superagent.WorkerMissing, nil
	}
}

// superagentPrincipal builds the service principal the orchestrator uses for
// bot.invoke. It is the orchestrator Bot's own identity: the queue never
// borrows a human's authority.
func superagentPrincipal(ctx context.Context, bridge *accesswire.Bridge, tenantID uuid.UUID, botID string, owner uuid.UUID) (access.Principal, error) {
	permissions, err := bridge.BotHarnessPermissions(ctx, tenantID, botID, owner)
	if err != nil {
		return access.Principal{}, fmt.Errorf("super-agent orchestrator harness: %w", err)
	}
	return access.Principal{TenantID: tenantID, AccountID: owner, BotID: botID, HarnessPermissions: permissions}, nil
}

// superagentPermissions is the guarded Host's bot.invoke seam. It authorizes
// the orchestrator Bot's own identity and writes the same bot.invoked audit row
// the tool surface writes.
type superagentPermissions struct {
	bridge    *accesswire.Bridge
	principal access.Principal
}

func (p superagentPermissions) CheckBotInvoke(ctx context.Context, tenantID uuid.UUID, caller collab.SessionRef, targetBotID string) error {
	decision, err := p.bridge.Authorize(ctx, p.principal, access.KeyBotInvoke, access.Resource{
		TenantID: tenantID, Type: "bot", ID: targetBotID,
	})
	result := "ok"
	if err != nil || decision.Denied() {
		result = "denied"
	}
	_ = p.bridge.AuditInvoke(ctx, tenantID, p.principal, targetBotID, caller.SessionID, result)
	if err != nil {
		return fmt.Errorf("super-agent bot.invoke: %w", err)
	}
	if decision.Denied() {
		return fmt.Errorf("%w: bot.invoke on %q: %s", collab.ErrPermissionDenied, targetBotID, decision.Reason)
	}
	return nil
}

// warnSuperagentWorkerGrants checks the worker Bot's Harness whitelist before
// the mode starts. The model-facing report/ask tools and the task.write
// permission are deployment configuration, not implicit grants; a missing
// entry is visible here instead of only as a denied tool call in a worker run.
func warnSuperagentWorkerGrants(ctx context.Context, st *store.Store, tenantID uuid.UUID, botID string, log *slog.Logger) {
	if st == nil || log == nil {
		return
	}
	record, err := st.GetBot(ctx, tenantID, botID)
	if err != nil {
		log.Warn("super-agent worker bot harness is unreadable; report/ask may be unavailable", "bot", botID, "error", err)
		return
	}
	var harness struct {
		Tools   []string `json:"tools"`
		Context struct {
			Permissions []string `json:"permissions"`
		} `json:"context"`
	}
	if err := json.Unmarshal(record.Harness, &harness); err != nil {
		log.Warn("super-agent worker bot harness is invalid; report/ask may be unavailable", "bot", botID, "error", err)
		return
	}
	tools := make(map[string]bool, len(harness.Tools))
	for _, tool := range harness.Tools {
		tools[strings.TrimSpace(tool)] = true
	}
	permissions := make(map[string]bool, len(harness.Context.Permissions))
	for _, key := range harness.Context.Permissions {
		permissions[strings.TrimSpace(key)] = true
	}
	missing := make([]string, 0, 3)
	for _, tool := range []string{toolsurface.Report, toolsurface.Ask} {
		if !tools[tool] {
			missing = append(missing, "tool:"+tool)
		}
	}
	if !permissions[access.KeyTaskWrite] {
		missing = append(missing, "permission:"+access.KeyTaskWrite)
	}
	if len(missing) > 0 {
		log.Warn("super-agent worker bot is missing worker tool grants; the model-facing report/ask tools will be unavailable or denied",
			"bot", botID, "missing", missing)
	}
}

func splitCSV(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out[item] = true
		}
	}
	return out
}
