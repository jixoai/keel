package harnessext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/sessionfork"
)

// Sub-agent primitive. A sub-agent is an independent session (same tenant,
// its own run) started through the same edge commit seam as any other turn.
// The parent receives a structured result; the sub-agent's failure can never
// take the parent's turn down (it is a value, not an exception).
var (
	ErrParentSessionNotFound  = errors.New("harnessext: parent session not found")
	ErrSubagentBudgetExceeded = errors.New("harnessext: sub-agent budget exhausted")
	// ErrSubagentDepthExceeded reports a spawn that would exceed the effective
	// sub-agent depth cap. Depth is a delegation fact: main is depth 0, a
	// spawned child is parent+1, and the refusal happens before any child
	// session, card or input exists.
	ErrSubagentDepthExceeded = errors.New("harnessext: subagent_depth_exceeded")
	// ErrSubagentChildrenExceeded reports a parent that already has the maximum
	// number of active direct children.
	ErrSubagentChildrenExceeded = errors.New("harnessext: subagent_children_exceeded")
	// ErrSubagentLinkInvalid reports a durable spawn link that cannot prove a
	// safe depth (a pre-depth record, or a link naming another mode). It is a
	// fail-closed answer: without a depth the runner refuses to treat the
	// session as a spawn parent.
	ErrSubagentLinkInvalid = errors.New("harnessext: subagent durable link is invalid")
)

// Sub-agent depth encodings. The core collab.Limits.MaxDepth is a positive
// ceiling only (0 means "inherit"); the user-facing settings below are
// interpreted here, in the mode's request layer, before the guarded Host is
// called.
const (
	// SubagentDepthUnlimited disables the mode's depth refusal. Budget, width,
	// concurrency and permission checks still run.
	SubagentDepthUnlimited = -1
	// SubagentDepthForbidden refuses every spawn from any session.
	SubagentDepthForbidden = 0
	// DefaultSubagentMaxDepth is the mode's default three-level tree.
	DefaultSubagentMaxDepth = 3
	// DefaultSubagentMaxChildren is the mode's default direct-children cap.
	DefaultSubagentMaxChildren = 8
)

// SubagentDepthError is the structured depth refusal. It wraps the sentinel so
// callers branch with errors.Is while the ids/numbers stay readable.
type SubagentDepthError struct {
	ParentSessionID uuid.UUID
	Depth           int
	MaxDepth        int
	Reason          string
}

func (e *SubagentDepthError) Error() string {
	detail := e.Reason
	if detail == "" {
		detail = fmt.Sprintf("child depth %d exceeds the sub-agent max depth %d", e.Depth, e.MaxDepth)
	}
	return fmt.Sprintf("%s: %s (parent %s)", ErrSubagentDepthExceeded, detail, e.ParentSessionID)
}

func (e *SubagentDepthError) Unwrap() error { return ErrSubagentDepthExceeded }

// SubagentChildrenError is the structured width refusal from the guarded Host.
type SubagentChildrenError struct {
	ParentSessionID uuid.UUID
	Active          int64
	Limit           int64
	Err             error
}

func (e *SubagentChildrenError) Error() string {
	return fmt.Sprintf("%s: parent %s has %d active children (limit %d)", ErrSubagentChildrenExceeded, e.ParentSessionID, e.Active, e.Limit)
}

func (e *SubagentChildrenError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrSubagentChildrenExceeded}
	}
	return []error{ErrSubagentChildrenExceeded, e.Err}
}

// Budget bounds one sub-agent execution. Zero means "inherit the runner
// default": no turn/token cap and the default timeout.
type Budget struct {
	MaxTurns  int   `json:"maxTurns,omitempty"`
	MaxTokens int   `json:"maxTokens,omitempty"`
	TimeoutMs int64 `json:"timeoutMs,omitempty"`
}

func (b Budget) timeout(def time.Duration) time.Duration {
	if b.TimeoutMs <= 0 {
		return def
	}
	return time.Duration(b.TimeoutMs) * time.Millisecond
}

// SpawnRequest asks for one sub-agent run.
type SpawnRequest struct {
	TenantID        uuid.UUID `json:"-"`
	ParentSessionID uuid.UUID `json:"-"`
	Task            string    `json:"task"`
	Agent           string    `json:"agent,omitempty"`
	// BotID is the Bot the child session runs as. It travels into the child's
	// input as selection.botId, so the child attempt resolves that Bot's prompt,
	// tool grants and default model exactly like any other bound run. Agent
	// stays the free-form label the record and hooks carry.
	BotID string `json:"botId,omitempty"`
	// Selection, when set, is a complete selection fact for the child input
	// (model identity and/or execution binding). It takes precedence over
	// BotID, which the runner folds into it.
	Selection json.RawMessage `json:"selection,omitempty"`
	// IdempotencyKey makes the derived child session deterministic: the same
	// key in the same parent always targets the same child session, so a retry
	// reuses the committed input instead of spawning a second agent.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Budget         Budget `json:"budget,omitempty"`
	// MaxDepth optionally narrows this spawn's depth cap. It can only narrow
	// the deployment's sub-agent setting: -1 (unlimited) is accepted only when
	// the deployment itself is unlimited, and values above the effective cap
	// are an invalid request, not a silent clamp. 0 refuses this spawn.
	MaxDepth *int `json:"maxDepth,omitempty"`
	// Depth is the parent+1 depth the guarded Host computed. Only the
	// CollabSubagentLauncher sets it; a direct caller leaves it 0 and the
	// runner derives it before creating anything.
	Depth int `json:"-"`
	// MaxChildren is the effective direct-children cap projected into the
	// durable link (the Host enforces it; the record is the UI/read view).
	MaxChildren int `json:"-"`
	// ModeID labels the created session (empty uses the runner's mode).
	ModeID string `json:"-"`
}

// childSelection derives the child input's selection fact from the request. It
// is a fact like any other: the child attempt resolves its Agent binding from
// it, and an empty result stays absent (the child then runs the deployment
// default).
func (r SpawnRequest) childSelection() json.RawMessage {
	if raw := bytes.TrimSpace(r.Selection); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		return append(json.RawMessage(nil), raw...)
	}
	botID := strings.TrimSpace(r.BotID)
	if botID == "" {
		return nil
	}
	encoded, err := json.Marshal(map[string]string{"botId": botID})
	if err != nil {
		return nil
	}
	return encoded
}

// spawnedBotID is the member Bot a spawn delegates to. It follows the same
// precedence as childSelection(): a non-empty selection is the complete
// execution fact and wins over the top-level botId; a team selection
// (teamBotId) keeps the parent's captain ownership because the team decides the
// captain. When both name different Bots the HTTP edge authorizes both, but the
// child may only be owned by the one the binding actually executes.
func (r *SubagentRunner) spawnedBotID(req SpawnRequest) string {
	raw := bytes.TrimSpace(req.Selection)
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var fact struct {
			BotID     string `json:"botId"`
			TeamBotID string `json:"teamBotId"`
		}
		if json.Unmarshal(raw, &fact) != nil {
			return ""
		}
		if strings.TrimSpace(fact.TeamBotID) != "" {
			return ""
		}
		return strings.TrimSpace(fact.BotID)
	}
	return strings.TrimSpace(req.BotID)
}

// Artifact is one provenance entry of a sub-agent run. It never carries tool
// output text: the summary is the value, artifacts are the trail. A blob/file
// artifact carries the content-addressed reference only (never the bytes):
// tool outputs land as blob ids, file writes as layerfs hashes and captures as
// a tree hash.
type Artifact struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
	Hash string `json:"hash,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// artifactLimit bounds the artifact trail a parent receives. A long child run
// can write thousands of files; the summary plus a bounded reference list is
// the contract, and the full trail stays readable in the child's ledger.
const artifactLimit = 256

// Usage is the objective accounting of one sub-agent run.
type Usage struct {
	Turns        int   `json:"turns"`
	InputTokens  int   `json:"inputTokens"`
	OutputTokens int   `json:"outputTokens"`
	DurationMs   int64 `json:"durationMs"`
}

// SubagentResult is the structured value the parent receives.
type SubagentResult struct {
	Status          string     `json:"status"`
	Summary         string     `json:"summary"`
	Artifacts       []Artifact `json:"artifacts,omitempty"`
	Usage           Usage      `json:"usage"`
	SessionID       string     `json:"sessionId"`
	ParentSessionID string     `json:"parentSessionId,omitempty"`
	Agent           string     `json:"agent,omitempty"`
	Error           string     `json:"error,omitempty"`
	// Depth is the collaboration depth of this child (main is 0). ModeID names
	// the collaboration mode that owns the link.
	Depth  int    `json:"depth"`
	ModeID string `json:"modeId,omitempty"`
	// MaxDepth / MaxChildren are the effective caps this spawn ran under.
	MaxDepth    int `json:"maxDepth,omitempty"`
	MaxChildren int `json:"maxChildren,omitempty"`
	// Origin / OriginSeq / ParentTaskID make a fork result self-describing: the
	// parent can see which ledger position the child branched from and which
	// card it hangs under without a second status call.
	Origin           string `json:"origin,omitempty"`
	OriginSeq        int64  `json:"originSeq,omitempty"`
	ParentTaskID     string `json:"parentTaskId,omitempty"`
	ConfigHash       string `json:"configHash,omitempty"`
	ToolManifestHash string `json:"toolManifestHash,omitempty"`
}

// SubagentRecord is the observable registry entry of one spawned sub-agent.
type SubagentRecord struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenantId"`
	ParentSessionID string          `json:"parentSessionId"`
	SessionID       string          `json:"sessionId"`
	Task            string          `json:"task"`
	Agent           string          `json:"agent,omitempty"`
	Status          string          `json:"status"`
	StartedAt       time.Time       `json:"startedAt"`
	EndedAt         *time.Time      `json:"endedAt,omitempty"`
	Result          *SubagentResult `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	// Origin is "spawn" or "fork"; OriginSeq is the parent ledger position a
	// fork started at (0 for a spawn).
	Origin    string `json:"origin,omitempty"`
	OriginSeq int64  `json:"originSeq,omitempty"`
	// ParentTaskID is the card the child card points at (set by the fork or
	// subagent link).
	ParentTaskID string `json:"parentTaskId,omitempty"`
	// ConfigHash / ToolManifestHash freeze the parent attempt's run snapshot
	// values at the fork origin (empty for a spawn, which has no parent run
	// snapshot to inherit).
	ConfigHash       string `json:"configHash,omitempty"`
	ToolManifestHash string `json:"toolManifestHash,omitempty"`
	// StartSeq is the child's initial input sequence (the resume point a parent
	// can name).
	StartSeq int64 `json:"startSeq,omitempty"`
	// Depth is the collaboration depth of this child; the spawn subtree is a
	// strict parent+1 chain, while a fork keeps depth out of the tree (it is a
	// ledger branch, not a delegation). ModeID names the owning mode.
	Depth  int    `json:"depth"`
	ModeID string `json:"modeId,omitempty"`
	// MaxDepth / MaxChildren are the caps in force at spawn time.
	MaxDepth    int `json:"maxDepth,omitempty"`
	MaxChildren int `json:"maxChildren,omitempty"`
	// Budget is the effective per-child budget the guarded Host resolved
	// (narrowed by the mode ceiling). Wait uses it, so an empty request budget
	// cannot bypass the mode's turns/tokens/duration caps.
	Budget *Budget `json:"budget,omitempty"`
}

// SubagentSpawnHost is the guarded creation gate every spawn goes through in a
// wired deployment. *collab.Coordinator implements it; the runner never
// constructs the child itself when a Host is present, so tenant, bot.invoke,
// budget, depth, width, concurrency and audit checks cannot be bypassed by an
// entry point.
type SubagentSpawnHost interface {
	CreateSession(ctx context.Context, tenantID uuid.UUID, modeID string, req collab.SessionRequest) (collab.SessionRef, error)
}

// SubagentLink is the durable spawn link of a sub-agent session. ParentSessionID
// is nil for a root session; a spawned child always carries parent+depth.
type SubagentLink struct {
	TenantID        uuid.UUID `json:"-"`
	SessionID       uuid.UUID `json:"sessionId"`
	ParentSessionID uuid.UUID `json:"parentSessionId"`
	ModeID          string    `json:"modeId,omitempty"`
	Depth           int       `json:"depth"`
	MaxDepth        int       `json:"maxDepth,omitempty"`
	MaxChildren     int       `json:"maxChildren,omitempty"`
}

// SubagentLinkStore is the durable projection of spawn links. The ledger's
// first user.message remains the truth; this projection is what makes width
// counting and restart recovery queryable without replaying every child ledger.
// A deployment without it falls back to the in-process registry.
type SubagentLinkStore interface {
	RecordSubagentLink(ctx context.Context, link SubagentLink) error
	SubagentLink(ctx context.Context, tenantID, childID uuid.UUID) (SubagentLink, bool, error)
	SubagentChildren(ctx context.Context, tenantID, parentID uuid.UUID) ([]SubagentLink, error)
}

// SubagentOptions configures the runner.
type SubagentOptions struct {
	Gate    SessionGate
	Edge    kernel.Committer
	Events  kernel.Reader
	Runs    RunStatusReader
	Control *RunControl
	Hooks   *HookBus
	Logger  *slog.Logger
	// Poll bounds how often the run row is read while waiting (default 250ms).
	Poll time.Duration
	// DefaultTimeout bounds a wait when the request carries no timeout
	// (default 10 minutes).
	DefaultTimeout time.Duration
	Now            func() time.Time
	// ValidateSelection is the same submit-time gate the session input edge
	// runs (the host binds it to the current model generation's
	// ValidateSelection). Spawn validates the child selection before the child
	// session or its user.message exists, so a selection the next attempt would
	// deny is refused at the spawn call instead of failing every attempt.
	// Nil is fail-closed: a selection is refused when no validator is wired.
	ValidateSelection func(json.RawMessage) error
	// Bindings projects the child's selection onto the session binding index, so
	// a live child session blocks its Agent's delete/archive exactly like a
	// session started through POST /input. Nil keeps the pre-projection behavior.
	Bindings SessionBinder
	// Forks records the durable fork origin (the store implements it). Fork
	// refuses without it: a fork whose file/ledger prefix is not durable is a
	// broken session, not a best-effort one.
	Forks sessionfork.Store
	// Snapshots resolves the parent's file-view tree at a fork origin.
	Snapshots ForkSnapshotReader
	// Cards resolves the origin card and creates the forked card (parentTaskId).
	Cards ForkCardStore
	// Admission releases/leaves a slot reservation left by a failed first
	// input. Nil skips the compensation (a deployment without placement).
	Admission ForkAdmission
	// Cleanup archives/deletes the child session of a fork that never started.
	// Nil skips the compensation; the card is still closed and the fact cleared.
	Cleanup ForkSessionCleanup
	// Requeue re-enqueues an existing run for resume (the store implements it).
	// Resume refuses without it.
	Requeue RunRequeuer
	// Notes appends the parent-card task.note when a sub-agent returns. Nil
	// leaves the ledger note out; the structured result is unaffected.
	Notes SubagentNoteWriter
	// ConfigVersion reports the configuration identity the next attempt will
	// run under (the current loadout). Resume returns it so the caller can tell
	// "resumed under revision N" from a guess.
	ConfigVersion func() (hash string, revision int64)
	// InheritOwnership copies the parent session's ownership/owner relation to a
	// newly created child (spawn/fork) before the child's first input. Nil
	// leaves the child unowned: the route relation gate then denies its
	// single-resource routes (fail closed), so a deployment with the identity
	// surface must wire this. A failure aborts the child before any event is
	// committed.
	InheritOwnership func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID) error
	// OwnSpawnedChild creates a spawned child's ownership. When botID is
	// non-empty the child belongs to that member Bot (its Profile's Account);
	// an empty botID inherits the parent. It takes precedence over
	// InheritOwnership for spawn; fork keeps using InheritOwnership because a
	// fork is a copy of the parent's identity, not a delegation to a member.
	OwnSpawnedChild func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID, botID string) error
	// Host is the guarded collab creation gate. When wired, SpawnAsync checks
	// the mode depth policy and then delegates tenant/permission/budget/width/
	// concurrency/audit enforcement to the Host; the Host calls Launch, which
	// creates the child through spawnChild. Nil keeps the pre-mode behavior for
	// tests and deployments without the collaboration layer.
	Host SubagentSpawnHost
	// Links is the durable spawn-link projection (parent/depth/mode). Nil keeps
	// only the ledger link, which is enough for depth recovery but not for
	// durable width counting or subtree cancellation.
	Links SubagentLinkStore
	// MaxDepth is the deployment's sub-agent depth setting: nil means the
	// 3-level default, -1 is unlimited, 0 forbids spawning. The mode request
	// layer interprets these values; the core Host only sees a positive cap.
	MaxDepth *int
	// MaxChildren is the deployment's direct-children cap. Zero means the
	// default (8). It is projected into the durable link; the Host enforces it.
	MaxChildren int
	// ModeID labels the created sessions. Empty defaults to collab.ModeSubagent.
	ModeID string
}

const (
	defaultSubagentPoll    = 250 * time.Millisecond
	defaultSubagentTimeout = 10 * time.Minute
	maxSubagentRecords     = 512
)

// SubagentRunner spawns and observes sub-agent sessions.
type SubagentRunner struct {
	gate           SessionGate
	starter        *TurnStarter
	events         kernel.Reader
	runs           RunStatusReader
	control        *RunControl
	hooks          *HookBus
	log            *slog.Logger
	poll           time.Duration
	defaultTimeout time.Duration
	now            func() time.Time
	// validateSelection is the input edge's submit-time selection gate (nil is
	// fail-closed, see SubagentOptions.ValidateSelection).
	validateSelection func(json.RawMessage) error

	forks            sessionfork.Store
	snapshots        ForkSnapshotReader
	cards            ForkCardStore
	admission        ForkAdmission
	cleanup          ForkSessionCleanup
	requeue          RunRequeuer
	notes            SubagentNoteWriter
	configVersion    func() (string, int64)
	inheritOwnership func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID) error
	ownSpawnedChild  func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID, botID string) error

	// Spawn gate / depth policy. optsMu guards the late SetHost call during
	// assembly; every read takes the lock only to copy the interface.
	optsMu      sync.RWMutex
	host        SubagentSpawnHost
	links       SubagentLinkStore
	maxDepth    int
	maxChildren int
	modeID      string
	// spawnMu serializes the guarded creation section (idempotency lookup,
	// depth/width checks and the Host/launcher call). The core width guard is a
	// check-then-act; one process serializing its own spawns is what keeps a
	// burst from all passing the same pre-check.
	spawnMu sync.Mutex

	mu      sync.Mutex
	records map[string]*SubagentRecord
	order   []string
}

// NewSubagentRunner validates the inputs.
func NewSubagentRunner(opts SubagentOptions) *SubagentRunner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultSubagentPoll
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = defaultSubagentTimeout
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	maxDepth := DefaultSubagentMaxDepth
	if opts.MaxDepth != nil {
		maxDepth = *opts.MaxDepth
	}
	maxChildren := opts.MaxChildren
	if maxChildren <= 0 {
		maxChildren = DefaultSubagentMaxChildren
	}
	modeID := strings.TrimSpace(opts.ModeID)
	if modeID == "" {
		modeID = collab.ModeSubagent
	}
	r := &SubagentRunner{
		gate:              opts.Gate,
		events:            opts.Events,
		runs:              opts.Runs,
		control:           opts.Control,
		hooks:             opts.Hooks,
		log:               opts.Logger,
		poll:              opts.Poll,
		defaultTimeout:    opts.DefaultTimeout,
		now:               opts.Now,
		records:           make(map[string]*SubagentRecord),
		validateSelection: opts.ValidateSelection,
		forks:             opts.Forks,
		snapshots:         opts.Snapshots,
		cards:             opts.Cards,
		admission:         opts.Admission,
		cleanup:           opts.Cleanup,
		requeue:           opts.Requeue,
		notes:             opts.Notes,
		configVersion:     opts.ConfigVersion,
		inheritOwnership:  opts.InheritOwnership,
		ownSpawnedChild:   opts.OwnSpawnedChild,
		host:              opts.Host,
		links:             opts.Links,
		maxDepth:          maxDepth,
		maxChildren:       maxChildren,
		modeID:            modeID,
	}
	if opts.Gate != nil && opts.Edge != nil {
		r.starter = NewTurnStarter(opts.Gate, opts.Edge, opts.Hooks, opts.Logger)
		r.starter.Bindings = opts.Bindings
	}
	return r
}

// SetHost installs the guarded creation gate. It is an assembly-time call: the
// product layer builds the Host after the runner (the Host's launcher resolves
// the runner lazily), so the seam is filled before any request is served.
func (r *SubagentRunner) SetHost(host SubagentSpawnHost) {
	r.optsMu.Lock()
	defer r.optsMu.Unlock()
	r.host = host
}

func (r *SubagentRunner) spawnHost() SubagentSpawnHost {
	r.optsMu.RLock()
	defer r.optsMu.RUnlock()
	return r.host
}

func (r *SubagentRunner) linkStore() SubagentLinkStore {
	r.optsMu.RLock()
	defer r.optsMu.RUnlock()
	return r.links
}

// subagentLaunchInput is the mode-specific part of a Host SessionRequest. The
// Host guards carry the generic fields (task, bot, budget, depth); this
// document carries what only the sub-agent mode knows: the free-form agent
// label, the child selection fact and the request-level depth narrowing.
type subagentLaunchInput struct {
	Agent       string          `json:"agent,omitempty"`
	Selection   json.RawMessage `json:"selection,omitempty"`
	MaxDepth    *int            `json:"maxDepth,omitempty"`
	MaxChildren int             `json:"maxChildren,omitempty"`
}

// SpawnAsync starts a sub-agent and returns immediately. Every spawn passes the
// mode's depth policy and, when a Host is wired, the Host's single creation
// gate; the child run is owned by the worker (call Wait to collect the
// structured result).
func (r *SubagentRunner) SpawnAsync(ctx context.Context, req SpawnRequest) (SubagentRecord, error) {
	req.Task = strings.TrimSpace(req.Task)
	if err := r.validateSpawn(ctx, req); err != nil {
		return SubagentRecord{}, err
	}

	// The guarded creation section is serialized: the core width guard is a
	// check-then-act, and an idempotent retry must observe the first spawn's
	// child before depth/width/launch run again.
	r.spawnMu.Lock()
	defer r.spawnMu.Unlock()
	if record, ok, err := r.idempotentSpawn(ctx, req); err != nil {
		return SubagentRecord{}, err
	} else if ok {
		return record, nil
	}

	// The mode request layer owns the -1/0 depth encodings. The core Host only
	// sees a positive cap (or 0, which it treats as "no core cap"), so the
	// special values must be resolved here, before anything is created.
	effectiveMaxDepth, err := r.effectiveMaxDepth(req.MaxDepth)
	if err != nil {
		return SubagentRecord{}, err
	}
	parentDepth, err := r.parentDepth(ctx, req.TenantID, req.ParentSessionID)
	if err != nil {
		return SubagentRecord{}, err
	}
	depth := parentDepth + 1
	if effectiveMaxDepth == SubagentDepthForbidden || (effectiveMaxDepth > 0 && depth > effectiveMaxDepth) {
		reason := ""
		if effectiveMaxDepth == SubagentDepthForbidden {
			reason = "sub-agent spawning is disabled (maxDepth=0)"
		}
		return SubagentRecord{}, &SubagentDepthError{
			ParentSessionID: req.ParentSessionID,
			Depth:           depth,
			MaxDepth:        effectiveMaxDepth,
			Reason:          reason,
		}
	}

	if host := r.spawnHost(); host != nil {
		return r.spawnThroughHost(ctx, host, req, depth, effectiveMaxDepth)
	}

	// A deployment without the collaboration Host keeps the local width check:
	// active children are the running records this process started.
	if r.maxChildren > 0 {
		if active := r.activeChildCount(req.TenantID, req.ParentSessionID); active >= r.maxChildren {
			return SubagentRecord{}, &SubagentChildrenError{
				ParentSessionID: req.ParentSessionID,
				Active:          int64(active),
				Limit:           int64(r.maxChildren),
			}
		}
	}
	req.Depth = depth
	req.MaxChildren = r.maxChildren
	return r.spawnChild(ctx, req)
}

// subagentChildSessionID is the deterministic child id of an idempotent spawn.
// The same tenant+parent+key always names the same child, so a retry converges
// instead of creating a second agent.
func subagentChildSessionID(tenantID, parentSessionID uuid.UUID, key string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.subagent.v1|"+
		tenantID.String()+"|"+parentSessionID.String()+"|"+key))
}

// idempotentSpawn answers the existing child of a repeated spawn key. It runs
// before the depth/width guards: a retry must return the session the first call
// created, never a session_exists/width_exceeded refusal caused by that same
// child. A cold registry is backed by the durable link (and its projection).
func (r *SubagentRunner) idempotentSpawn(ctx context.Context, req SpawnRequest) (SubagentRecord, bool, error) {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return SubagentRecord{}, false, nil
	}
	childID := subagentChildSessionID(req.TenantID, req.ParentSessionID, key)
	if record, ok := r.Get(childID); ok {
		if record.TenantID == req.TenantID.String() && record.ParentSessionID == req.ParentSessionID.String() {
			return record, true, nil
		}
		return SubagentRecord{}, false, nil
	}
	link, ok, err := r.durableSubagentLink(ctx, req.TenantID, childID)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return SubagentRecord{}, false, err
	}
	if err != nil {
		// The derived child does not exist: this is the first attempt.
		return SubagentRecord{}, false, nil
	}
	if !ok {
		if links := r.linkStore(); links != nil {
			link, ok, err = links.SubagentLink(ctx, req.TenantID, childID)
			if err != nil {
				return SubagentRecord{}, false, err
			}
		}
	}
	if !ok || link.ParentSessionID != req.ParentSessionID {
		return SubagentRecord{}, false, nil
	}
	status := "unknown"
	if r.runs != nil {
		if state, stateErr := r.runs.RunState(ctx, req.TenantID, childID); stateErr == nil {
			status = state.Status
		}
	}
	return SubagentRecord{
		ID:              childID.String(),
		TenantID:        req.TenantID.String(),
		ParentSessionID: req.ParentSessionID.String(),
		SessionID:       childID.String(),
		Task:            req.Task,
		Agent:           req.Agent,
		Status:          status,
		StartedAt:       r.now(),
		Origin:          "spawn",
		Depth:           link.Depth,
		ModeID:          link.ModeID,
		MaxDepth:        link.MaxDepth,
		MaxChildren:     link.MaxChildren,
	}, true, nil
}

// spawnThroughHost submits the guarded SessionRequest and returns the record
// the launcher stored. The Host has already checked tenant, bot.invoke, budget,
// depth, width and concurrency when the launcher runs.
func (r *SubagentRunner) spawnThroughHost(ctx context.Context, host SubagentSpawnHost, req SpawnRequest, depth, maxDepth int) (SubagentRecord, error) {
	inputs, err := json.Marshal(subagentLaunchInput{
		Agent:       req.Agent,
		Selection:   req.childSelection(),
		MaxDepth:    req.MaxDepth,
		MaxChildren: r.maxChildren,
	})
	if err != nil {
		return SubagentRecord{}, fmt.Errorf("%w: encode spawn input: %v", ErrInvalidSpawn, err)
	}
	ref, err := host.CreateSession(ctx, req.TenantID, r.modeID, collab.SessionRequest{
		ParentSessionID: req.ParentSessionID,
		BotID:           r.spawnedBotID(req),
		Task:            req.Task,
		Inputs:          inputs,
		IdempotencyKey:  strings.TrimSpace(req.IdempotencyKey),
		Budget: collab.Budget{
			MaxTurns:  req.Budget.MaxTurns,
			MaxTokens: req.Budget.MaxTokens,
			TimeoutMs: req.Budget.TimeoutMs,
		},
	})
	if err != nil {
		// The Host reports core depth/width refusals with its own codes; the
		// sub-agent mode owns the user-facing vocabulary.
		var collabErr *collab.Error
		if errors.As(err, &collabErr) {
			switch collabErr.Code {
			case collab.CodeDepthExceeded:
				return SubagentRecord{}, &SubagentDepthError{
					ParentSessionID: req.ParentSessionID,
					Depth:           depth,
					MaxDepth:        maxDepth,
					Reason:          collabErr.Detail,
				}
			case collab.CodeWidthExceeded:
				return SubagentRecord{}, &SubagentChildrenError{
					ParentSessionID: req.ParentSessionID,
					Active:          collabErr.Actual,
					Limit:           collabErr.Limit,
					Err:             err,
				}
			}
		}
		return SubagentRecord{}, err
	}
	if record, ok := r.Get(ref.SessionID); ok {
		return record, nil
	}
	// A launcher on another runner instance still returns a complete SessionRef;
	// project it so the caller never loses the child identity.
	record := SubagentRecord{
		ID:              ref.SessionID.String(),
		TenantID:        ref.TenantID.String(),
		ParentSessionID: ref.ParentSessionID.String(),
		SessionID:       ref.SessionID.String(),
		Task:            req.Task,
		Agent:           req.Agent,
		Status:          "running",
		StartedAt:       r.now(),
		Origin:          "spawn",
		Depth:           ref.Depth,
		ModeID:          r.modeID,
		MaxDepth:        maxDepth,
		MaxChildren:     r.maxChildren,
	}
	return record, nil
}

// validateSpawn performs the request-shape checks that are independent of the
// Host (a Host request without a task would be refused there too, but the tool
// and HTTP entries deserve the local error before any resolution work).
func (r *SubagentRunner) validateSpawn(ctx context.Context, req SpawnRequest) error {
	if r.starter == nil || r.runs == nil {
		return errors.New("harnessext: sub-agent runner needs the session gate, the kernel edge and the run reader")
	}
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil {
		return errors.New("harnessext: tenant and parent session are required")
	}
	if strings.TrimSpace(req.Task) == "" {
		return errors.New("harnessext: task is required")
	}
	if _, exists, err := r.gate.SessionOwner(ctx, req.TenantID, req.ParentSessionID); err != nil {
		return err
	} else if !exists {
		return ErrParentSessionNotFound
	}
	selection := req.childSelection()
	if len(selection) > 0 {
		if r.validateSelection == nil {
			return fmt.Errorf("%w: selection is not supported by this sub-agent runner", ErrInvalidSpawn)
		}
		if err := r.validateSelection(selection); err != nil {
			return fmt.Errorf("%w: selection: %v", ErrInvalidSpawn, err)
		}
	}
	return nil
}

// effectiveMaxDepth resolves the request's narrowing against the deployment
// setting. A request may only narrow: -1 is accepted only when the deployment
// is unlimited, and 0 always means "refuse this spawn".
func (r *SubagentRunner) effectiveMaxDepth(requested *int) (int, error) {
	effective := r.maxDepth
	if requested == nil {
		return effective, nil
	}
	value := *requested
	switch {
	case value < SubagentDepthUnlimited:
		return 0, fmt.Errorf("%w: maxDepth %d is below the -1 (unlimited) floor", ErrInvalidSpawn, value)
	case value == SubagentDepthUnlimited:
		if effective != SubagentDepthUnlimited {
			return 0, fmt.Errorf("%w: maxDepth=-1 (unlimited) is wider than the deployment cap %d", ErrInvalidSpawn, effective)
		}
		return SubagentDepthUnlimited, nil
	case value == SubagentDepthForbidden:
		return SubagentDepthForbidden, nil
	case effective == SubagentDepthUnlimited:
		return value, nil
	case value > effective:
		return 0, fmt.Errorf("%w: maxDepth %d is wider than the deployment cap %d", ErrInvalidSpawn, value, effective)
	default:
		return value, nil
	}
}

// parentDepth resolves the parent's delegation depth. The ledger link is the
// restart truth; the durable projection is the fallback for a deployment whose
// event reader is not wired at this seam. A missing link means a root session
// (depth 0); a spawn link without a depth is refused (fail closed).
func (r *SubagentRunner) parentDepth(ctx context.Context, tenantID, parentSessionID uuid.UUID) (int, error) {
	if r.events != nil {
		link, ok, err := r.durableSubagentLink(ctx, tenantID, parentSessionID)
		if err != nil {
			return 0, err
		}
		if ok {
			return link.Depth, nil
		}
	}
	if links := r.linkStore(); links != nil {
		link, ok, err := links.SubagentLink(ctx, tenantID, parentSessionID)
		if err != nil {
			return 0, err
		}
		if ok {
			return link.Depth, nil
		}
	}
	return 0, nil
}

// activeChildCount counts the running children this process started under the
// parent (the no-Host width fallback).
func (r *SubagentRunner) activeChildCount(tenantID, parentSessionID uuid.UUID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, record := range r.records {
		if record.TenantID != tenantID.String() || record.ParentSessionID != parentSessionID.String() {
			continue
		}
		if record.Status == "running" {
			count++
		}
	}
	return count
}

// spawnChild is the unguarded child creation the Host launcher calls after the
// gate accepted the request. Every entry point reaches it through SpawnAsync
// (directly when no Host is wired, or through the Host's launcher otherwise),
// so the depth/link facts are always present.
func (r *SubagentRunner) spawnChild(ctx context.Context, req SpawnRequest) (SubagentRecord, error) {
	selection := req.childSelection()
	childID := uuid.New()
	key := strings.TrimSpace(req.IdempotencyKey)
	requestID := uuid.NewString()
	if key != "" {
		childID = subagentChildSessionID(req.TenantID, req.ParentSessionID, key)
		requestID = "subagent:" + key
	}
	depth := req.Depth
	if depth < 1 {
		// The unguarded caller (a launcher) always supplies the Host depth; a
		// direct call without one still gets a truthful parent+1.
		parentDepth, err := r.parentDepth(ctx, req.TenantID, req.ParentSessionID)
		if err != nil {
			return SubagentRecord{}, err
		}
		depth = parentDepth + 1
	}
	maxDepth, err := r.effectiveMaxDepth(req.MaxDepth)
	if err != nil {
		return SubagentRecord{}, err
	}
	maxChildren := req.MaxChildren
	if maxChildren <= 0 {
		maxChildren = r.maxChildren
	}
	modeID := strings.TrimSpace(req.ModeID)
	if modeID == "" {
		modeID = r.modeID
	}
	extras := map[string]any{
		"subagent": map[string]any{
			"mode":            modeID,
			"parentSessionId": req.ParentSessionID.String(),
			"depth":           depth,
			"maxDepth":        maxDepth,
			"maxChildren":     maxChildren,
			"agent":           req.Agent,
			"idempotencyKey":  key,
			"budget":          req.Budget,
			"origin":          "spawn",
		},
	}
	// The validated selection travels as the child input's own fact.
	if len(selection) > 0 {
		extras["selection"] = selection
	}
	// The child is a session resource: inherit the parent's owner before its
	// first input, so the route relation gate can authorize its single-resource
	// endpoints. A failure aborts before anything is committed.
	spawnBotID := r.spawnedBotID(req)
	if r.ownSpawnedChild != nil {
		if err := r.ownSpawnedChild(ctx, req.TenantID, req.ParentSessionID, childID, spawnBotID); err != nil {
			return SubagentRecord{}, err
		}
	} else if r.inheritOwnership != nil {
		if err := r.inheritOwnership(ctx, req.TenantID, req.ParentSessionID, childID); err != nil {
			return SubagentRecord{}, err
		}
	}
	// The durable projection is written before the first input (the fork path
	// records its fact the same way): a crash after the input must still leave
	// the parent/depth/width facts queryable. The first user.message carries the
	// same document and remains the semantic truth.
	if links := r.linkStore(); links != nil {
		if err := links.RecordSubagentLink(ctx, SubagentLink{
			TenantID:        req.TenantID,
			SessionID:       childID,
			ParentSessionID: req.ParentSessionID,
			ModeID:          modeID,
			Depth:           depth,
			MaxDepth:        maxDepth,
			MaxChildren:     maxChildren,
		}); err != nil {
			return SubagentRecord{}, err
		}
	}
	start, err := r.starter.StartTurn(ctx, StartTurnRequest{
		TenantID:  req.TenantID,
		SessionID: childID,
		Text:      req.Task,
		RequestID: requestID,
		Extras:    extras,
	})
	if err != nil {
		return SubagentRecord{}, err
	}
	record := SubagentRecord{
		ID:              start.SessionID.String(),
		TenantID:        req.TenantID.String(),
		ParentSessionID: req.ParentSessionID.String(),
		SessionID:       start.SessionID.String(),
		Task:            req.Task,
		Agent:           req.Agent,
		Status:          "running",
		StartedAt:       r.now(),
		Origin:          "spawn",
		StartSeq:        start.Seq,
		Depth:           depth,
		ModeID:          modeID,
		MaxDepth:        maxDepth,
		MaxChildren:     maxChildren,
		Budget:          &req.Budget,
	}
	r.store(record)
	r.emit(HookSubagentStarted, record, map[string]string{
		"seq":    fmt.Sprintf("%d", start.Seq),
		"origin": "spawn",
	})
	return record, nil
}

// Spawn starts a sub-agent and waits (bounded by the budget) for its structured
// result.
func (r *SubagentRunner) Spawn(ctx context.Context, req SpawnRequest) (SubagentResult, error) {
	record, err := r.SpawnAsync(ctx, req)
	if err != nil {
		return SubagentResult{}, err
	}
	tenantID, err := uuid.Parse(record.TenantID)
	if err != nil {
		return SubagentResult{}, err
	}
	sessionID, err := uuid.Parse(record.SessionID)
	if err != nil {
		return SubagentResult{}, err
	}
	// The Host may have narrowed the request budget to the mode ceiling; wait
	// under that effective budget so zero request fields cannot bypass it.
	budget := req.Budget
	if record.Budget != nil {
		budget = *record.Budget
	}
	return r.Wait(ctx, tenantID, sessionID, budget, req.Agent)
}

// Wait polls the child's run row until it settles or the budget ends. It is
// safe to call after a restart: the child session and its record are recovered
// from the run row and the event log (the in-process registry is only the
// parent-link index).
func (r *SubagentRunner) Wait(ctx context.Context, tenantID, sessionID uuid.UUID, budget Budget, agent string) (SubagentResult, error) {
	if r.runs == nil {
		return SubagentResult{}, errors.New("harnessext: sub-agent runner has no run reader")
	}
	waitCtx, cancel := context.WithTimeout(ctx, budget.timeout(r.defaultTimeout))
	defer cancel()
	started := r.now()
	lastBudgetCheck := time.Time{}
	var lastState RunState
	for {
		state, err := r.runs.RunState(waitCtx, tenantID, sessionID)
		switch {
		case err == nil:
			lastState = state
		case errors.Is(err, ErrNoRun):
			// The edge commit queues the run in the same transaction, so this
			// is a short lag at most; keep waiting until the budget ends.
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "budget_exhausted", "wait cancelled before the sub-agent settled")
		default:
			return SubagentResult{}, err
		}

		if err == nil {
			if overlay, ok := r.controlStatus(tenantID, sessionID); ok && overlay == "cancelled" {
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "cancelled", "the sub-agent run was cancelled")
			}
			switch state.Status {
			case "done":
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "done", "")
			case "failed":
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "failed", state.LastError)
			}
		}

		if time.Since(lastBudgetCheck) >= time.Second && (budget.MaxTurns > 0 || budget.MaxTokens > 0) {
			lastBudgetCheck = time.Now()
			if reason, exceeded := r.budgetExceeded(waitCtx, tenantID, sessionID, budget); exceeded {
				r.cancelChild(tenantID, sessionID)
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "budget_exhausted", reason)
			}
		}

		select {
		case <-waitCtx.Done():
			if lastState.Status == "" {
				lastState.Status = "unknown"
			}
			r.cancelChild(tenantID, sessionID)
			return r.finish(ctx, tenantID, sessionID, agent, started, budget, "budget_exhausted",
				fmt.Sprintf("timed out waiting for the sub-agent (last run status %s)", lastState.Status))
		case <-time.After(r.poll):
		}
	}
}

func (r *SubagentRunner) controlStatus(tenantID, sessionID uuid.UUID) (string, bool) {
	if r.control == nil {
		return "", false
	}
	return r.control.Status(kernel.Scope{TenantID: tenantID, SessionID: sessionID})
}

func (r *SubagentRunner) budgetExceeded(ctx context.Context, tenantID, sessionID uuid.UUID, budget Budget) (string, bool) {
	events, err := r.readEvents(ctx, tenantID, sessionID)
	if err != nil {
		return "", false
	}
	turns, tokens := 0, 0
	for _, event := range events {
		if event.Kind != kernel.KindAssistantMessage {
			continue
		}
		turns++
		var msg struct {
			Usage *agentcore.Usage `json:"usage"`
		}
		if json.Unmarshal(event.Payload, &msg) == nil && msg.Usage != nil {
			tokens += msg.Usage.InputTokens + msg.Usage.OutputTokens
		}
	}
	if budget.MaxTurns > 0 && turns > budget.MaxTurns {
		return fmt.Sprintf("turn budget exceeded (%d > %d)", turns, budget.MaxTurns), true
	}
	if budget.MaxTokens > 0 && tokens > budget.MaxTokens {
		return fmt.Sprintf("token budget exceeded (%d > %d)", tokens, budget.MaxTokens), true
	}
	return "", false
}

func (r *SubagentRunner) cancelChild(tenantID, sessionID uuid.UUID) {
	if r.control == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelWait)
	defer cancel()
	if _, err := r.control.Cancel(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}); err != nil {
		r.log.Info("sub-agent cancel is not wired; the child run continues", "session_id", sessionID, "err", err)
	}
}

// finish reads the child's log, builds the structured result, updates the
// registry and emits subagent.finished.
func (r *SubagentRunner) finish(ctx context.Context, tenantID, sessionID uuid.UUID, agent string, started time.Time, _ Budget, status, failure string) (SubagentResult, error) {
	events, readErr := r.readEvents(ctx, tenantID, sessionID)
	if readErr != nil && !errors.Is(readErr, kernel.ErrNotFound) {
		r.log.Warn("read sub-agent log", "session_id", sessionID, "err", readErr)
	}
	summary, turns, tokens := summarizeEvents(events)
	result := SubagentResult{
		Status:          status,
		Summary:         summary,
		Artifacts:       artifactsFromEvents(events),
		Usage:           Usage{Turns: turns, InputTokens: tokens.Input, OutputTokens: tokens.Output, DurationMs: r.now().Sub(started).Milliseconds()},
		SessionID:       sessionID.String(),
		ParentSessionID: "",
		Agent:           agent,
	}
	if failure != "" {
		result.Error = failure
	}
	parentSessionID := ""
	if record := r.lookup(sessionID); record != nil {
		result.ParentSessionID = record.ParentSessionID
		result.Origin = record.Origin
		result.OriginSeq = record.OriginSeq
		result.ParentTaskID = record.ParentTaskID
		result.ConfigHash = record.ConfigHash
		result.ToolManifestHash = record.ToolManifestHash
		result.Depth = record.Depth
		result.ModeID = record.ModeID
		result.MaxDepth = record.MaxDepth
		result.MaxChildren = record.MaxChildren
		parentSessionID = record.ParentSessionID
	}
	if parentSessionID == "" {
		// A restart loses the registry; the child's first input carries the
		// durable parent link. Without it the note has no card to land on.
		parentSessionID = r.durableParent(ctx, tenantID, sessionID)
		result.ParentSessionID = parentSessionID
		if link, ok, err := r.durableSubagentLink(ctx, tenantID, sessionID); err == nil && ok {
			result.Depth = link.Depth
			result.ModeID = link.ModeID
			result.MaxDepth = link.MaxDepth
			result.MaxChildren = link.MaxChildren
		}
	}
	now := r.now()
	durationFrom := started
	r.update(sessionID, func(record *SubagentRecord) {
		record.Status = status
		record.EndedAt = &now
		record.Result = &result
		record.Error = failure
		if !record.StartedAt.IsZero() {
			durationFrom = record.StartedAt
		}
	})
	result.Usage.DurationMs = now.Sub(durationFrom).Milliseconds()
	// The parent card's task.note is the return fact the parent reads next to
	// the structured value. It is idempotent through its task-event key, so a
	// repeated Wait cannot duplicate it.
	if parentSessionID != "" {
		if parentID, err := uuid.Parse(parentSessionID); err == nil {
			r.noteParent(ctx, tenantID, parentID, sessionID, status, summary)
		}
	}
	payload := HookPayload{
		Event:             HookSubagentFinished,
		TenantID:          tenantID.String(),
		SessionID:         sessionID.String(),
		ParentSessionID:   parentSessionID,
		SubagentSessionID: sessionID.String(),
		Status:            status,
		At:                now,
	}
	payload.SetData("turns", fmt.Sprintf("%d", turns))
	payload.SetData("durationMs", fmt.Sprintf("%d", result.Usage.DurationMs))
	if r.hooks != nil {
		r.hooks.Emit(payload)
	}
	return result, nil
}

func (r *SubagentRunner) readEvents(ctx context.Context, tenantID, sessionID uuid.UUID) ([]kernel.Event, error) {
	if r.events == nil {
		return nil, nil
	}
	var all []kernel.Event
	var after int64
	for {
		page, err := r.events.LoadEvents(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, after, 500)
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
		after = page[len(page)-1].Seq
	}
}

func summarizeEvents(events []kernel.Event) (string, int, struct{ Input, Output int }) {
	var tokens struct{ Input, Output int }
	turns := 0
	summary := ""
	for _, event := range events {
		switch event.Kind {
		case kernel.KindAssistantMessage:
			turns++
			var msg agentcore.AssistantMessage
			if err := json.Unmarshal(event.Payload, &msg); err != nil {
				continue
			}
			if msg.Usage != nil {
				tokens.Input += msg.Usage.InputTokens
				tokens.Output += msg.Usage.OutputTokens
			}
			var text strings.Builder
			for _, content := range msg.Content {
				if block, ok := content.(agentcore.TextContent); ok {
					text.WriteString(block.Text)
				}
			}
			if text.Len() > 0 {
				summary = text.String()
			}
		}
	}
	return summary, turns, tokens
}

func artifactsFromEvents(events []kernel.Event) []Artifact {
	seen := make(map[string]struct{})
	out := make([]Artifact, 0)
	add := func(artifact Artifact) {
		if len(out) >= artifactLimit {
			return
		}
		key := artifact.Type + "\x00" + artifact.Ref + "\x00" + artifact.Path
		if artifact.Ref == "" && artifact.Path == "" {
			return
		}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, artifact)
	}
	for _, event := range events {
		switch event.Kind {
		case kernel.KindToolResult:
			var result struct {
				ToolCallID string `json:"toolCallId"`
				ToolName   string `json:"toolName"`
				Details    any    `json:"details"`
			}
			if err := json.Unmarshal(event.Payload, &result); err != nil || result.ToolCallID == "" {
				continue
			}
			add(Artifact{Type: "tool", Name: result.ToolName, Ref: result.ToolCallID})
			// A bash result's full output is a blob reference; the trail keeps
			// the id, never the bytes.
			if details, ok := result.Details.(map[string]any); ok {
				for _, stream := range []string{"stdout", "stderr"} {
					entry, ok := details[stream].(map[string]any)
					if !ok {
						continue
					}
					if blobID, ok := entry["blobId"].(string); ok && blobID != "" {
						add(Artifact{Type: "blob", Name: stream, Ref: blobID})
					}
				}
			}
		case kernel.KindFSWrite:
			var write struct {
				Path string `json:"path"`
				Hash string `json:"hash"`
			}
			if err := json.Unmarshal(event.Payload, &write); err != nil {
				continue
			}
			add(Artifact{Type: "file", Name: write.Path, Ref: write.Hash, Path: write.Path, Hash: write.Hash})
		case kernel.KindFSCapture:
			var capture struct {
				TreeHash string `json:"treeHash"`
				Count    int    `json:"count"`
			}
			if err := json.Unmarshal(event.Payload, &capture); err != nil || capture.TreeHash == "" {
				continue
			}
			add(Artifact{Type: "tree", Name: fmt.Sprintf("capture:%d", capture.Count), Ref: capture.TreeHash, Hash: capture.TreeHash})
		}
	}
	return out
}

// List returns sub-agent records, newest first. A non-nil parent filters by
// parent session.
func (r *SubagentRunner) List(tenantID uuid.UUID, parent *uuid.UUID) []SubagentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SubagentRecord, 0, len(r.records))
	for _, record := range r.records {
		if tenantID != uuid.Nil && record.TenantID != tenantID.String() {
			continue
		}
		if parent != nil && record.ParentSessionID != parent.String() {
			continue
		}
		copied := *record
		if record.Result != nil {
			result := *record.Result
			copied.Result = &result
		}
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].SessionID > out[j].SessionID
	})
	return out
}

// ParentOf reports the parent session of a sub-agent session (the session-list
// endpoint uses it to link a child row to its parent while the registry is
// warm; the durable copy of the link lives in the child's first user.message
// payload).
func (r *SubagentRunner) ParentOf(sessionID uuid.UUID) (string, bool) {
	record := r.lookup(sessionID)
	if record == nil || record.ParentSessionID == "" {
		return "", false
	}
	return record.ParentSessionID, true
}

// Get returns one record by child session id.
func (r *SubagentRunner) Get(sessionID uuid.UUID) (SubagentRecord, bool) {
	record := r.lookup(sessionID)
	if record == nil {
		return SubagentRecord{}, false
	}
	return *record, true
}

func (r *SubagentRunner) lookup(sessionID uuid.UUID) *SubagentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[sessionID.String()]
	if record == nil {
		return nil
	}
	copied := *record
	return &copied
}

func (r *SubagentRunner) store(record SubagentRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := record
	r.records[copied.SessionID] = &copied
	r.order = append(r.order, copied.SessionID)
	// Bound the registry: drop the oldest terminal records first.
	for len(r.order) > maxSubagentRecords {
		dropID := r.order[0]
		r.order = r.order[1:]
		if existing := r.records[dropID]; existing != nil && existing.Status == "running" {
			// Never drop a live entry; keep it and re-append the id so the
			// bound applies to settled records only.
			r.order = append(r.order, dropID)
			break
		}
		delete(r.records, dropID)
	}
}

func (r *SubagentRunner) update(sessionID uuid.UUID, mutate func(*SubagentRecord)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[sessionID.String()]
	if record == nil {
		return
	}
	mutate(record)
}

func (r *SubagentRunner) emit(event HookEvent, record SubagentRecord, data map[string]string) {
	if r.hooks == nil {
		return
	}
	payload := HookPayload{
		Event:             event,
		TenantID:          record.TenantID,
		SessionID:         record.SessionID,
		ParentSessionID:   record.ParentSessionID,
		SubagentSessionID: record.SessionID,
		Status:            record.Status,
		At:                r.now(),
	}
	if record.Agent != "" {
		payload.SetData("agent", record.Agent)
	}
	for key, value := range data {
		payload.SetData(key, value)
	}
	r.hooks.Emit(payload)
}

// --- run scope + shared spawn handler ------------------------------------

type runScopeKey struct{}

// RunScope is the ambient run identity the `agent` tool needs to attribute a
// sub-agent to its parent. RunControl.BeginRun installs it on the attempt
// context, so a model tool call inside a run always has it.
type RunScope struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	RunID     string
}

// WithRunScope attaches the run identity to a tool execution context.
func WithRunScope(ctx context.Context, scope RunScope) context.Context {
	return context.WithValue(ctx, runScopeKey{}, scope)
}

// RunScopeFromContext reads the ambient run identity.
func RunScopeFromContext(ctx context.Context) (RunScope, bool) {
	scope, ok := ctx.Value(runScopeKey{}).(RunScope)
	return scope, ok
}

// ErrRunScopeMissing reports a spawn request outside a run (no ambient parent).
var ErrRunScopeMissing = errors.New("harnessext: the spawn action needs the ambient run scope")

// ErrInvalidSpawn reports a malformed spawn request (bad JSON, missing task).
var ErrInvalidSpawn = errors.New("harnessext: invalid spawn arguments")

// SpawnFromTool is the one parser/handler behind the `agent` aggregate tool's
// spawn action. It carries the model's botId into the child input's selection
// fact (the child attempt then resolves that Agent's prompt, tool grants and
// default model), exactly like the HTTP sub-agent endpoint.
func SpawnFromTool(ctx context.Context, runner *SubagentRunner, args json.RawMessage) (SubagentResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok {
		return SubagentResult{}, ErrRunScopeMissing
	}
	if runner == nil {
		return SubagentResult{}, errors.New("harnessext: no sub-agent runner is wired")
	}
	var input struct {
		Task           string `json:"task"`
		Agent          string `json:"agent"`
		BotID          string `json:"botId"`
		IdempotencyKey string `json:"idempotencyKey"`
		MaxTurns       int    `json:"maxTurns"`
		MaxTokens      int    `json:"maxTokens"`
		TimeoutMs      int64  `json:"timeoutMs"`
		MaxDepth       *int   `json:"maxDepth"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return SubagentResult{}, fmt.Errorf("%w: %v", ErrInvalidSpawn, err)
	}
	if strings.TrimSpace(input.Task) == "" {
		return SubagentResult{}, fmt.Errorf("%w: task is required", ErrInvalidSpawn)
	}
	return runner.Spawn(ctx, SpawnRequest{
		TenantID:        scope.TenantID,
		ParentSessionID: scope.SessionID,
		Task:            input.Task,
		Agent:           input.Agent,
		BotID:           input.BotID,
		IdempotencyKey:  input.IdempotencyKey,
		Budget:          Budget{MaxTurns: input.MaxTurns, MaxTokens: input.MaxTokens, TimeoutMs: input.TimeoutMs},
		MaxDepth:        input.MaxDepth,
	})
}

// ErrorToolResult is exported for plugin authors: a tool failure is a result
// the model can read and react to, not a Go error that aborts the batch.
func ErrorToolResult(message string) agentcore.AgentToolResult { return errorToolResult(message) }

func errorToolResult(message string) agentcore.AgentToolResult {
	isError := true
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(message)},
		IsError: &isError,
	}
}
