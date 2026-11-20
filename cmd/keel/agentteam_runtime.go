// Runtime faces of the agent/team CLI: `keel agent start|stop` and
// `keel team save|start` (change keel-agent-team-v1, tasks P2/P3).
//
// They are thin clients of the same HTTP surface the settings UI uses:
//
//   - start = POST /v1/sessions, then POST /v1/sessions/{id}/input with a
//     selection naming the Agent (agentId) or the team (teamId). The server
//     resolves the binding per attempt — default model, system prompt and tool
//     grants — so the CLI never re-implements the model picker's old
//     client-side authority. The input that queues the run is also the run's
//     identity: its server-minted submissionId is printed as runId (the runtime
//     keys one run row per session, so there is no second id to fetch).
//   - stop = find the live sessions whose newest binding names the Agent (or a
//     team whose captain it is), then POST /v1/sessions/{id}/cancel. No active
//     run and a settled race are clear refusals (exit 1); exit 0 means the
//     server reported at least one real cancellation.
//   - save = decode a YAML or JSON team document (which must carry its id),
//     then create (POST) or update (PUT) it. revision and hash are server-owned:
//     the file may carry the values `keel team show` exported, and they are
//     ignored, never sent.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/httpapi"
)

// defaultStartText is the first user message a bare `keel agent start <id>`
// sends. A run needs one input fact; an empty text would be refused by /input,
// so the CLI sends a neutral kickoff the model can answer instead of inventing
// a task. Pass --text to start with the real instruction.
const defaultStartText = "Start."

// sessionPageLimit is one page of GET /v1/sessions (the AgentAPI cap).
const sessionPageLimit = 200

// maxSessionPages bounds the stop scan: 1000 most recently active sessions.
// A live run keeps its session's updated_at fresh (every event commit and every
// lease renewal touches the session row), so an active run stays inside this
// window; the bound keeps `keel agent stop` a bounded client call rather than a
// full table walk.
const maxSessionPages = 5

// --- wire shapes ---------------------------------------------------------

// runtimeSessionSummary is one row of GET /v1/sessions.
type runtimeSessionSummary struct {
	SessionID string `json:"sessionId"`
	RunStatus string `json:"runStatus"`
	UpdatedAt string `json:"updatedAt"`
}

// runtimeSessionPage is one keyset page of GET /v1/sessions.
type runtimeSessionPage struct {
	Sessions   []runtimeSessionSummary `json:"sessions"`
	NextCursor string                  `json:"nextCursor"`
	HasMore    bool                    `json:"hasMore"`
}

// runtimeEvent is one row of GET /v1/sessions/{id}/events.
type runtimeEvent struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// runtimeEventPage is the events envelope.
type runtimeEventPage struct {
	Events []runtimeEvent `json:"events"`
}

// selectionFact is the execution-binding part of a user.message selection.
type selectionFact struct {
	AgentID string `json:"agentId"`
	TeamID  string `json:"teamId"`
}

// startOutcome is what `agent start` / `team start` reports.
type startOutcome struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId"`
	RequestID string `json:"requestId"`
	Status    string `json:"status,omitempty"`
}

// stopOutcome is one session the stop scan acted on.
type stopOutcome struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
	RunStatus string `json:"runStatus,omitempty"`
	Cancelled bool   `json:"cancelled"`
	Detail    string `json:"detail,omitempty"`
}

// --- agent start ---------------------------------------------------------

func runAgentStart(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent start", stderr)
	server, asJSON := commonFlags(fs)
	text := fs.String("text", defaultStartText, "first user message of the run")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel agent start <id> [--text <message>]")
		return 2
	}
	if strings.TrimSpace(*text) == "" {
		fmt.Fprintln(stderr, "keel agent start: --text must not be empty (a run needs a first input)")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var agent httpapi.AgentView
	if err := client.do(http.MethodGet, "/v1/agents/"+url.PathEscape(positional[0]), "", nil, &agent); err != nil {
		return reportError(stderr, err)
	}
	if agent.Archived {
		fmt.Fprintf(stderr, "keel: agent %s is archived; unarchive or create a live agent before starting it\n", agent.ID)
		return 1
	}
	outcome, err := queueAgentRun(client, agent.ID, *text)
	if err != nil {
		return reportError(stderr, err)
	}
	outcome.Kind = "agent"
	outcome.ID = agent.ID
	outcome.Name = agent.Name
	return printStartOutcome(stdout, stderr, outcome, *asJSON)
}

// --- team start ----------------------------------------------------------

func runTeamStart(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team start", stderr)
	server, asJSON := commonFlags(fs)
	text := fs.String("text", defaultStartText, "first user message of the run")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel team start <id> [--text <message>]")
		return 2
	}
	if strings.TrimSpace(*text) == "" {
		fmt.Fprintln(stderr, "keel team start: --text must not be empty (a run needs a first input)")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var team httpapi.AgentTeamView
	if err := client.do(http.MethodGet, "/v1/teams/"+url.PathEscape(positional[0]), "", nil, &team); err != nil {
		return reportError(stderr, err)
	}
	if team.Archived {
		fmt.Fprintf(stderr, "keel: team %s is archived; it cannot start a run\n", team.ID)
		return 1
	}
	// The status is derived by the server on every read, so "not valid" (and an
	// empty status from a server that cannot derive one) both refuse: a start
	// must never turn an invalid team into a run the server will reject later.
	if team.Status != agentteam.StatusValid {
		fmt.Fprintf(stderr, "keel: team %s is not startable (status=%q); fix it (keel team show %s) before starting it\n",
			team.ID, team.Status, team.ID)
		return 1
	}
	outcome, err := queueTeamRun(client, team.ID, *text)
	if err != nil {
		return reportError(stderr, err)
	}
	outcome.Kind = "team"
	outcome.ID = team.ID
	outcome.Name = team.Name
	return printStartOutcome(stdout, stderr, outcome, *asJSON)
}

// queueAgentRun creates a session and commits one input whose selection names
// the Agent. The server merges the Agent's default model, so the CLI sends no
// model identity: the binding is the server's authority (P6 in RUNTIME-PLAN).
func queueAgentRun(client *cliClient, agentID, text string) (startOutcome, error) {
	return queueRun(client, map[string]string{"agentId": agentID}, text)
}

// queueTeamRun creates a session and commits one input whose selection names
// the team. teamId wins over the captain's id by the selection contract; the
// server resolves the captain, its prompt and its tool grants per attempt.
func queueTeamRun(client *cliClient, teamID, text string) (startOutcome, error) {
	return queueRun(client, map[string]string{"teamId": teamID}, text)
}

// queueRun is the one start path both commands share: POST /v1/sessions, then
// POST /v1/sessions/{id}/input. It returns the session id and the submission id
// the input endpoint minted (printed as runId: the input is what queued the
// run, and the run row itself is keyed by session).
func queueRun(client *cliClient, selection map[string]string, text string) (startOutcome, error) {
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := client.do(http.MethodPost, "/v1/sessions", "", struct{}{}, &created); err != nil {
		return startOutcome{}, err
	}
	if created.SessionID == "" {
		return startOutcome{}, errors.New("server answered a session create without sessionId")
	}
	requestID := "keel-start-" + uuid.NewString()
	var committed struct {
		SubmissionID string `json:"submissionId"`
		RequestID    string `json:"requestId"`
		Status       string `json:"status"`
	}
	body := map[string]any{
		"text":      text,
		"requestId": requestID,
		"selection": selection,
	}
	if err := client.do(http.MethodPost, "/v1/sessions/"+url.PathEscape(created.SessionID)+"/input", "", body, &committed); err != nil {
		// A refused input means no run was queued: archive the empty session the
		// start just created instead of leaving a blank session in the library.
		discardUnqueuedSession(client, created.SessionID, err)
		return startOutcome{}, err
	}
	runID := strings.TrimSpace(committed.SubmissionID)
	if runID == "" {
		// The input endpoint always mints a submission id; losing it means the
		// client cannot name the run it just queued. Fail loudly rather than
		// printing a request id as if it were the run.
		return startOutcome{}, errors.New("server committed the input without a submissionId; cannot name the queued run")
	}
	return startOutcome{
		SessionID: created.SessionID,
		RunID:     runID,
		RequestID: committed.RequestID,
		Status:    committed.Status,
	}, nil
}

// discardUnqueuedSession soft-deletes the session a failed start created. Only a
// definitive HTTP refusal is cleaned up: on a network error or a 5xx the input
// may have been committed, and archiving that session would hide a real run.
// The delete is best-effort; the input refusal is the error the caller sees.
func discardUnqueuedSession(client *cliClient, sessionID string, cause error) {
	var refusal *apiError
	if !errors.As(cause, &refusal) || refusal.Status < 400 || refusal.Status >= 500 {
		return
	}
	_ = client.do(http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID), "", nil, nil)
}

func printStartOutcome(stdout, stderr io.Writer, outcome startOutcome, asJSON bool) int {
	if asJSON {
		if err := printJSON(stdout, outcome); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "started %s %s", outcome.Kind, outcome.ID)
	if outcome.Name != "" {
		fmt.Fprintf(stdout, " (%s)", outcome.Name)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  sessionId: %s\n", outcome.SessionID)
	fmt.Fprintf(stdout, "  runId:     %s\n", outcome.RunID)
	if outcome.RequestID != "" {
		fmt.Fprintf(stdout, "  requestId: %s\n", outcome.RequestID)
	}
	if outcome.Status != "" {
		fmt.Fprintf(stdout, "  status:    %s\n", outcome.Status)
	}
	return 0
}

// --- agent stop ----------------------------------------------------------

func runAgentStop(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent stop", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel agent stop <id>")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var agent httpapi.AgentView
	if err := client.do(http.MethodGet, "/v1/agents/"+url.PathEscape(positional[0]), "", nil, &agent); err != nil {
		return reportError(stderr, err)
	}
	sessions, err := activeSessionsForAgent(client, agent.ID)
	if err != nil {
		return reportError(stderr, err)
	}
	if len(sessions) == 0 {
		fmt.Fprintf(stderr, "keel: no active run for agent %s; nothing to stop\n", agent.ID)
		return 1
	}
	outcomes := make([]stopOutcome, 0, len(sessions))
	failed := false
	for _, sessionID := range sessions {
		outcome, err := cancelSession(client, sessionID)
		if err != nil {
			failed = true
			fmt.Fprintf(stderr, "keel: session %s: %v\n", sessionID, err)
			continue
		}
		outcomes = append(outcomes, outcome)
	}
	if *asJSON {
		if err := printJSON(stdout, outcomes); err != nil {
			return reportError(stderr, err)
		}
	} else {
		for _, outcome := range outcomes {
			printStopOutcome(stdout, outcome)
		}
	}
	if failed {
		return 1
	}
	for _, outcome := range outcomes {
		if outcome.Cancelled {
			return 0
		}
	}
	// Nothing was actually stopped: every matched session was already settled
	// (a race with a clean finish), or its input is queued with no attempt. The
	// command reports that state and exits nonzero — it must never claim a
	// cancellation the server did not perform.
	return 1
}

func printStopOutcome(w io.Writer, outcome stopOutcome) {
	switch {
	case outcome.Cancelled:
		fmt.Fprintf(w, "stopped session %s (runStatus=%s)\n", outcome.SessionID, outcome.RunStatus)
	case outcome.Status == "settled":
		fmt.Fprintf(w, "session %s already settled (runStatus=%s); nothing to cancel\n",
			outcome.SessionID, outcome.RunStatus)
	case outcome.RunStatus == "pending" || outcome.Status == "idle":
		fmt.Fprintf(w, "session %s is queued (runStatus=%s) with no active attempt; the input will run when a worker claims it\n",
			outcome.SessionID, outcome.RunStatus)
	default:
		fmt.Fprintf(w, "session %s: %s (runStatus=%s)\n", outcome.SessionID, outcome.Status, outcome.RunStatus)
	}
}

// activeSessionsForAgent returns the live sessions whose newest execution
// binding names agentID: a standard selection.agentId, or a team selection whose
// captain is agentID. Only non-terminal run rows are scanned; a session with no
// live run cannot be stopped.
func activeSessionsForAgent(client *cliClient, agentID string) ([]string, error) {
	captains := make(map[string]string)
	var found []string
	cursor := ""
	for page := 0; page < maxSessionPages; page++ {
		path := fmt.Sprintf("/v1/sessions?limit=%d", sessionPageLimit)
		if cursor != "" {
			path += "&after=" + url.QueryEscape(cursor)
		}
		var pageData runtimeSessionPage
		if err := client.do(http.MethodGet, path, "", nil, &pageData); err != nil {
			return nil, err
		}
		for _, session := range pageData.Sessions {
			if !liveRunStatus(session.RunStatus) {
				continue
			}
			selection, err := newestSelection(client, session.SessionID)
			if err != nil {
				return nil, err
			}
			if selection == nil {
				continue
			}
			// teamId wins over agentId (the selection contract: a team selection
			// decides the captain). Only an agent-only selection is the agent's
			// own run; a team session belongs to its captain even if the payload
			// also names an agent.
			if teamID := strings.TrimSpace(selection.TeamID); teamID != "" {
				captainID, ok := captains[teamID]
				if !ok {
					team, err := fetchTeam(client, teamID)
					if err != nil {
						// A team deleted while its session still runs is not a
						// reason to abort the whole stop: its captain can no
						// longer be resolved, so it is not this agent's run.
						if isNotFound(err) {
							captains[teamID] = ""
							continue
						}
						return nil, err
					}
					captainID = strings.TrimSpace(team.Captain.AgentID)
					captains[teamID] = captainID
				}
				if captainID != "" && captainID == agentID {
					found = append(found, session.SessionID)
				}
				continue
			}
			if strings.TrimSpace(selection.AgentID) == agentID {
				found = append(found, session.SessionID)
			}
		}
		if !pageData.HasMore || pageData.NextCursor == "" {
			break
		}
		cursor = pageData.NextCursor
	}
	return found, nil
}

// liveRunStatus reports the run states with an attempt that could be stopped.
// "pending" is included because the cancel endpoint answers it truthfully
// (idle: queued, no attempt); the caller then reports that instead of exiting 0.
func liveRunStatus(status string) bool {
	switch status {
	case "running", "pending", "cancelling":
		return true
	default:
		return false
	}
}

// newestSelection reads the newest user.message carrying an execution binding.
// A later model-only input does not erase the binding: the run keeps using the
// last identity the session named until a newer input changes it.
func newestSelection(client *cliClient, sessionID string) (*selectionFact, error) {
	after := int64(0)
	var latest *selectionFact
	for page := 0; page < 50; page++ {
		path := fmt.Sprintf("/v1/sessions/%s/events?limit=1000", url.PathEscape(sessionID))
		if after > 0 {
			path += fmt.Sprintf("&after=%d", after)
		}
		var pageData runtimeEventPage
		if err := client.do(http.MethodGet, path, "", nil, &pageData); err != nil {
			return nil, err
		}
		for _, event := range pageData.Events {
			if event.Kind != "user.message" || len(event.Payload) == 0 {
				continue
			}
			var payload struct {
				Selection *selectionFact `json:"selection"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Selection == nil {
				continue
			}
			if strings.TrimSpace(payload.Selection.AgentID) == "" && strings.TrimSpace(payload.Selection.TeamID) == "" {
				continue
			}
			latest = payload.Selection
		}
		if len(pageData.Events) < 1000 {
			break
		}
		after = pageData.Events[len(pageData.Events)-1].Seq
	}
	return latest, nil
}

func fetchTeam(client *cliClient, teamID string) (httpapi.AgentTeamView, error) {
	var team httpapi.AgentTeamView
	err := client.do(http.MethodGet, "/v1/teams/"+url.PathEscape(teamID), "", nil, &team)
	return team, err
}

// cancelSession calls the existing cancel endpoint and decodes its truthful
// answer. A 501 (no cancel seam / running elsewhere) is rendered by reportError
// at the command boundary; here it is returned as an error.
func cancelSession(client *cliClient, sessionID string) (stopOutcome, error) {
	var result struct {
		Status    string `json:"status"`
		RunStatus string `json:"runStatus"`
		Cancelled bool   `json:"cancelled"`
		Detail    string `json:"detail"`
	}
	if err := client.do(http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/cancel", "", nil, &result); err != nil {
		return stopOutcome{}, err
	}
	return stopOutcome{
		SessionID: sessionID,
		Status:    result.Status,
		RunStatus: result.RunStatus,
		Cancelled: result.Cancelled,
		Detail:    result.Detail,
	}, nil
}

// --- team save -----------------------------------------------------------

func runTeamSave(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team save", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel team save <file>   (YAML or JSON; - reads stdin)")
		return 2
	}
	request, err := loadTeamDocument(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "keel team save: %v\n", err)
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var view httpapi.AgentTeamView
	path := "/v1/teams/" + url.PathEscape(request.ID)
	method := http.MethodPut
	var existing httpapi.AgentTeamView
	err = client.do(http.MethodGet, path, "", nil, &existing)
	switch {
	case err == nil:
		// The id exists: PUT replaces the whole configuration document.
	case isNotFound(err):
		// A save with an unknown id creates it: upsert means "this file is the
		// configuration", not "this file must already exist".
		method = http.MethodPost
		path = "/v1/teams"
	default:
		return reportError(stderr, err)
	}
	if err := client.do(method, path, "", request, &view); err != nil {
		return reportError(stderr, err)
	}
	created := method == http.MethodPost
	if *asJSON {
		if err := printJSON(stdout, view); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	verb := "updated"
	if created {
		verb = "created"
	}
	fmt.Fprintf(stdout, "saved team %s (%s) %s revision=%d hash=%s\n",
		view.ID, view.Name, verb, view.Revision, view.Hash)
	return 0
}

// teamDocumentServerKeys are the fields `keel team show` exports but a save
// must not send: the server derives them (or they are lifecycle operations).
// Dropping them makes `keel team show --json > team.json && edit && keel team
// save team.json` a legal round trip without letting a client pin a revision.
var teamDocumentServerKeys = []string{"tenantId", "status", "revision", "hash", "archived"}

// loadTeamDocument decodes one YAML or JSON team document. The source is
// normalised to JSON first, so the server DTO's json tags are the single
// vocabulary (YAML's default field naming would not match agentId/captain).
func loadTeamDocument(path string) (httpapi.AgentTeamWriteRequest, error) {
	raw, err := readTeamSource(path)
	if err != nil {
		return httpapi.AgentTeamWriteRequest{}, err
	}
	document, err := decodeTeamDocument(raw)
	if err != nil {
		return httpapi.AgentTeamWriteRequest{}, err
	}
	for _, key := range teamDocumentServerKeys {
		delete(document, key)
	}
	normalized, err := json.Marshal(document)
	if err != nil {
		return httpapi.AgentTeamWriteRequest{}, fmt.Errorf("encode team document: %w", err)
	}
	var request httpapi.AgentTeamWriteRequest
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return httpapi.AgentTeamWriteRequest{}, fmt.Errorf("decode team document: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return httpapi.AgentTeamWriteRequest{}, errors.New("decode team document: trailing data after the object")
	}
	// save is an upsert, so the document must carry the identity it updates: an
	// id-less save would create a new team on every invocation, contradicting
	// the command's idempotence. `keel team create` is the name-only path.
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" {
		return httpapi.AgentTeamWriteRequest{}, errors.New("team document must carry id: save is an upsert (use keel team create for a name-only team)")
	}
	return request, nil
}

// maxTeamDocumentBytes bounds one team document. A configuration file is small;
// the bound keeps a mistaken path (a device, a giant export) from being read
// into memory.
const maxTeamDocumentBytes = 4 << 20

func readTeamSource(path string) ([]byte, error) {
	if path == "-" {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxTeamDocumentBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(raw) > maxTeamDocumentBytes {
			return nil, fmt.Errorf("stdin exceeds %d bytes", maxTeamDocumentBytes)
		}
		return raw, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxTeamDocumentBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxTeamDocumentBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return raw, nil
}

// decodeTeamDocument accepts a JSON object or a YAML mapping and returns the
// generic document. Anything else is refused before a request is sent.
func decodeTeamDocument(raw []byte) (map[string]any, error) {
	var document map[string]any
	if json.Valid(raw) {
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, fmt.Errorf("decode JSON: %w", err)
		}
	} else if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	if document == nil {
		return nil, errors.New("the team document must be a mapping (id, name, captain, ...)")
	}
	return document, nil
}

// isNotFound reports a server 404, the upsert's "create it" signal.
func isNotFound(err error) bool {
	var refusal *apiError
	return errors.As(err, &refusal) && refusal.Status == http.StatusNotFound
}
