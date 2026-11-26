package harnessapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/store"
)

// The durable approval of plugin lifecycle writes (change keel-plugin-core, P2).
// The spec's rule is absolute — "安装/启用/升级 MUST 经 durable 审批（拒绝则不
// 生效）" — so these tests hold the four sides of it: asking does not act, a
// refusal does not act (and is audited as `refused`), a grant acts, and a
// deployment that cannot ask does not act either.

// TestPluginApprovalAskingDoesNotAct: without approval evidence the write answers
// 202 with the durable question, and the manager is never called.
func TestPluginApprovalAskingDoesNotAct(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	writes := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{http.MethodPost, "/v1/plugins/install", map[string]any{"path": "/tmp/panel"}},
		{http.MethodPost, "/v1/plugins/vendor.panel/enable", nil},
		{http.MethodPost, "/v1/plugins/vendor.panel/update", map[string]any{"path": "/tmp/panel-2"}},
	}
	for _, write := range writes {
		var asked struct {
			ApprovalRequired bool                `json:"approvalRequired"`
			Decision         harnessext.Decision `json:"decision"`
			AnswerURL        string              `json:"answerUrl"`
		}
		if code := harness.write(t, write.method, write.path, tenant, write.body, &asked); code != http.StatusAccepted {
			t.Fatalf("%s = %d, want 202", write.path, code)
		}
		if !asked.ApprovalRequired || asked.Decision.DecisionID == "" {
			t.Fatalf("%s: the 202 must carry the question: %+v", write.path, asked)
		}
		if asked.Decision.Status != harnessext.DecisionOpen || asked.Decision.Kind != "plugin."+actionOfPath(write.path) {
			t.Fatalf("%s: decision = %+v", write.path, asked.Decision)
		}
		if len(asked.Decision.Options) != 2 || asked.Decision.Options[0].ID != pluginApprovalAllow || asked.Decision.Options[1].ID != pluginApprovalDeny {
			t.Fatalf("%s: options = %+v", write.path, asked.Decision.Options)
		}
		target := "/tmp/panel"
		if !strings.HasSuffix(write.path, "/install") {
			target = "vendor.panel"
		}
		if !strings.Contains(asked.Decision.Prompt, target) {
			t.Fatalf("%s: the question must name what it approves: %q", write.path, asked.Decision.Prompt)
		}
		if asked.AnswerURL != "/v1/plugin-approvals/"+asked.Decision.DecisionID {
			t.Fatalf("%s: answerUrl = %q", write.path, asked.AnswerURL)
		}
		// The question is durable and readable through its own route.
		var read struct {
			Decision harnessext.Decision `json:"decision"`
		}
		if code := doJSON(t, harness.server.Client(), http.MethodGet,
			harness.server.URL+asked.AnswerURL, tenant, nil, &read); code != 200 || read.Decision.DecisionID != asked.Decision.DecisionID {
			t.Fatalf("%s: read approval = %d %+v", write.path, code, read)
		}
	}
	if len(manager.calls) != 0 {
		t.Fatalf("asking must not act: calls = %+v", manager.calls)
	}
	if rows := harness.audit.list(); len(rows) != 0 {
		t.Fatalf("an open question is not an audit event: %+v", rows)
	}
}

// TestPluginApprovalQuestionIsIdempotent: repeating the write without a
// decisionId re-addresses the same question instead of asking again.
func TestPluginApprovalQuestionIsIdempotent(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	var first, second struct {
		Decision harnessext.Decision `json:"decision"`
	}
	body := map[string]any{"path": "/tmp/panel", "requestId": "settings-page-1"}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", tenant, body, &first); code != http.StatusAccepted {
		t.Fatalf("first ask = %d", code)
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", tenant, body, &second); code != http.StatusAccepted {
		t.Fatalf("second ask = %d", code)
	}
	if first.Decision.DecisionID != second.Decision.DecisionID {
		t.Fatalf("the same request must address the same question: %s vs %s",
			first.Decision.DecisionID, second.Decision.DecisionID)
	}
}

// TestPluginApprovalDeniedIsRefusedAndAudited: a refusal never reaches the
// manager, and it lands in the audit trail as `refused` (not `failed`).
func TestPluginApprovalDeniedIsRefusedAndAudited(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	var answered harnessext.Decision
	code, body := harness.approveWriteAs(t, http.MethodPost, "/v1/plugins/install", tenant,
		map[string]any{"path": "/tmp/panel"}, pluginApprovalDeny,
		func(decision harnessext.Decision) { answered = decision })
	if code != http.StatusConflict {
		t.Fatalf("denied install = %d %+v", code, body)
	}
	if refused, _ := body["refused"].(bool); !refused {
		t.Fatalf("the refusal must say so: %+v", body)
	}
	if hint, _ := body["hint"].(string); !strings.Contains(hint, "requestId") {
		t.Fatalf("a terminal refusal must tell the client how to ask again: %+v", body)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("a refused write must not act: %+v", manager.calls)
	}
	if answered.Status != harnessext.DecisionAnswered || answered.Answer == nil || answered.Answer.OptionID != pluginApprovalDeny {
		t.Fatalf("the decision must record the denial: %+v", answered)
	}
	rows := harness.audit.list()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v", rows)
	}
	row := rows[0]
	if row.Result != store.PluginResultRefused || row.Action != store.PluginActionInstall {
		t.Fatalf("audit row = %+v", row)
	}
	if row.PluginID != "panel" {
		t.Fatalf("the refusal must name the package: %+v", row)
	}
	if row.TenantID != tenant || row.Actor == "" {
		t.Fatalf("the refusal must carry its attribution: %+v", row)
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail = %s (%v)", row.Detail, err)
	}
	if detail["decisionId"] != answered.DecisionID || detail["optionId"] != pluginApprovalDeny {
		t.Fatalf("detail = %+v", detail)
	}
}

// TestPluginApprovalGrantedActsOnce: the granted write runs the manager action
// exactly once, with the audit attribution of the caller.
func TestPluginApprovalGrantedActsOnce(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	var answered harnessext.Decision
	code, body := harness.approveWriteAs(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil,
		pluginApprovalAllow, func(decision harnessext.Decision) { answered = decision })
	if code != http.StatusOK {
		t.Fatalf("granted enable = %d %+v", code, body)
	}
	if plugin, ok := body["plugin"].(map[string]any); !ok || plugin["id"] != "vendor.panel" {
		t.Fatalf("the granted write answers the view: %+v", body)
	}
	if manager.count("enable:vendor.panel") != 1 {
		t.Fatalf("calls = %+v", manager.calls)
	}
	if rows := harness.audit.list(); len(rows) != 0 {
		t.Fatalf("a granted write is audited by the manager, not by the gate: %+v", rows)
	}
	// The answer names the authenticated approver, and the lifecycle row is
	// attributed to that approver rather than an anonymous caller.
	if answered.Answer == nil || answered.Answer.AnsweredBy != pluginTestAccount.String() ||
		answered.Answer.AnsweredByKind != auth.PrincipalHuman {
		t.Fatalf("answered decision = %+v", answered.Answer)
	}
	if got := manager.lastAudit(t); got.Actor != "approver:"+pluginTestAccount.String() {
		t.Fatalf("lifecycle audit actor = %q", got.Actor)
	}
}

// TestPluginApprovalBindsInstallToContent: the expectedHash is part of the
// question's identity. A decision opened for hash A cannot authorize hash B.
func TestPluginApprovalBindsInstallToContent(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	var first, second struct {
		Decision harnessext.Decision `json:"decision"`
	}
	hashA, hashB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", pluginTestTenant,
		map[string]any{"path": "/tmp/panel", "expectedHash": hashA}, &first); code != http.StatusAccepted {
		t.Fatalf("ask A = %d", code)
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", pluginTestTenant,
		map[string]any{"path": "/tmp/panel", "expectedHash": hashB}, &second); code != http.StatusAccepted {
		t.Fatalf("ask B = %d", code)
	}
	if first.Decision.DecisionID == second.Decision.DecisionID {
		t.Fatal("two content hashes share one approval question")
	}
	// Answering the A question and replaying with B's content is refused before
	// the manager runs.
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: pluginTestTenant, SessionID: pluginApprovalSession(pluginTestTenant),
		DecisionID: uuid.MustParse(first.Decision.DecisionID), OptionID: pluginApprovalAllow,
	}); err != nil {
		t.Fatal(err)
	}
	code := harness.write(t, http.MethodPost, "/v1/plugins/install", pluginTestTenant,
		map[string]any{"path": "/tmp/panel", "expectedHash": hashB, "decisionId": first.Decision.DecisionID}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("reused decision for another hash = %d, want 400", code)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("the mismatched decision reached the manager: %v", manager.calls)
	}
}

// TestSessionDecisionPluginApprovalUsesTheSameGate: a creation approval opened
// inside a run (session decisions route) is still a plugin lifecycle approval —
// a bot cannot answer it and the lifecycle key is required.
func TestSessionDecisionPluginApprovalUsesTheSameGate(t *testing.T) {
	manager := &fakePluginManager{}
	bot := auth.Principal{
		Kind: auth.PrincipalBot, AccountID: pluginTestAccount, BotID: "bot-1", TenantID: pluginTestTenant,
	}
	harness := newPluginAPITestAs(t, manager, bot)
	sessionID := uuid.New()
	decision, _, err := harness.broker.Open(context.Background(), harnessext.OpenDecisionRequest{
		TenantID: pluginTestTenant, SessionID: sessionID, RequestID: "creation.install:demo.panel",
		Kind: "plugin.install",
		Options: []harnessext.DecisionOption{
			{ID: pluginApprovalAllow}, {ID: pluginApprovalDeny},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	code := harness.write(t, http.MethodPost,
		"/v1/sessions/"+sessionID.String()+"/decisions/"+decision.DecisionID,
		pluginTestTenant, map[string]any{"optionId": pluginApprovalAllow}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("bot session answer = %d, want 403", code)
	}
	pending, found := harness.broker.Get(context.Background(), pluginTestTenant, sessionID, uuid.MustParse(decision.DecisionID))
	if !found || pending.Status != harnessext.DecisionOpen {
		t.Fatalf("bot session answer changed the decision: %+v", pending)
	}
}

// TestPluginApprovalBotCannotAnswer: a bot key may open a plugin question but
// never answer it (no self-approval).
func TestPluginApprovalBotCannotAnswer(t *testing.T) {
	manager := &fakePluginManager{}
	bot := auth.Principal{
		Kind: auth.PrincipalBot, AccountID: pluginTestAccount, BotID: "bot-1", TenantID: pluginTestTenant,
	}
	harness := newPluginAPITestAs(t, manager, bot)
	var asked struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", pluginTestTenant,
		map[string]any{"path": "/tmp/panel"}, &asked); code != http.StatusAccepted {
		t.Fatalf("bot opened the question = %d", code)
	}
	code := harness.write(t, http.MethodPost, "/v1/plugin-approvals/"+asked.Decision.DecisionID,
		pluginTestTenant, map[string]any{"optionId": pluginApprovalAllow}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("bot answer = %d, want 403", code)
	}
	pending, found := harness.broker.Get(context.Background(), pluginTestTenant,
		pluginApprovalSession(pluginTestTenant), uuid.MustParse(asked.Decision.DecisionID))
	if !found || pending.Status != harnessext.DecisionOpen {
		t.Fatalf("a bot answer changed the decision: %+v", pending)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("the bot answer reached the manager: %v", manager.calls)
	}
}

// TestPluginApprovalRequiresLifecycleKey: answering an install approval needs
// plugin.lifecycle.install even when the caller can invoke plugin tools.
func TestPluginApprovalRequiresLifecycleKey(t *testing.T) {
	manager := &fakePluginManager{}
	denied := permitAllAccess()
	denied.decisions = map[string]access.Decision{
		access.KeyPluginLifecycleInstall: {Allow: false, Key: access.KeyPluginLifecycleInstall,
			Reason: access.ReasonMissingKey, Detail: "the account does not hold the lifecycle key"},
	}
	harness := newPluginAPITest(t, manager, func(cfg *Config) { cfg.Access = denied })
	var asked struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", pluginTestTenant,
		map[string]any{"path": "/tmp/panel"}, &asked); code != http.StatusAccepted {
		t.Fatalf("install question = %d", code)
	}
	code := harness.write(t, http.MethodPost, "/v1/plugin-approvals/"+asked.Decision.DecisionID,
		pluginTestTenant, map[string]any{"optionId": pluginApprovalAllow}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("answer without plugin.lifecycle.install = %d, want 403", code)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("the refused answer reached the manager: %v", manager.calls)
	}
}

// TestPluginApprovalPendingRetryIsRefusedNotExecuted: a question that is still
// open answers 409 with the question, and never acts.
func TestPluginApprovalPendingRetryIsRefusedNotExecuted(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	var asked struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("ask = %d", code)
	}
	var pending struct {
		ApprovalRequired bool                `json:"approvalRequired"`
		Decision         harnessext.Decision `json:"decision"`
	}
	code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant,
		map[string]any{"decisionId": asked.Decision.DecisionID}, &pending)
	if code != http.StatusConflict || !pending.ApprovalRequired || pending.Decision.Status != harnessext.DecisionOpen {
		t.Fatalf("pending retry = %d %+v", code, pending)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("a pending question must not act: %+v", manager.calls)
	}
}

// TestPluginApprovalDecisionIsBoundToItsRequest: a decision cannot authorize a
// different action or a different plugin/package.
func TestPluginApprovalDecisionIsBoundToItsRequest(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	var asked struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("ask = %d", code)
	}
	if code := doJSON(t, harness.server.Client(), http.MethodPost,
		harness.server.URL+"/v1/plugin-approvals/"+asked.Decision.DecisionID, tenant,
		map[string]any{"optionId": pluginApprovalAllow}, nil); code != 200 {
		t.Fatalf("answer = %d", code)
	}
	// Same decision id, different target.
	var body map[string]any
	if code := harness.write(t, http.MethodPost, "/v1/plugins/other.plugin/enable", tenant,
		map[string]any{"decisionId": asked.Decision.DecisionID}, &body); code != http.StatusBadRequest {
		t.Fatalf("replayed decision on another plugin = %d %+v", code, body)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("a mismatched decision must not act: %+v", manager.calls)
	}
}

// TestPluginApprovalFailsClosedWithoutDecisions: a host without the decision
// primitive cannot ask, so it must not act (the spec's rule, not a suggestion).
func TestPluginApprovalFailsClosedWithoutDecisions(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager, func(cfg *Config) { cfg.Decisions = nil; cfg.PluginAuditWriter = nil })
	tenant := pluginTestTenant
	for _, path := range []string{"/v1/plugins/install", "/v1/plugins/vendor.panel/enable", "/v1/plugins/vendor.panel/update"} {
		body := map[string]any{}
		if strings.HasSuffix(path, "/install") {
			body["path"] = "/tmp/panel"
		}
		var out map[string]any
		if code := harness.write(t, http.MethodPost, path, tenant, body, &out); code != http.StatusServiceUnavailable {
			t.Fatalf("%s without decisions = %d %+v", path, code, out)
		}
		if !strings.Contains(out["error"].(string), "approval") {
			t.Fatalf("%s must name the missing primitive: %+v", path, out)
		}
	}
	// Disable and uninstall are not approval-gated: revoking is always allowed.
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/disable", tenant, nil, nil); code != http.StatusOK {
		t.Fatalf("disable without decisions = %d", code)
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/uninstall", tenant, nil, nil); code != http.StatusOK {
		t.Fatalf("uninstall without decisions = %d", code)
	}
	if manager.count("enable:vendor.panel") != 0 {
		t.Fatalf("calls = %+v", manager.calls)
	}
}

// TestPluginApprovalBounds: the window and the target are bounded inputs.
func TestPluginApprovalBounds(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	var body map[string]any
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant,
		map[string]any{"approvalTimeoutMs": 10}, &body); code != http.StatusBadRequest {
		t.Fatalf("tiny window = %d %+v", code, body)
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant,
		map[string]any{"approvalTimeoutMs": 7 * 24 * 60 * 60 * 1000}, &body); code != http.StatusBadRequest {
		t.Fatalf("huge window = %d %+v", code, body)
	}
	// The window travels to the question.
	var asked struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant,
		map[string]any{"approvalTimeoutMs": 60_000}, &asked); code != http.StatusAccepted {
		t.Fatalf("valid window = %d", code)
	}
	if remaining := time.Until(asked.Decision.Deadline); remaining > 61*time.Second || remaining < 30*time.Second {
		t.Fatalf("the deadline must follow the request: %s", remaining)
	}
	// An install without a path is the manager's own field error, asked of nobody.
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", tenant, map[string]any{"path": ""}, &body); code != http.StatusBadRequest {
		t.Fatalf("empty path = %d %+v", code, body)
	}
	if body["fields"] == nil {
		t.Fatalf("the field error must survive the gate: %+v", body)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("calls = %+v", manager.calls)
	}
}

// TestPluginApprovalQuestionSurvivesRestart pins the durability claim: the
// question is a row, not a waiter, so a broker built over the same store finds
// it again after the process that asked it is gone.
func TestPluginApprovalQuestionSurvivesRestart(t *testing.T) {
	manager := &fakePluginManager{}
	memory := harnessext.NewMemoryDecisionStore()
	harness := newPluginAPITest(t, manager, func(cfg *Config) {
		cfg.Decisions = harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Store: memory, Logger: testLogger()})
	})
	tenant := pluginTestTenant
	var asked struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("ask = %d", code)
	}
	// "Restart": a fresh broker over the same store, no timers, no waiters.
	restarted := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Store: memory, Logger: testLogger()})
	defer restarted.Close()
	found, ok := restarted.Get(context.Background(), tenant, pluginApprovalSession(tenant), mustUUID(t, asked.Decision.DecisionID))
	if !ok || found.Status != harnessext.DecisionOpen {
		t.Fatalf("the question must survive the restart: %+v found=%v", found, ok)
	}
	if found.Kind != "plugin.enable" {
		t.Fatalf("kind = %q", found.Kind)
	}
}

func actionOfPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/install"):
		return "install"
	case strings.HasSuffix(path, "/enable"):
		return "enable"
	case strings.HasSuffix(path, "/update"):
		return "update"
	}
	return "?"
}

func mustUUID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("uuid %q: %v", raw, err)
	}
	return parsed
}

// TestPluginInstallRejectsAForcedDecisionKind is the P0 control for the API
// path: the approval decision id is derived from the ask (action + target), so a
// client with session.write could open a generic decision under that requestId
// at the derived approval session and answer it itself. The gate must require
// the row to be the plugin.install question it actually asked.
func TestPluginInstallRejectsAForcedDecisionKind(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	path := "/tmp/keel-forged-install-package"
	ask := installApprovalAsk(path, "")
	requestID, err := pluginApprovalRequestID(ask, pluginApprovalRequest{RequestID: "forge-1"})
	if err != nil {
		t.Fatal(err)
	}
	session := pluginApprovalSession(tenant)
	decision, _, err := harness.broker.Open(context.Background(), harnessext.OpenDecisionRequest{
		TenantID:  tenant,
		SessionID: session,
		RequestID: requestID,
		Kind:      "approval", // not plugin.install
		Prompt:    "An unrelated question",
		Options:   []harnessext.DecisionOption{{ID: pluginApprovalAllow, Label: "Approve"}, {ID: pluginApprovalDeny, Label: "Deny"}},
	})
	if err != nil {
		t.Fatalf("open the forged decision: %v", err)
	}
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: uuid.MustParse(decision.DecisionID), OptionID: pluginApprovalAllow,
	}); err != nil {
		t.Fatalf("answer the forged decision: %v", err)
	}
	code := harness.write(t, http.MethodPost, "/v1/plugins/install", tenant, map[string]any{
		"path": path, "requestId": "forge-1", "decisionId": decision.DecisionID,
	}, nil)
	if code == http.StatusOK || code == http.StatusCreated {
		t.Fatalf("a forged decision kind authorized the install (%d)", code)
	}
	for _, call := range manager.calls {
		if strings.HasPrefix(call, "install:") {
			t.Fatalf("the manager installed the package: %v", manager.calls)
		}
	}
}

// TestPluginApprovalShowsAttachmentClaims is the install-approval snapshot for
// change keel-plugin-attachment: the question names the attachment preview
// contribution and its MIME narrowing, so the approver sees the surface being
// adopted before saying allow.
func TestPluginApprovalShowsAttachmentClaims(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := pluginTestTenant
	dir := t.TempDir()
	files := map[string]string{
		"plugin.yaml": `id: vendor.preview
version: 1.0.0
kind: webview
description: PDF preview
engines:
  keel: ">=0.1.0 <1.0.0"
provides:
  ui:
    entry: ui/index.html
contributes:
  - { slot: keel.ui.message-part, form: chunk, entry: ui/panel.js }
extensibility:
  doc: EXTENSIBILITY.md
  schema: extensibility.json
`,
		"EXTENSIBILITY.md": "# preview\n",
		"extensibility.json": `{
  "schema": "keel.plugin-extensibility/1",
  "plugin": "vendor.preview",
  "version": "1.0.0",
  "summary": "PDF preview",
  "slots": [],
  "contributes": [
    {"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js",
     "part": "attachment", "mime": ["application/pdf", "image/*"]}
  ],
  "configuration": [],
  "hooks": [],
  "providers": [],
  "examples": [],
  "limits": []
}`,
		"ui/index.html": "<html>preview</html>",
		"ui/panel.js":   "export function render() {}",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var asked struct {
		ApprovalRequired bool                `json:"approvalRequired"`
		Decision         harnessext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/install", tenant,
		map[string]any{"path": dir}, &asked); code != http.StatusAccepted {
		t.Fatalf("install ask = %d (%+v)", code, asked)
	}
	for _, want := range []string{"keel.ui.message-part", "attachment preview contribution", "application/pdf", "image/*"} {
		if !strings.Contains(asked.Decision.Prompt, want) {
			t.Fatalf("approval prompt lacks %q: %q", want, asked.Decision.Prompt)
		}
	}
	if len(manager.calls) != 0 {
		t.Fatalf("asking must not install: %+v", manager.calls)
	}
}
