// `keel agent ...` and `keel team ...`: the CLI管理面 of the agent registry and
// agent teams (change keel-agent-team-v1, P2/P3).
//
// Both surfaces are thin HTTP clients of a running `keel serve`, exactly like
// `keel tui` is a client of the AgentAPI: one write path, one validation path,
// one tenant boundary. Nothing here opens the database or re-implements a rule —
// the server answers 400 with field paths, 404 for a missing id, 409 for a
// reference conflict and 403 for a captain's disabled team CLI, and this file
// renders them.
//
// Authentication: the server passphrase arrives through KEEL_SERVE_PASSWORD and
// is exchanged for the signed session cookie at POST /v1/auth/login. It is never
// accepted on the command line (a flag shows up in `ps`) and never printed; the
// only credential-shaped output is the server's own error text, which is
// redacted on the server side.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/httpapi"
)

// actorAgentHeader is the header a captain's member mutation carries, so the
// server can check the captain's effective team CLI. It is the same constant the
// HTTP surface reads.
const actorAgentHeader = "X-Keel-Actor-Agent"

// --- dispatch ------------------------------------------------------------

func runAgentCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		agentUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runAgentList(args[1:], stdout, stderr)
	case "show":
		return runAgentShow(args[1:], stdout, stderr)
	case "create":
		return runAgentCreate(args[1:], stdout, stderr)
	case "update":
		return runAgentUpdate(args[1:], stdout, stderr)
	case "delete":
		return runAgentDelete(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		agentUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel agent: unknown command %q\n\n", args[0])
		agentUsage(stderr)
		return 2
	}
}

func runTeamCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		teamUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runTeamList(args[1:], stdout, stderr)
	case "show":
		return runTeamShow(args[1:], stdout, stderr)
	case "create":
		return runTeamCreate(args[1:], stdout, stderr)
	case "update":
		return runTeamUpdate(args[1:], stdout, stderr)
	case "delete":
		return runTeamDelete(args[1:], stdout, stderr)
	case "member":
		return runTeamMember(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		teamUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel team: unknown command %q\n\n", args[0])
		teamUsage(stderr)
		return 2
	}
}

func agentUsage(w io.Writer) {
	fmt.Fprint(w, `keel agent — manage the Agent library over a running keel serve

Usage:
  keel agent list [--json]
  keel agent show <id>
  keel agent create --name <name> [flags]
  keel agent update <id> [flags]
  keel agent delete <id>

Flags (create/update):
  --id <id>                 stable id (create only; minted when omitted)
  --name <name>             display name (required on create)
  --description <text>
  --provider <providerId>   default model provider (with --model)
  --model <modelId>         default model (with --provider)
  --tool <id[,id...]>       tool grants; replaces the whole list on update
  --prompt <text>           inline system prompt
  --prompt-file <ref>       system prompt file reference (e.g. AGENTS.md)
  --skills <a,b>
  --team-cli                enable the captain's team CLI for this agent
  --team-cli-commands <a,b> allowlisted team subcommands (member.add, ...)

Common flags:
  --server <addr>           keel serve address (default $KEEL_SERVE_ADDR or 127.0.0.1:13750)
  --json                    print raw JSON instead of a table

Authentication: KEEL_SERVE_PASSWORD is exchanged for the session cookie
(POST /v1/auth/login); it is never taken from a flag and never printed.
`)
}

func teamUsage(w io.Writer) {
	fmt.Fprint(w, `keel team — manage agent teams over a running keel serve

Usage:
  keel team list [--json]
  keel team show <id>
  keel team create --captain <agentId> --name <name> [flags]
  keel team update <id> [flags]
  keel team delete <id>
  keel team member add <teamId> <agentId> [--label <l>] [--description <d>] [--as <captainAgentId>]
  keel team member remove <teamId> <agentId> [--as <captainAgentId>]

Flags (create/update):
  --id <id>                 stable id (create only; minted when omitted)
  --name <name>             display name (required on create)
  --description <text>
  --captain <agentId>       the single mandatory captain (required on create)
  --member <agentId[:label]>  repeatable member seat
  --team-cli                enable the team's team CLI switch
  --team-cli-commands <a,b> allowlisted team subcommands
  --default-tool <id[,id...]>  tool ids granted to every member

Member flags:
  --as <agentId>            act as this agent (the captain): the server checks
                            the captain's effective team CLI and answers 403
                            team_cli_disabled / team_cli_not_allowed when off.
                            Omitted means the operator face (settings UI/local CLI).

Common flags:
  --server <addr>           keel serve address (default $KEEL_SERVE_ADDR or 127.0.0.1:13750)
  --json                    print raw JSON instead of a table

Authentication: KEEL_SERVE_PASSWORD is exchanged for the session cookie
(POST /v1/auth/login); it is never taken from a flag and never printed.
`)
}

// --- client --------------------------------------------------------------

// cliClient is one authenticated session against `keel serve`.
type cliClient struct {
	base  string
	http  *http.Client
	debug bool
}

// apiError is a refusal from the server, carrying the machine code and the
// field paths so the CLI can print a fixable message. Server names the address
// the request went to: a 404 from the wrong port must be visible in the error,
// not inferred from a flag the user thought they had exported.
type apiError struct {
	Status     int
	Code       string
	Message    string
	Server     string
	Fields     []agentteam.FieldError
	References []map[string]any
}

func (e *apiError) Error() string {
	where := ""
	if e.Server != "" {
		where = " at " + e.Server
	}
	switch {
	case e.Code != "":
		return fmt.Sprintf("server refused (%d %s)%s: %s", e.Status, e.Code, where, e.Message)
	case e.Message != "":
		return fmt.Sprintf("server refused (%d)%s: %s", e.Status, where, e.Message)
	default:
		return fmt.Sprintf("server refused (%d)%s", e.Status, where)
	}
}

// newCLIClient resolves the server address and, when a passphrase is
// configured, logs in so the cookie jar carries the session.
func newCLIClient(server string) (*cliClient, error) {
	base, err := normalizeServerURL(server)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &cliClient{base: base, http: &http.Client{Jar: jar}}
	if err := client.login(os.Getenv("KEEL_SERVE_PASSWORD")); err != nil {
		return nil, err
	}
	return client, nil
}

// normalizeServerURL accepts "127.0.0.1:13750", "http://127.0.0.1:13750" and a
// trailing slash, and returns a base without a trailing slash.
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no server address: set KEEL_SERVE_ADDR or pass --server")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("server address %q: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("server address %q has no host", raw)
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host+parsed.Path, "/"), nil
}

// login exchanges the passphrase for the session cookie. An empty passphrase is
// a no-op: a loopback server with authentication disabled answers without one,
// and a server WITH authentication answers 401 on the real request (which names
// the missing setting), so no error is invented here.
func (c *cliClient) login(password string) error {
	if password == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"password": password})
	request, err := http.NewRequest(http.MethodPost, c.base+"/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.base, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("login to %s failed: %w", c.base, readAPIError(response))
	}
	return nil
}

// do performs one request and decodes a JSON answer into out (which may be nil).
func (c *cliClient) do(method, path, actor string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	if actor != "" {
		request.Header.Set(actorAgentHeader, actor)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.base, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		refusal := readAPIError(response)
		if typed, ok := refusal.(*apiError); ok {
			typed.Server = c.base
		}
		return refusal
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s answer: %w", method, path, err)
	}
	return nil
}

// readAPIError turns the server's error envelope into an apiError.
func readAPIError(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	out := &apiError{Status: response.StatusCode}
	var envelope struct {
		Error      string                 `json:"error"`
		Code       string                 `json:"code"`
		Fields     []agentteam.FieldError `json:"fields"`
		References []map[string]any       `json:"references"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		out.Message = strings.TrimSpace(envelope.Error)
		out.Code = envelope.Code
		out.Fields = envelope.Fields
		out.References = envelope.References
	}
	if out.Message == "" {
		out.Message = strings.TrimSpace(string(raw))
	}
	if out.Message == "" {
		out.Message = http.StatusText(response.StatusCode)
	}
	return out
}

// reportError renders a refusal and returns the process exit code: usage errors
// were already handled (2), every server/runtime failure is 1.
func reportError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "keel: %v\n", err)
	var refusal *apiError
	if errors.As(err, &refusal) {
		for _, field := range refusal.Fields {
			if field.Field == "" {
				fmt.Fprintf(stderr, "  - %s: %s\n", field.Code, field.Message)
				continue
			}
			fmt.Fprintf(stderr, "  - %s (%s): %s\n", field.Field, field.Code, field.Message)
		}
		for _, reference := range refusal.References {
			id, _ := reference["id"].(string)
			name, _ := reference["name"].(string)
			fmt.Fprintf(stderr, "  referencing team: %s %s\n", id, name)
		}
	}
	return 1
}

// --- flag plumbing -------------------------------------------------------

// commonFlags registers the flags every subcommand accepts.
func commonFlags(fs *flag.FlagSet) (server *string, asJSON *bool) {
	server = fs.String("server", envOr("KEEL_SERVE_ADDR", defaultServeAddr), "keel serve address")
	asJSON = fs.Bool("json", false, "print raw JSON")
	return server, asJSON
}

// parseInterleaved parses flags and positionals in any order, so
// `keel team member add <team> <agent> --label x` reads like the help text. The
// stdlib stops at the first positional; this resumes after each one.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	positional := make([]string, 0, len(args))
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// visitSet returns the flag names the user actually passed, so an update can
// distinguish "set to empty" from "not mentioned". A full-replace PUT would
// otherwise clear every field the command line did not repeat.
func visitSet(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// csv splits a comma list, dropping blanks.
func csv(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// newFlagSet builds a quiet flag set; usage goes to stderr only.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// --- printing ------------------------------------------------------------

func printJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printAgentTable(w io.Writer, views []httpapi.AgentView) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tMODEL\tTOOLS\tARCHIVED")
	for _, view := range views {
		model := "-"
		if view.DefaultModel.Complete() {
			model = view.DefaultModel.ProviderID + "/" + view.DefaultModel.ModelID
		} else if view.DefaultModel.Inherits() {
			model = "(inherit)"
		}
		archived := ""
		if view.Archived {
			archived = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", view.ID, view.Name, model, len(view.Tools), archived)
	}
	_ = tw.Flush()
}

func printTeamTable(w io.Writer, views []httpapi.AgentTeamView) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCAPTAIN\tMEMBERS\tSTATUS\tREV")
	for _, view := range views {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\n",
			view.ID, view.Name, view.Captain.AgentID, len(view.Members), view.Status, view.Revision)
	}
	_ = tw.Flush()
}

// --- agent commands ------------------------------------------------------

func runAgentList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent list", stderr)
	server, asJSON := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var views []httpapi.AgentView
	if err := client.do(http.MethodGet, "/v1/agents", "", nil, &views); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, views); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	printAgentTable(stdout, views)
	return 0
}

func runAgentShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent show", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel agent show <id>")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var view httpapi.AgentView
	if err := client.do(http.MethodGet, "/v1/agents/"+url.PathEscape(positional[0]), "", nil, &view); err != nil {
		return reportError(stderr, err)
	}
	if err := printJSON(stdout, view); err != nil {
		return reportError(stderr, err)
	}
	return 0
}

// agentWriteFlags are the editable fields both create and update accept.
type agentWriteFlags struct {
	id, name, description string
	provider, model       string
	tools, skills         string
	prompt, promptFile    string
	teamCLI               bool
	teamCLICommands       string
}

func registerAgentWriteFlags(fs *flag.FlagSet, values *agentWriteFlags) {
	fs.StringVar(&values.id, "id", "", "stable agent id (create only; minted when omitted)")
	fs.StringVar(&values.name, "name", "", "display name")
	fs.StringVar(&values.description, "description", "", "description")
	fs.StringVar(&values.provider, "provider", "", "default model provider id")
	fs.StringVar(&values.model, "model", "", "default model id")
	fs.StringVar(&values.tools, "tool", "", "comma list of tool ids")
	fs.StringVar(&values.prompt, "prompt", "", "inline system prompt")
	fs.StringVar(&values.promptFile, "prompt-file", "", "system prompt file reference")
	fs.StringVar(&values.skills, "skills", "", "comma list of skill names")
	fs.BoolVar(&values.teamCLI, "team-cli", false, "enable the captain team CLI")
	fs.StringVar(&values.teamCLICommands, "team-cli-commands", "", "comma list of allowlisted team subcommands")
}

// apply overlays the flags the user passed onto body. set is the visited-flag
// set, so an update keeps every field the command line did not mention.
func (flags agentWriteFlags) apply(body *httpapi.AgentWriteRequest, set map[string]bool) {
	if set["id"] {
		body.ID = flags.id
	}
	if set["name"] {
		body.Name = flags.name
	}
	if set["description"] {
		body.Description = flags.description
	}
	if set["provider"] || set["model"] {
		body.DefaultModel = agentteam.ModelBinding{ProviderID: flags.provider, ModelID: flags.model}
	}
	if set["tool"] {
		grants := make([]agentteam.ToolGrant, 0)
		for _, id := range csv(flags.tools) {
			grants = append(grants, agentteam.ToolGrant{ToolID: id, Enabled: true})
		}
		body.Tools = grants
	}
	if set["prompt"] || set["prompt-file"] {
		switch {
		case flags.promptFile != "":
			body.SystemPrompt = agentteam.SystemPrompt{Kind: agentteam.SystemPromptFile, Ref: flags.promptFile}
		case flags.prompt != "":
			body.SystemPrompt = agentteam.SystemPrompt{Kind: agentteam.SystemPromptInline, Text: flags.prompt}
		default:
			body.SystemPrompt = agentteam.SystemPrompt{}
		}
	}
	if set["skills"] {
		body.Skills = csv(flags.skills)
	}
	if set["team-cli"] {
		body.TeamCLI.Enabled = flags.teamCLI
	}
	if set["team-cli-commands"] {
		body.TeamCLI.Commands = csv(flags.teamCLICommands)
	}
}

func runAgentCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent create", stderr)
	server, _ := commonFlags(fs)
	var values agentWriteFlags
	registerAgentWriteFlags(fs, &values)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintf(stderr, "keel agent create: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if strings.TrimSpace(values.name) == "" {
		fmt.Fprintln(stderr, "keel agent create: --name is required")
		return 2
	}
	var body httpapi.AgentWriteRequest
	values.apply(&body, map[string]bool{
		"id": true, "name": true, "description": true, "provider": true, "model": true,
		"tool": true, "prompt": true, "prompt-file": true, "skills": true,
		"team-cli": true, "team-cli-commands": true,
	})
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var view httpapi.AgentView
	if err := client.do(http.MethodPost, "/v1/agents", "", body, &view); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "created agent %s (%s)\n", view.ID, view.Name)
	return 0
}

func runAgentUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent update", stderr)
	server, _ := commonFlags(fs)
	var values agentWriteFlags
	registerAgentWriteFlags(fs, &values)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel agent update <id> [flags]")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	path := "/v1/agents/" + url.PathEscape(positional[0])
	var current httpapi.AgentView
	if err := client.do(http.MethodGet, path, "", nil, &current); err != nil {
		return reportError(stderr, err)
	}
	body := httpapi.AgentWriteRequest{
		ID:           current.ID,
		Name:         current.Name,
		Description:  current.Description,
		Avatar:       current.Avatar,
		DefaultModel: current.DefaultModel,
		Tools:        current.Tools,
		SystemPrompt: current.SystemPrompt,
		TeamCLI:      current.TeamCLI,
		Skills:       current.Skills,
	}
	values.apply(&body, visitSet(fs))
	// The path owns the identity; a --id on update would be a second identity.
	body.ID = current.ID
	var view httpapi.AgentView
	if err := client.do(http.MethodPut, path, "", body, &view); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "updated agent %s (%s)\n", view.ID, view.Name)
	return 0
}

func runAgentDelete(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel agent delete", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel agent delete <id>")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer map[string]any
	if err := client.do(http.MethodDelete, "/v1/agents/"+url.PathEscape(positional[0]), "", nil, &answer); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "deleted agent %s\n", positional[0])
	return 0
}

// --- team commands -------------------------------------------------------

func runTeamList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team list", stderr)
	server, asJSON := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var views []httpapi.AgentTeamView
	if err := client.do(http.MethodGet, "/v1/teams", "", nil, &views); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, views); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	printTeamTable(stdout, views)
	return 0
}

func runTeamShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team show", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel team show <id>")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var view httpapi.AgentTeamView
	if err := client.do(http.MethodGet, "/v1/teams/"+url.PathEscape(positional[0]), "", nil, &view); err != nil {
		return reportError(stderr, err)
	}
	if err := printJSON(stdout, view); err != nil {
		return reportError(stderr, err)
	}
	return 0
}

// teamWriteFlags are the editable team fields create and update accept.
type teamWriteFlags struct {
	id, name, description string
	captain               string
	members               stringList
	teamCLI               bool
	teamCLICommands       string
	defaultTools          string
}

// stringList is a repeatable string flag (--member a --member b).
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func registerTeamWriteFlags(fs *flag.FlagSet, values *teamWriteFlags) {
	fs.StringVar(&values.id, "id", "", "stable team id (create only; minted when omitted)")
	fs.StringVar(&values.name, "name", "", "display name")
	fs.StringVar(&values.description, "description", "", "description")
	fs.StringVar(&values.captain, "captain", "", "captain agent id (required on create)")
	fs.Var(&values.members, "member", "member seat agentId[:label] (repeatable)")
	fs.BoolVar(&values.teamCLI, "team-cli", false, "enable the team's team CLI switch")
	fs.StringVar(&values.teamCLICommands, "team-cli-commands", "", "comma list of allowlisted team subcommands")
	fs.StringVar(&values.defaultTools, "default-tool", "", "comma list of tool ids granted to every member")
}

// apply overlays the flags the user passed onto body.
func (flags teamWriteFlags) apply(body *httpapi.AgentTeamWriteRequest, set map[string]bool) {
	if set["id"] {
		body.ID = flags.id
	}
	if set["name"] {
		body.Name = flags.name
	}
	if set["description"] {
		body.Description = flags.description
	}
	if set["captain"] {
		body.Captain.AgentID = strings.TrimSpace(flags.captain)
	}
	if set["member"] {
		seats := make([]agentteam.TeamMember, 0, len(flags.members))
		for _, raw := range flags.members {
			agentID, label, _ := strings.Cut(raw, ":")
			seats = append(seats, agentteam.TeamMember{
				AgentID: strings.TrimSpace(agentID),
				Label:   strings.TrimSpace(label),
			})
		}
		body.Members = seats
	}
	if set["team-cli"] {
		body.Capabilities.TeamCLI.Enabled = flags.teamCLI
	}
	if set["team-cli-commands"] {
		body.Capabilities.TeamCLI.Commands = csv(flags.teamCLICommands)
	}
	if set["default-tool"] {
		body.Defaults.Tools = csv(flags.defaultTools)
	}
}

func runTeamCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team create", stderr)
	server, _ := commonFlags(fs)
	var values teamWriteFlags
	registerTeamWriteFlags(fs, &values)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintf(stderr, "keel team create: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if strings.TrimSpace(values.name) == "" {
		fmt.Fprintln(stderr, "keel team create: --name is required")
		return 2
	}
	if strings.TrimSpace(values.captain) == "" {
		fmt.Fprintln(stderr, "keel team create: --captain <agentId> is required (a team must have exactly one captain)")
		return 2
	}
	var body httpapi.AgentTeamWriteRequest
	values.apply(&body, map[string]bool{
		"id": true, "name": true, "description": true, "captain": true, "member": true,
		"team-cli": true, "team-cli-commands": true, "default-tool": true,
	})
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var view httpapi.AgentTeamView
	if err := client.do(http.MethodPost, "/v1/teams", "", body, &view); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "created team %s (%s) captain=%s revision=%d\n",
		view.ID, view.Name, view.Captain.AgentID, view.Revision)
	return 0
}

func runTeamUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team update", stderr)
	server, _ := commonFlags(fs)
	var values teamWriteFlags
	registerTeamWriteFlags(fs, &values)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel team update <id> [flags]")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	path := "/v1/teams/" + url.PathEscape(positional[0])
	var current httpapi.AgentTeamView
	if err := client.do(http.MethodGet, path, "", nil, &current); err != nil {
		return reportError(stderr, err)
	}
	body := httpapi.AgentTeamWriteRequest{
		ID:              current.ID,
		Name:            current.Name,
		Description:     current.Description,
		Captain:         current.Captain,
		Members:         current.Members,
		Defaults:        current.Defaults,
		Capabilities:    current.Capabilities,
		FeatureBindings: current.FeatureBindings,
	}
	values.apply(&body, visitSet(fs))
	body.ID = current.ID
	var view httpapi.AgentTeamView
	if err := client.do(http.MethodPut, path, "", body, &view); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "updated team %s (%s) revision=%d\n", view.ID, view.Name, view.Revision)
	return 0
}

func runTeamDelete(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team delete", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel team delete <id>")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	if err := client.do(http.MethodDelete, "/v1/teams/"+url.PathEscape(positional[0]), "", nil, nil); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "deleted team %s\n", positional[0])
	return 0
}

func runTeamMember(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: keel team member add|remove <teamId> <agentId>")
		return 2
	}
	switch args[0] {
	case "add":
		return runTeamMemberAdd(args[1:], stdout, stderr)
	case "remove":
		return runTeamMemberRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "keel team member: unknown command %q (add|remove)\n", args[0])
		return 2
	}
}

func runTeamMemberAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team member add", stderr)
	server, _ := commonFlags(fs)
	label := fs.String("label", "", "team-local label")
	description := fs.String("description", "", "team-local role text")
	actor := fs.String("as", "", "act as this agent (the captain) so the captain's team CLI is checked")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 2 {
		fmt.Fprintln(stderr, "usage: keel team member add <teamId> <agentId> [--label <l>] [--description <d>] [--as <captainAgentId>]")
		return 2
	}
	member := agentteam.TeamMember{AgentID: strings.TrimSpace(positional[1])}
	if *label != "" {
		member.Label = *label
	}
	if *description != "" {
		member.Description = *description
	}
	return sendMemberMutation(*server, http.MethodPost,
		"/v1/teams/"+url.PathEscape(positional[0])+"/members", *actor, member, "added", positional, stdout, stderr)
}

func runTeamMemberRemove(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team member remove", stderr)
	server, _ := commonFlags(fs)
	actor := fs.String("as", "", "act as this agent (the captain) so the captain's team CLI is checked")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 2 {
		fmt.Fprintln(stderr, "usage: keel team member remove <teamId> <agentId> [--as <captainAgentId>]")
		return 2
	}
	return sendMemberMutation(*server, http.MethodDelete,
		"/v1/teams/"+url.PathEscape(positional[0])+"/members/"+url.PathEscape(positional[1]),
		*actor, nil, "removed", positional, stdout, stderr)
}

// sendMemberMutation performs one member add/remove and prints the resulting
// team revision, so a captain sees the configuration it just changed.
func sendMemberMutation(server, method, path, actor string, body any, verb string,
	positional []string, stdout, stderr io.Writer) int {
	client, err := newCLIClient(server)
	if err != nil {
		return reportError(stderr, err)
	}
	var view httpapi.AgentTeamView
	if err := client.do(method, path, actor, body, &view); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "%s agent %s on team %s (members=%d revision=%d)\n",
		verb, positional[1], view.ID, len(view.Members), view.Revision)
	return 0
}
