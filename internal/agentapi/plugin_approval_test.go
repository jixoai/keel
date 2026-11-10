package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
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
	tenant := uuid.New()
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
			ApprovalRequired bool              `json:"approvalRequired"`
			Decision         agentext.Decision `json:"decision"`
			AnswerURL        string            `json:"answerUrl"`
		}
		if code := harness.write(t, write.method, write.path, tenant, write.body, &asked); code != http.StatusAccepted {
			t.Fatalf("%s = %d, want 202", write.path, code)
		}
		if !asked.ApprovalRequired || asked.Decision.DecisionID == "" {
			t.Fatalf("%s: the 202 must carry the question: %+v", write.path, asked)
		}
		if asked.Decision.Status != agentext.DecisionOpen || asked.Decision.Kind != "plugin."+actionOfPath(write.path) {
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
			Decision agentext.Decision `json:"decision"`
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
	tenant := uuid.New()
	var first, second struct {
		Decision agentext.Decision `json:"decision"`
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
	tenant := uuid.New()
	var answered agentext.Decision
	code, body := harness.approveWriteAs(t, http.MethodPost, "/v1/plugins/install", tenant,
		map[string]any{"path": "/tmp/panel"}, pluginApprovalDeny,
		func(decision agentext.Decision) { answered = decision })
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
	if answered.Status != agentext.DecisionAnswered || answered.Answer == nil || answered.Answer.OptionID != pluginApprovalDeny {
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
	if row.TenantID == nil || *row.TenantID != tenant || row.Actor == "" {
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
	tenant := uuid.New()
	code, body := harness.approveWrite(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil, pluginApprovalAllow)
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
}

// TestPluginApprovalPendingRetryIsRefusedNotExecuted: a question that is still
// open answers 409 with the question, and never acts.
func TestPluginApprovalPendingRetryIsRefusedNotExecuted(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := uuid.New()
	var asked struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("ask = %d", code)
	}
	var pending struct {
		ApprovalRequired bool              `json:"approvalRequired"`
		Decision         agentext.Decision `json:"decision"`
	}
	code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant,
		map[string]any{"decisionId": asked.Decision.DecisionID}, &pending)
	if code != http.StatusConflict || !pending.ApprovalRequired || pending.Decision.Status != agentext.DecisionOpen {
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
	tenant := uuid.New()
	var asked struct {
		Decision agentext.Decision `json:"decision"`
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
	tenant := uuid.New()
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
	tenant := uuid.New()
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
		Decision agentext.Decision `json:"decision"`
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
	memory := agentext.NewMemoryDecisionStore()
	harness := newPluginAPITest(t, manager, func(cfg *Config) {
		cfg.Decisions = agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: memory, Logger: testLogger()})
	})
	tenant := uuid.New()
	var asked struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/enable", tenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("ask = %d", code)
	}
	// "Restart": a fresh broker over the same store, no timers, no waiters.
	restarted := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: memory, Logger: testLogger()})
	defer restarted.Close()
	found, ok := restarted.Get(context.Background(), tenant, pluginApprovalSession(tenant), mustUUID(t, asked.Decision.DecisionID))
	if !ok || found.Status != agentext.DecisionOpen {
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
