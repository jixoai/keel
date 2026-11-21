// `keel bot ...`: the CLI surface of Bots (change keel-bot-identity-access,
// A4.2b3a).
//
// A Bot is the runnable AI employee (Profile + ModelKind + default model +
// Harness). The CLI is a thin client of /v1/bots: one write path and one
// validation path, shared with the web app. `keel agent` is deliberately NOT
// an alias — the retired product word is an unknown command (alpha zero
// compatibility), so a stale script fails loudly instead of silently editing a
// different surface.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/jixoai/keel/internal/botteam"
)

// defaultStartText is the first user message a bare `keel bot start` sends. A
// run needs one input fact; an empty text would be refused, so the CLI sends a
// neutral kickoff the model can answer instead of inventing a task.
const defaultStartText = "Start."

// botView mirrors the /v1/bots projection.
type botView struct {
	BotID        string           `json:"botId"`
	Name         string           `json:"name"`
	ProfileID    string           `json:"profileId"`
	AccountID    string           `json:"accountId"`
	ModelKind    string           `json:"modelKind"`
	DefaultModel botteam.ModelRef `json:"defaultModel"`
	Harness      botteam.Harness  `json:"harness"`
	Archived     bool             `json:"archived"`
	CreatedAt    string           `json:"createdAt"`
	UpdatedAt    string           `json:"updatedAt"`
}

type botAnswer struct {
	Bot botView `json:"bot"`
}

type botListAnswer struct {
	Bots []botView `json:"bots"`
}

func runBotCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		botUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runBotList(args[1:], stdout, stderr)
	case "show":
		return runBotShow(args[1:], stdout, stderr)
	case "create":
		return runBotCreate(args[1:], stdout, stderr)
	case "update":
		return runBotUpdate(args[1:], stdout, stderr)
	case "archive":
		return runBotArchive(args[1:], stdout, stderr)
	case "start":
		return runBotStart(args[1:], stdout, stderr)
	case "keys":
		return runBotKeys(args[1:], stdout, stderr)
	case "key-create":
		return runBotKeyCreate(args[1:], stdout, stderr)
	case "key-revoke":
		return runBotKeyRevoke(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		botUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel bot: unknown command %q\n\n", args[0])
		botUsage(stderr)
		return 2
	}
}

func botUsage(w io.Writer) {
	fmt.Fprint(w, `keel bot — manage Bots over a running keel serve

Usage:
  keel bot list [--include-archived] [--json]
  keel bot show <botId> [--json]
  keel bot create --name <text> --profile <uuid> --model-kind text
                  --provider <id> --model <id>
                  [--bot-id <handle>] [--tools bash,mcp] [--permissions session.read]
                  [--instruction <text>] [--json]
  keel bot update <botId> [--name ...] [--profile <uuid>] [--model-kind ...]
                  [--provider ...] [--model ...] [--tools ...] [--permissions ...]
                  [--instruction ...] [--archived] [--json]
  keel bot archive <botId>
  keel bot start <botId> [--text <message>] [--session <uuid>] [--request-id <id>]
  keel bot keys <botId> [--json]
  keel bot key-create <botId> [--label <text>] [--json]
  keel bot key-revoke <botId> <keyId>

A Bot's Harness tools are a whitelist: only the listed tools are available, and
an empty list grants nothing. The Harness permissions narrow the owning
Account's grants (Account ∩ Harness); an empty list narrows to nothing.

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

func printBotTable(w io.Writer, bots []botView) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BOT\tNAME\tPROFILE\tMODEL\tTOOLS\tARCHIVED")
	for _, bot := range bots {
		model := "-"
		if bot.DefaultModel.Complete() {
			model = bot.DefaultModel.ProviderID + "/" + bot.DefaultModel.ModelID
		}
		archived := ""
		if bot.Archived {
			archived = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", bot.BotID, bot.Name, bot.ProfileID,
			model, len(bot.Harness.Tools), archived)
	}
	_ = tw.Flush()
}

func runBotList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot list", stderr)
	server, asJSON := commonFlags(fs)
	includeArchived := fs.Bool("include-archived", false, "include archived bots")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	path := "/v1/bots"
	if *includeArchived {
		path += "?includeArchived=true"
	}
	var answer botListAnswer
	if err := client.do(http.MethodGet, path, nil, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	if len(answer.Bots) == 0 {
		fmt.Fprintln(stdout, "no bots")
		return 0
	}
	printBotTable(stdout, answer.Bots)
	return 0
}

func runBotShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot show", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel bot show: exactly one botId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer botAnswer
	if err := client.do(http.MethodGet, "/v1/bots/"+positional[0], nil, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	bot := answer.Bot
	fmt.Fprintf(stdout, "bot:         %s\n", bot.BotID)
	fmt.Fprintf(stdout, "name:        %s\n", bot.Name)
	fmt.Fprintf(stdout, "profile:     %s\n", bot.ProfileID)
	fmt.Fprintf(stdout, "account:     %s\n", bot.AccountID)
	fmt.Fprintf(stdout, "model kind:  %s\n", bot.ModelKind)
	fmt.Fprintf(stdout, "model:       %s/%s\n", bot.DefaultModel.ProviderID, bot.DefaultModel.ModelID)
	fmt.Fprintf(stdout, "tools:       %s\n", strings.Join(bot.Harness.Tools, ", "))
	fmt.Fprintf(stdout, "permissions: %s\n", strings.Join(bot.Harness.Context.Permissions, ", "))
	if bot.Harness.Context.Instruction != "" {
		fmt.Fprintf(stdout, "instruction: %s\n", bot.Harness.Context.Instruction)
	}
	fmt.Fprintf(stdout, "archived:    %v\n", bot.Archived)
	return 0
}

type botWriteFlags struct {
	botID       *string
	name        *string
	profile     *string
	modelKind   *string
	provider    *string
	model       *string
	tools       *string
	permission  *string
	instruction *string
	archived    *bool
}

func registerBotWriteFlags(fs *flag.FlagSet) *botWriteFlags {
	return &botWriteFlags{
		botID:       fs.String("bot-id", "", "stable bot handle (minted when omitted)"),
		name:        fs.String("name", "", "display name"),
		profile:     fs.String("profile", "", "profile UUID"),
		modelKind:   fs.String("model-kind", "", "model kind: text|image|decision|realtime"),
		provider:    fs.String("provider", "", "default model provider id"),
		model:       fs.String("model", "", "default model id"),
		tools:       fs.String("tools", "", "comma-separated Harness tool whitelist"),
		permission:  fs.String("permissions", "", "comma-separated Harness permission narrowing"),
		instruction: fs.String("instruction", "", "Harness context instruction"),
		archived:    fs.Bool("archived", false, "archive the bot"),
	}
}

func (f *botWriteFlags) body(set map[string]bool) map[string]any {
	body := map[string]any{}
	if set["bot-id"] {
		body["botId"] = strings.TrimSpace(*f.botID)
	}
	if set["name"] {
		body["name"] = strings.TrimSpace(*f.name)
	}
	if set["profile"] {
		body["profileId"] = strings.TrimSpace(*f.profile)
	}
	if set["model-kind"] {
		body["modelKind"] = strings.TrimSpace(*f.modelKind)
	}
	if set["provider"] || set["model"] {
		model := map[string]string{}
		if set["provider"] {
			model["providerId"] = strings.TrimSpace(*f.provider)
		}
		if set["model"] {
			model["modelId"] = strings.TrimSpace(*f.model)
		}
		body["defaultModel"] = model
	}
	if set["tools"] || set["permissions"] || set["instruction"] {
		harness := map[string]any{}
		if set["tools"] {
			harness["tools"] = csv(*f.tools)
		}
		if set["permissions"] || set["instruction"] {
			context := map[string]any{}
			if set["permissions"] {
				context["permissions"] = csv(*f.permission)
			}
			if set["instruction"] {
				context["instruction"] = strings.TrimSpace(*f.instruction)
			}
			harness["context"] = context
		}
		body["harness"] = harness
	}
	if set["archived"] {
		body["archived"] = *f.archived
	}
	return body
}

func runBotCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot create", stderr)
	server, asJSON := commonFlags(fs)
	flags := registerBotWriteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	set := visitSet(fs)
	body := flags.body(set)
	// Create requires the four identity facts; a missing one is a usage error,
	// not a server 400 the operator has to decode.
	for _, required := range []string{"name", "profile", "model-kind"} {
		if !set[required] {
			fmt.Fprintf(stderr, "keel bot create: --%s is required\n", required)
			return 2
		}
	}
	if !set["provider"] || !set["model"] {
		fmt.Fprintln(stderr, "keel bot create: --provider and --model are required (a bot always has a default model)")
		return 2
	}
	body["expectedArchived"] = false
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer botAnswer
	if err := client.do(http.MethodPost, "/v1/bots", body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "created bot: %s\n", answer.Bot.BotID)
	return 0
}

func runBotUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot update", stderr)
	server, asJSON := commonFlags(fs)
	flags := registerBotWriteFlags(fs)
	expectedArchived := fs.Bool("expected-archived", false, "expected archived state for the CAS write")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel bot update: exactly one botId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	// The CAS flag is what the caller read; default false is the normal case.
	var current botAnswer
	if err := client.do(http.MethodGet, "/v1/bots/"+positional[0], nil, &current); err != nil {
		return reportError(stderr, err)
	}
	body := flags.body(visitSet(fs))
	body["expectedArchived"] = *expectedArchived
	if _, ok := body["expectedArchived"]; ok && !visitSet(fs)["expected-archived"] {
		body["expectedArchived"] = current.Bot.Archived
	}
	var answer botAnswer
	if err := client.do(http.MethodPatch, "/v1/bots/"+positional[0], body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "updated bot: %s\n", answer.Bot.BotID)
	return 0
}

func runBotArchive(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot archive", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel bot archive: exactly one botId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	if err := client.do(http.MethodPost, "/v1/bots/"+positional[0]+"/archive", map[string]any{}, nil); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "archived bot: %s\n", positional[0])
	return 0
}

func runBotStart(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot start", stderr)
	server, asJSON := commonFlags(fs)
	text := fs.String("text", defaultStartText, "first user message of the run")
	session := fs.String("session", "", "continue an existing session UUID")
	requestID := fs.String("request-id", "", "idempotency key")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel bot start: exactly one botId is required")
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
	if err := client.do(http.MethodPost, "/v1/bots/"+positional[0]+"/invoke", body, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "session: %v\nseq: %v\n", answer["sessionId"], answer["seq"])
	return 0
}

type botKeyView struct {
	KeyID      string `json:"keyId"`
	Prefix     string `json:"prefix"`
	Label      string `json:"label"`
	CreatedAt  string `json:"createdAt"`
	RevokedAt  string `json:"revokedAt"`
	LastUsedAt string `json:"lastUsedAt"`
}

func runBotKeys(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot keys", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel bot keys: exactly one botId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer struct {
		Keys []botKeyView `json:"keys"`
	}
	if err := client.do(http.MethodGet, "/v1/bots/"+positional[0]+"/keys", nil, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	if len(answer.Keys) == 0 {
		fmt.Fprintln(stdout, "no keys")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tPREFIX\tLABEL\tCREATED\tREVOKED\tLAST USED")
	for _, key := range answer.Keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", key.KeyID, key.Prefix, key.Label,
			key.CreatedAt, key.RevokedAt, key.LastUsedAt)
	}
	_ = tw.Flush()
	return 0
}

func runBotKeyCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot key-create", stderr)
	server, asJSON := commonFlags(fs)
	label := fs.String("label", "", "label for the key")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "keel bot key-create: exactly one botId is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var answer map[string]any
	if err := client.do(http.MethodPost, "/v1/bots/"+positional[0]+"/keys",
		map[string]string{"label": strings.TrimSpace(*label)}, &answer); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	// The plaintext appears exactly once, in this answer.
	fmt.Fprintf(stdout, "key id: %v\nprefix: %v\n\nBOT KEY (shown once, store it now):\n%v\n\n",
		answer["keyId"], answer["prefix"], answer["key"])
	fmt.Fprintln(stdout, "This key cannot be shown again. Issue a new one if it is lost.")
	return 0
}

func runBotKeyRevoke(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel bot key-revoke", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 2 {
		fmt.Fprintln(stderr, "keel bot key-revoke: <botId> <keyId> are required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	if err := client.do(http.MethodDelete, "/v1/bots/"+positional[0]+"/keys/"+positional[1], nil, nil); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "revoked key: %s\n", positional[1])
	return 0
}
