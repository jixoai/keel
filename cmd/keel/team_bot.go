// `keel team-bot ...`: the CLI surface of TeamBots (change keel-bot-identity-access,
// A4.2b3a).
//
// A TeamBot is Mission + Roster. It is a resource, never a permission subject:
// it has no Account/Profile and grants nothing. `keel team` is deliberately NOT
// an alias — the retired product word is an unknown command (alpha zero
// compatibility).
package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/jixoai/keel/internal/botteam"
)

// teamBotView mirrors the /v1/team-bots projection.
type teamBotView struct {
	TeamBotID string                `json:"teamBotId"`
	Name      string                `json:"name"`
	Mission   string                `json:"mission"`
	Roster    []botteam.RosterEntry `json:"roster"`
	Revision  int64                 `json:"revision"`
	Hash      string                `json:"hash"`
	Archived  bool                  `json:"archived"`
	CreatedAt string                `json:"createdAt"`
	UpdatedAt string                `json:"updatedAt"`
}

type teamBotAnswer struct {
	TeamBot teamBotView `json:"teamBot"`
}

type teamBotListAnswer struct {
	TeamBots []teamBotView `json:"teamBots"`
}

func runTeamBotCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		teamBotUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runTeamBotList(args[1:], stdout, stderr)
	case "show":
		return runTeamBotShow(args[1:], stdout, stderr)
	case "create":
		return runTeamBotCreate(args[1:], stdout, stderr)
	case "update":
		return runTeamBotUpdate(args[1:], stdout, stderr)
	case "archive":
		return runTeamBotArchive(args[1:], stdout, stderr)
	case "roster":
		return runTeamBotRoster(args[1:], stdout, stderr)
	case "start":
		return runTeamBotStart(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		teamBotUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel team-bot: unknown command %q\n\n", args[0])
		teamBotUsage(stderr)
		return 2
	}
}

func teamBotUsage(w io.Writer) {
	fmt.Fprint(w, `keel team-bot — manage TeamBots over a running keel serve

Usage:
  keel team-bot list [--include-archived] [--json]
  keel team-bot show <teamBotId> [--json]
  keel team-bot create --name <text> --mission <text>
                       [--team-bot-id <handle>] [--roster bot1:captain,bot2:member]
                       [--json]
  keel team-bot update <teamBotId> [--name <text>] [--mission <text>]
                       [--expected-revision <n>] [--json]
  keel team-bot archive <teamBotId>
  keel team-bot roster <teamBotId> --bot <botId> [--role captain|member]
  keel team-bot start <teamBotId> [--text <message>] [--session <uuid>] [--request-id <id>]

A TeamBot is Mission + Roster: it has no Account/Profile and grants no
permission. Whether a Bot can be seated or invoked is decided by bot.invoke on
that Bot. A roster always keeps at least one captain.

Common flags:
  --server <addr>           keel serve address (default $KEEL_SERVE_ADDR or 127.0.0.1:13750)
  --username <name>         account login name (default $KEEL_ACCOUNT_USERNAME)
  --password-file <path>    read the account password from a file (default
                            $KEEL_ACCOUNT_PASSWORD); never pass a password flag
  --json                    print raw JSON instead of a table

Authentication: KEEL_ACCOUNT_USERNAME + KEEL_ACCOUNT_PASSWORD log in with
username+password (POST /v1/auth/login); KEEL_AGENT_TOKEN presents a bot key
(keel_bot_...) as a Bearer credential and never logs in. KEEL_SERVE_PASSWORD is
retired and refused with an explanation. Passwords are never taken from a flag
and never printed.
`)
}

func printTeamBotTable(w io.Writer, teams []teamBotView) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TEAM BOT\tNAME\tCAPTAIN\tMEMBERS\tREV\tARCHIVED")
	for _, team := range teams {
		captain, members := "", 0
		for _, seat := range team.Roster {
			if seat.Role == botteam.RosterRoleCaptain && captain == "" {
				captain = seat.BotID
			}
			if seat.Role == botteam.RosterRoleMember {
				members++
			}
		}
		archived := ""
		if team.Archived {
			archived = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
			team.TeamBotID, team.Name, captain, members, team.Revision, archived)
	}
	_ = tw.Flush()
}

func runTeamBotList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot list", stderr)
	server, asJSON := commonFlags(fs)
	includeArchived := fs.Bool("include-archived", false, "include archived team bots")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	path := "/v1/team-bots"
	if *includeArchived {
		path += "?includeArchived=true"
	}
	var answer teamBotListAnswer
	if err := client.do(http.MethodGet, path, nil, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	if len(answer.TeamBots) == 0 {
		fmt.Fprintln(stdout, "no team bots")
		return 0
	}
	printTeamBotTable(stdout, answer.TeamBots)
	return 0
}

func runTeamBotShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot show", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel team-bot show: exactly one teamBotId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer teamBotAnswer
	if err := client.do(http.MethodGet, "/v1/team-bots/"+positional[0], nil, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	team := answer.TeamBot
	fmt.Fprintf(stdout, "team bot: %s\n", team.TeamBotID)
	fmt.Fprintf(stdout, "name:     %s\n", team.Name)
	fmt.Fprintf(stdout, "mission:  %s\n", team.Mission)
	fmt.Fprintf(stdout, "revision: %d\n", team.Revision)
	fmt.Fprintf(stdout, "hash:     %s\n", team.Hash)
	fmt.Fprintf(stdout, "archived: %v\n", team.Archived)
	fmt.Fprintln(stdout, "roster:")
	for _, seat := range team.Roster {
		fmt.Fprintf(stdout, "  %s (%s)\n", seat.BotID, seat.Role)
	}
	return 0
}

// parseRosterFlag parses "bot1:captain,bot2:member". A bare id is a member.
func parseRosterFlag(raw string) []botteam.RosterEntry {
	parts := csv(raw)
	roster := make([]botteam.RosterEntry, 0, len(parts))
	for _, part := range parts {
		botID, role, found := strings.Cut(part, ":")
		if !found {
			role = string(botteam.RosterRoleMember)
		}
		roster = append(roster, botteam.RosterEntry{
			BotID: strings.TrimSpace(botID),
			Role:  botteaRole(role),
		})
	}
	return roster
}

func botteaRole(raw string) botteam.RosterRole {
	return botteam.RosterRole(strings.ToLower(strings.TrimSpace(raw)))
}

func runTeamBotCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot create", stderr)
	server, asJSON := commonFlags(fs)
	teamBotID := fs.String("team-bot-id", "", "stable team bot handle (minted when omitted)")
	name := fs.String("name", "", "display name")
	mission := fs.String("mission", "", "team mission")
	roster := fs.String("roster", "", "roster as bot1:captain,bot2:member")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*mission) == "" {
		fmt.Fprintln(stderr, "keel team-bot create: --name and --mission are required")
		return 2
	}
	body := map[string]any{
		"name":    strings.TrimSpace(*name),
		"mission": strings.TrimSpace(*mission),
	}
	if *teamBotID != "" {
		body["teamBotId"] = strings.TrimSpace(*teamBotID)
	}
	if *roster != "" {
		body["roster"] = parseRosterFlag(*roster)
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer teamBotAnswer
	if err := client.do(http.MethodPost, "/v1/team-bots", body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "created team bot: %s\n", answer.TeamBot.TeamBotID)
	return 0
}

func runTeamBotUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot update", stderr)
	server, asJSON := commonFlags(fs)
	name := fs.String("name", "", "display name")
	mission := fs.String("mission", "", "team mission")
	expectedRevision := fs.Int64("expected-revision", -1, "revision the caller read (fetched when omitted)")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel team-bot update: exactly one teamBotId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	if *expectedRevision < 0 {
		var current teamBotAnswer
		if err := client.do(http.MethodGet, "/v1/team-bots/"+positional[0], nil, &current); err != nil {
			return reportError(stderr, err)
		}
		*expectedRevision = current.TeamBot.Revision
	}
	body := map[string]any{"expectedRevision": *expectedRevision}
	set := visitSet(fs)
	if set["name"] {
		body["name"] = strings.TrimSpace(*name)
	}
	if set["mission"] {
		body["mission"] = strings.TrimSpace(*mission)
	}
	var answer teamBotAnswer
	if err := client.do(http.MethodPatch, "/v1/team-bots/"+positional[0], body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "updated team bot: %s (revision %d)\n", answer.TeamBot.TeamBotID, answer.TeamBot.Revision)
	return 0
}

func runTeamBotArchive(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot archive", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel team-bot archive: exactly one teamBotId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	if err := client.do(http.MethodPost, "/v1/team-bots/"+positional[0]+"/archive", map[string]any{}, nil); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "archived team bot: %s\n", positional[0])
	return 0
}

// runTeamBotRoster adds one seat (or updates its role) by reading the current
// roster, merging, and writing the full list with the revision CAS. The server
// keeps at least one captain; replacing the captain's role with member is the
// caller's choice and the server refuses a roster with no captain.
func runTeamBotRoster(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot roster", stderr)
	server, asJSON := commonFlags(fs)
	botID := fs.String("bot", "", "bot id to add or update")
	role := fs.String("role", "member", "roster role: captain|member")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel team-bot roster: exactly one teamBotId is required")
		return 2
	}
	if strings.TrimSpace(*botID) == "" {
		fmt.Fprintln(stderr, "keel team-bot roster: --bot is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var current teamBotAnswer
	if err := client.do(http.MethodGet, "/v1/team-bots/"+positional[0], nil, &current); err != nil {
		return reportError(stderr, err)
	}
	roster := make([]botteam.RosterEntry, 0, len(current.TeamBot.Roster)+1)
	replaced := false
	for _, seat := range current.TeamBot.Roster {
		if seat.BotID == strings.TrimSpace(*botID) {
			seat.Role = botteaRole(*role)
			replaced = true
		}
		roster = append(roster, seat)
	}
	if !replaced {
		roster = append(roster, botteam.RosterEntry{BotID: strings.TrimSpace(*botID), Role: botteaRole(*role)})
	}
	body := map[string]any{"roster": roster, "expectedRevision": current.TeamBot.Revision}
	var answer teamBotAnswer
	if err := client.do(http.MethodPost, "/v1/team-bots/"+positional[0]+"/roster", body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "roster updated: %s (revision %d)\n", answer.TeamBot.TeamBotID, answer.TeamBot.Revision)
	return 0
}

func runTeamBotStart(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel team-bot start", stderr)
	server, asJSON := commonFlags(fs)
	text := fs.String("text", defaultStartText, "first user message of the run")
	session := fs.String("session", "", "continue an existing session UUID")
	requestID := fs.String("request-id", "", "idempotency key")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel team-bot start: exactly one teamBotId is required")
		return 2
	}
	body := map[string]any{"text": *text, "requestId": *requestID}
	if *session != "" {
		body["sessionId"] = *session
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer map[string]any
	if err := client.do(http.MethodPost, "/v1/team-bots/"+positional[0]+"/start", body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "captain bot: %v\nsession: %v\nseq: %v\n",
		answer["captainBotId"], answer["sessionId"], answer["seq"])
	return 0
}
