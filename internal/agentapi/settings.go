package agentapi

// The product's settings surface (change keel-model-settings): the tenant's
// model configuration write path, its secret storage and the plugin inventory.
// It is deliberately separate from the workspace routes (sessions, chat,
// decisions) — settings is its own page in the client, and its endpoints are
// their own /v1/settings/* subtree here.
//
// Tenant scoping: every route reads the tenant from the session middleware's
// injected X-Tenant-ID. No route accepts a tenant id from a body or a query,
// and a body that carries one is refused rather than stored.
//
// Credential discipline: a stored secret value is never echoed, never logged
// and never part of an error. The model write path only ever reports field
// paths and messages; the secret list only reports names and timestamps.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/store"
)

// Model settings sources: where the effective models document comes from.
const (
	modelSettingsSourceStored     = "stored"
	modelSettingsSourceDeployment = "deployment"
)

// secretNamePattern is the secret name grammar, kept identical to the one a
// models document can reference (`secret:NAME` in modelcfg). A wider grammar
// would let the settings surface store a secret that no provider can ever
// reference — a footgun the API refuses instead of documenting.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// ModelSettingsStore is the persistence seam of the tenant models override. It
// is the store package's model-settings API verbatim, so *store.Store satisfies
// it without an adapter; the typed errors
// (store.ModelSettingsConflictError / store.ErrModelSettingsConflict /
// store.ErrNoModelSettings) travel through unchanged.
type ModelSettingsStore interface {
	GetModelSettings(ctx context.Context, tenantID uuid.UUID) (json.RawMessage, int64, time.Time, bool, error)
	SetModelSettings(ctx context.Context, tenantID uuid.UUID, expectedRevision int64, document json.RawMessage) (int64, error)
	DeleteModelSettings(ctx context.Context, tenantID uuid.UUID, expectedRevision int64) (int64, error)
}

// ModelSettingsRuntime is the live-runtime seam of the settings path. The
// implementation lives in the hosting process (cmd/keel over
// servicehost.Runtime), so this package never depends on the service assembly.
type ModelSettingsRuntime interface {
	// DeploymentModels is the deployment-level models document (the boot
	// configuration), nil when the deployment declares none.
	DeploymentModels() json.RawMessage
	// ValidateModelsDocument resolves a candidate document exactly as the next
	// generation would (the same construction an attempt uses). A document the
	// runtime would refuse comes back as field-path issues; error is reserved
	// for a runtime that cannot answer at all.
	ValidateModelsDocument(document json.RawMessage) (ModelCandidate, []modelcfg.Issue, error)
	// ApplyModels reassembles the runtime on the given document; a nil document
	// restores the deployment document. The previous generation keeps serving
	// when the candidate cannot be assembled.
	ApplyModels(ctx context.Context, document json.RawMessage) error
	// RejectedModelsOverride reports a stored override this process refused at
	// boot because its credentials no longer resolve: the deployment document
	// is what is serving, and the read points the operator at the row to fix or
	// delete. ok=false when what is stored is what is running.
	RejectedModelsOverride() (revision int64, reason string, ok bool)
}

// ModelCandidate is the validated summary the PUT response publishes: the
// default selection the next generation would execute, and how many providers
// and models this process could actually serve.
type ModelCandidate struct {
	Selection           modelcfg.Selection
	ProvidersExecutable int
	ModelsSelectable    int
}

// SecretStore is the tenant secret storage seam (PUT/GET/DELETE
// /v1/settings/secrets). The implementation needs the KEEL_SECRET_KEY-backed
// cipher, so it lives in the hosting process. Nil means this deployment has no
// secret storage: reads report it as unavailable and writes are refused.
type SecretStore interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]SecretMeta, error)
	Put(ctx context.Context, tenantID uuid.UUID, name string, value []byte) (time.Time, error)
	Delete(ctx context.Context, tenantID uuid.UUID, name string) (bool, error)
}

// SecretMeta is what a secret list may show: the name and the timestamps.
type SecretMeta struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// --- routes --------------------------------------------------------------

func (s *Server) settingsRoutes() {
	s.mux.HandleFunc("GET /v1/settings/models", s.getModelSettings)
	s.mux.HandleFunc("PUT /v1/settings/models", s.putModelSettings)
	s.mux.HandleFunc("DELETE /v1/settings/models", s.deleteModelSettings)
	s.mux.HandleFunc("GET /v1/settings/secrets", s.listSecrets)
	s.mux.HandleFunc("PUT /v1/settings/secrets/{name}", s.putSecret)
	s.mux.HandleFunc("DELETE /v1/settings/secrets/{name}", s.deleteSecret)
	s.mux.HandleFunc("GET /v1/settings/plugins", s.settingsPlugins)
}

// --- model settings ------------------------------------------------------

// modelSettingsResponse is the GET /v1/settings/models body. selection is the
// additive field the settings page needs to show the effective default even
// when the document itself declares none (a deployment whose default comes from
// the flat agent fields): it is the current generation's default selection, the
// one a run without a selection of its own resolves.
type modelSettingsResponse struct {
	Revision  int64              `json:"revision"`
	Source    string             `json:"source"`
	Document  json.RawMessage    `json:"document"`
	Selection modelcfg.Selection `json:"selection"`
	Issues    []modelcfg.Issue   `json:"issues"`
}

// modelSettingsWriteResponse is the PUT /v1/settings/models body.
type modelSettingsWriteResponse struct {
	Revision            int64              `json:"revision"`
	Source              string             `json:"source"`
	Selection           modelcfg.Selection `json:"selection"`
	ProvidersExecutable int                `json:"providersExecutable"`
	ModelsSelectable    int                `json:"modelsSelectable"`
}

// modelSettingsResetResponse is the DELETE /v1/settings/models body.
type modelSettingsResetResponse struct {
	Revision int64  `json:"revision"`
	Source   string `json:"source"`
}

// modelSettingsWired reports whether the model settings path has both seams.
func (s *Server) modelSettingsWired() bool {
	return s.cfg.ModelSettingsStore != nil && s.cfg.ModelSettingsRuntime != nil
}

// getModelSettings implements GET /v1/settings/models: the effective models
// document, where it comes from, and the resolution diagnostics of the
// generation that is serving right now.
func (s *Server) getModelSettings(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.modelSettingsWired() {
		notWired(w, "model settings are not wired")
		return
	}
	document, revision, _, found, err := s.cfg.ModelSettingsStore.GetModelSettings(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	source := modelSettingsSourceDeployment
	rejectedRevision, rejectedReason, rejected := s.cfg.ModelSettingsRuntime.RejectedModelsOverride()
	if found && !rejected {
		source = modelSettingsSourceStored
	} else {
		// What is serving is the runtime's boot document, not a re-read of the
		// file: the runtime is the authority. A stored override this process
		// refused at boot is not serving; the read says so below.
		document = s.cfg.ModelSettingsRuntime.DeploymentModels()
	}
	if len(document) == 0 {
		document = json.RawMessage("{}")
	}
	var selection modelcfg.Selection
	if catalog := s.catalog(); catalog != nil {
		selection = catalog.DefaultSelection()
	}
	issues := s.modelResolutionIssues()
	if rejected {
		issues = append(issues, modelcfg.Issue{
			Field: "storedOverride",
			Message: fmt.Sprintf(
				"the saved override at revision %d cannot be served and is not in effect: %s. The deployment document is serving; save a working document or reset to the deployment default.",
				rejectedRevision, rejectedReason),
		})
	}
	writeJSON(w, http.StatusOK, modelSettingsResponse{
		Revision:  revision,
		Source:    source,
		Document:  document,
		Selection: selection,
		Issues:    issues,
	})
}

// modelResolutionIssues is the resolution-level diagnostic set of the current
// generation: the same facts GET /v1/providers publishes, so the settings view
// and the registry view cannot disagree about the state being served.
func (s *Server) modelResolutionIssues() []modelcfg.Issue {
	catalog := s.catalog()
	if catalog == nil {
		return []modelcfg.Issue{noProvidersIssue()}
	}
	return resolutionIssues(catalog, visibleProviderViews(catalog.ProviderViews()))
}

// putModelSettings implements PUT /v1/settings/models. The composition (validate
// with the runtime's own construction, persist, apply) lives in
// applyModelSettings; this handler only shapes the wire.
func (s *Server) putModelSettings(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.modelSettingsWired() {
		notWired(w, "model settings are not wired")
		return
	}
	document, expectedRevision, err := s.readSettingsDocument(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.applyModelSettings(r.Context(), tenantID, expectedRevision, document)
	if err != nil {
		s.writeSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelSettingsWriteResponse{
		Revision:            result.revision,
		Source:              modelSettingsSourceStored,
		Selection:           result.candidate.Selection,
		ProvidersExecutable: result.candidate.ProvidersExecutable,
		ModelsSelectable:    result.candidate.ModelsSelectable,
	})
}

// settingsWriteResult is one accepted models write.
type settingsWriteResult struct {
	revision  int64
	candidate ModelCandidate
}

// applyModelSettings is the PUT composition: validate through the runtime's own
// construction (so a stored document can never be one an attempt would deny),
// persist, then apply. A write the runtime refuses to assemble is compensated —
// the store returns to its previous state — so the stored override never lies
// about what is serving, and the client gets the reload error.
func (s *Server) applyModelSettings(ctx context.Context, tenantID uuid.UUID, expectedRevision int64, document json.RawMessage) (settingsWriteResult, error) {
	runtime := s.cfg.ModelSettingsRuntime
	candidate, issues, err := runtime.ValidateModelsDocument(document)
	if err != nil {
		return settingsWriteResult{}, err
	}
	if len(issues) > 0 {
		return settingsWriteResult{}, &settingsValidationError{issues: issues}
	}
	previous, _, _, hadOverride, err := s.cfg.ModelSettingsStore.GetModelSettings(ctx, tenantID)
	if err != nil {
		return settingsWriteResult{}, err
	}
	revision, err := s.cfg.ModelSettingsStore.SetModelSettings(ctx, tenantID, expectedRevision, document)
	if err != nil {
		return settingsWriteResult{}, err
	}
	if err := runtime.ApplyModels(ctx, document); err != nil {
		s.rollbackModelSettings(ctx, tenantID, revision, previous, hadOverride)
		return settingsWriteResult{}, &settingsReloadError{err: err}
	}
	return settingsWriteResult{revision: revision, candidate: candidate}, nil
}

// rollbackModelSettings restores the stored state a rejected write replaced:
// the previous override, or no override at all. Both compensations are guarded
// by the revision of the write being undone, so a concurrent accepted write is
// never clobbered. A failed rollback is logged (never the document): the stored
// state and the applied generation would then disagree until the next write,
// which is exactly what a restart resolves.
func (s *Server) rollbackModelSettings(ctx context.Context, tenantID uuid.UUID, revision int64, previous json.RawMessage, hadOverride bool) {
	var err error
	if hadOverride {
		_, err = s.cfg.ModelSettingsStore.SetModelSettings(ctx, tenantID, revision, previous)
	} else {
		_, err = s.cfg.ModelSettingsStore.DeleteModelSettings(ctx, tenantID, revision)
	}
	if err != nil {
		s.cfg.Logger.Error("model settings rollback failed",
			"tenant_id", tenantID, "revision", revision, "error", err)
	}
}

// deleteModelSettings implements DELETE /v1/settings/models: clear the override
// and go back to the deployment document. A tenant with no override is already
// in the requested state, so the reset is idempotent: it answers 200 with the
// current revision and source=deployment instead of failing a repeated click.
func (s *Server) deleteModelSettings(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.modelSettingsWired() {
		notWired(w, "model settings are not wired")
		return
	}
	expectedRevision, err := settingsExpectedRevision(r.URL.Query().Get("expectedRevision"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	document, revision, _, found, err := s.cfg.ModelSettingsStore.GetModelSettings(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, modelSettingsResetResponse{
			Revision: revision, Source: modelSettingsSourceDeployment,
		})
		return
	}
	if expectedRevision > 0 && expectedRevision != revision {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    fmt.Sprintf("model settings revision conflict: the current revision is %d", revision),
			"revision": revision,
		})
		return
	}
	// The delete is guarded by the revision just read: a concurrent write between
	// the read and the delete is refused instead of silently discarded.
	newRevision, err := s.cfg.ModelSettingsStore.DeleteModelSettings(r.Context(), tenantID, revision)
	if err != nil {
		s.writeSettingsError(w, err)
		return
	}
	if err := s.cfg.ModelSettingsRuntime.ApplyModels(r.Context(), nil); err != nil {
		// The reset could not be applied: put the override back, guarded by the
		// revision the delete produced.
		if restored, restoreErr := s.cfg.ModelSettingsStore.SetModelSettings(r.Context(), tenantID, newRevision, document); restoreErr != nil {
			// restored is 0 on error; name the revision the rollback tried to
			// write instead of reporting a number that never existed.
			s.cfg.Logger.Error("model settings reset rollback failed",
				"tenant_id", tenantID, "expected_revision", newRevision, "written_revision", restored, "error", restoreErr)
		}
		s.writeSettingsError(w, &settingsReloadError{err: err})
		return
	}
	writeJSON(w, http.StatusOK, modelSettingsResetResponse{
		Revision: newRevision, Source: modelSettingsSourceDeployment,
	})
}

// settingsValidationError is a rejected models document: the field-path issues
// the settings view renders.
type settingsValidationError struct{ issues []modelcfg.Issue }

func (e *settingsValidationError) Error() string {
	parts := make([]string, 0, len(e.issues))
	for _, issue := range e.issues {
		if issue.Field == "" {
			parts = append(parts, issue.Message)
			continue
		}
		parts = append(parts, issue.Field+": "+issue.Message)
	}
	if len(parts) == 0 {
		return "models document rejected"
	}
	return "models document rejected: " + strings.Join(parts, "; ")
}

// settingsReloadError is a document the runtime validated but could not
// assemble. The store has been compensated by the time it is reported.
type settingsReloadError struct{ err error }

func (e *settingsReloadError) Error() string {
	return "the runtime could not apply the models document: " + e.err.Error()
}
func (e *settingsReloadError) Unwrap() error { return e.err }

// writeSettingsError maps the write path's failures onto the wire: a rejected
// document is 400 with its issues, a revision guard failure is 409 with the
// revision that won, a document the runtime refuses to assemble is 422 (it
// passed validation, so this is the runtime refusing, not the client — 409 is
// reserved for the optimistic-concurrency guard), everything else 500.
func (s *Server) writeSettingsError(w http.ResponseWriter, err error) {
	var validation *settingsValidationError
	if errors.As(err, &validation) {
		issues := validation.issues
		if issues == nil {
			issues = []modelcfg.Issue{}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  validation.Error(),
			"issues": issues,
		})
		return
	}
	var conflict *store.ModelSettingsConflictError
	if errors.As(err, &conflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    err.Error(),
			"revision": conflict.Current,
		})
		return
	}
	if errors.Is(err, store.ErrModelSettingsConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	var reload *settingsReloadError
	if errors.As(err, &reload) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": reload.Error()})
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// readSettingsDocument reads the PUT body: the models document plus the
// optional expectedRevision sibling. expectedRevision is stripped and never
// persisted; every other key travels verbatim (the resolver owns the
// vocabulary). A body that names a tenant is refused: the tenant comes from the
// session, never from the document.
func (s *Server) readSettingsDocument(w http.ResponseWriter, r *http.Request) (json.RawMessage, int64, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBody))
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, 0, errors.New("a models document is required")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, 0, errors.New("the models document must be a JSON object")
	}
	if _, ok := fields["tenantId"]; ok {
		return nil, 0, errors.New("the models document must not carry a tenantId: the tenant comes from the session")
	}
	var expectedRevision int64
	if raw, ok := fields["expectedRevision"]; ok {
		if err := json.Unmarshal(raw, &expectedRevision); err != nil || expectedRevision < 0 {
			return nil, 0, errors.New("expectedRevision must be a non-negative integer")
		}
		delete(fields, "expectedRevision")
	}
	document, err := json.Marshal(fields)
	if err != nil {
		return nil, 0, fmt.Errorf("encode document: %w", err)
	}
	return document, expectedRevision, nil
}

// settingsExpectedRevision parses the optional expectedRevision query parameter
// of a reset. 0 (absent) means "no expectation".
func settingsExpectedRevision(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("expectedRevision must be a non-negative integer")
	}
	return value, nil
}

// --- secrets -------------------------------------------------------------

// secretsResponse is the GET /v1/settings/secrets body. available=false means
// this deployment has no secret storage (KEEL_SECRET_KEY is not configured);
// writes answer 503 with the same reason. The list never carries a value, a
// length or any other datum about one.
type secretsResponse struct {
	Secrets   []SecretMeta `json:"secrets"`
	Available bool         `json:"available"`
	Reason    string       `json:"reason,omitempty"`
}

// secretWriteResponse is the PUT /v1/settings/secrets/{name} body.
type secretWriteResponse struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// secretDeleteResponse is the DELETE /v1/settings/secrets/{name} body.
type secretDeleteResponse struct {
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

// secretsUnavailableReason is the operator-facing explanation rendered whenever
// the secret store is missing. It names the configuration, never a value.
func (s *Server) secretsUnavailableReason() string {
	if strings.TrimSpace(s.cfg.SecretStoreReason) != "" {
		return s.cfg.SecretStoreReason
	}
	return "tenant secret storage is not enabled: KEEL_SECRET_KEY is not configured"
}

// listSecrets implements GET /v1/settings/secrets.
func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.SecretStore == nil {
		writeJSON(w, http.StatusOK, secretsResponse{
			Secrets:   []SecretMeta{},
			Available: false,
			Reason:    s.secretsUnavailableReason(),
		})
		return
	}
	secrets, err := s.cfg.SecretStore.List(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if secrets == nil {
		secrets = []SecretMeta{}
	}
	writeJSON(w, http.StatusOK, secretsResponse{Secrets: secrets, Available: true})
}

// putSecret implements PUT /v1/settings/secrets/{name}: write or overwrite one
// named secret. The value travels one way only — it is never echoed, never
// logged and never part of an error.
func (s *Server) putSecret(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !secretNamePattern.MatchString(name) {
		writeErr(w, http.StatusBadRequest,
			errors.New("secret name must match [A-Za-z_][A-Za-z0-9_]{0,63} (the grammar a models document can reference)"))
		return
	}
	if s.cfg.SecretStore == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New(s.secretsUnavailableReason()))
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Value == "" {
		writeErr(w, http.StatusBadRequest, errors.New("value is required"))
		return
	}
	updatedAt, err := s.cfg.SecretStore.Put(r.Context(), tenantID, name, []byte(body.Value))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, secretWriteResponse{Name: name, UpdatedAt: updatedAt})
}

// deleteSecret implements DELETE /v1/settings/secrets/{name}. It is idempotent:
// deleting a name that is not stored answers 200 with deleted=false.
func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !secretNamePattern.MatchString(name) {
		writeErr(w, http.StatusBadRequest,
			errors.New("secret name must match [A-Za-z_][A-Za-z0-9_]{0,63} (the grammar a models document can reference)"))
		return
	}
	if s.cfg.SecretStore == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New(s.secretsUnavailableReason()))
		return
	}
	// A stored models document may reference this secret. Deleting it would
	// leave the next boot with an override that cannot resolve its credential
	// (the boot falls back to the deployment document and says so, but the
	// honest move is to refuse the delete while the reference exists).
	if s.cfg.ModelSettingsStore != nil {
		if document, revision, _, found, err := s.cfg.ModelSettingsStore.GetModelSettings(r.Context(), tenantID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		} else if found && documentReferencesSecret(document, name) {
			writeErr(w, http.StatusConflict, fmt.Errorf(
				"secret %q is referenced by the stored models document at revision %d; save a document that does not reference it (or reset to the deployment default) before deleting it",
				name, revision))
			return
		}
	}
	deleted, err := s.cfg.SecretStore.Delete(r.Context(), tenantID, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, secretDeleteResponse{Name: name, Deleted: deleted})
}

// documentReferencesSecret reports whether a models document names the tenant
// secret anywhere. Documents are data: every string shaped like a reference is
// one the resolver may try, so the walk is over the whole document rather than
// a list of known fields.
func documentReferencesSecret(document json.RawMessage, name string) bool {
	ref := "secret:" + name
	var walk func(value any) bool
	walk = func(value any) bool {
		switch typed := value.(type) {
		case string:
			return typed == ref
		case []any:
			for _, item := range typed {
				if walk(item) {
					return true
				}
			}
		case map[string]any:
			for _, item := range typed {
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	var parsed any
	if err := json.Unmarshal(document, &parsed); err != nil {
		// An unreadable document cannot be shown to reference anything; the
		// boot path rejects it on its own terms.
		return false
	}
	return walk(parsed)
}

// --- plugins -------------------------------------------------------------

// settingsPluginsResponse is the GET /v1/settings/plugins body: the settings
// page's plugin inventory. It aggregates the surfaces that already exist
// (GET /v1/plugins, GET /v1/triggers) so the page needs one request, and it
// says explicitly what this surface does not do: plugins load at boot, the
// inventory reports their state, and firing a trigger is the one control.
//
// Firing stays on POST /v1/triggers/{id}/fire: its 200 body already carries the
// FireResult (status, sessionId, seq, deduped, attempts, error), which is the
// whole result the page shows.
type settingsPluginsResponse struct {
	Generation  int64                    `json:"generation"`
	Plugins     []agentext.PluginStatus  `json:"plugins"`
	Hooks       agentext.StatsSummary    `json:"hooks"`
	HookStats   []agentext.HookStat      `json:"hookStats,omitempty"`
	Tools       []string                 `json:"tools"`
	Triggers    []agentext.TriggerStatus `json:"triggers"`
	Lifecycle   string                   `json:"lifecycle"`
	Description string                   `json:"description"`
}

// settingsPlugins implements GET /v1/settings/plugins: the settings page's one
// request for the plugin section. It does not invent plugin management — the
// inventory is the assembled surface as it is, and `lifecycle: "boot"` plus the
// description state the contract the client must render.
func (s *Server) settingsPlugins(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	if s.cfg.Plugins == nil {
		notWired(w, "plugins are not wired")
		return
	}
	snapshot := s.cfg.Plugins.Snapshot()
	body := settingsPluginsResponse{
		Generation: snapshot.Generation,
		Plugins:    snapshot.Plugins,
		Hooks:      snapshot.Hooks,
		Tools:      snapshot.Tools,
		Triggers:   []agentext.TriggerStatus{},
		Lifecycle:  "boot",
		Description: "plugins are loaded at boot: this section reports each plugin's state, tools, hooks and " +
			"triggers, and can fire a registered trigger (POST /v1/triggers/{id}/fire). Plugin code is not configured here.",
	}
	if s.cfg.Hooks != nil {
		body.HookStats = s.cfg.Hooks.Stats()
	}
	if s.cfg.Triggers != nil {
		body.Triggers = s.cfg.Triggers.List()
	}
	if body.Plugins == nil {
		body.Plugins = []agentext.PluginStatus{}
	}
	if body.Tools == nil {
		body.Tools = []string{}
	}
	if body.Triggers == nil {
		body.Triggers = []agentext.TriggerStatus{}
	}
	writeJSON(w, http.StatusOK, body)
}
