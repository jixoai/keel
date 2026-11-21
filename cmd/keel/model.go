// `keel model ...`: the CLI of the model registry — the keel.model plugin's
// declared command surface (provides.cli in
// internal/pluginpkg/builtin/keel.model.yaml).
//
// It is a thin HTTP client of a running `keel serve`, exactly like `keel agent`
// and `keel team`: GET /v1/providers and GET /v1/models are the same resolved
// projections the settings page and the session picker read, so the three
// surfaces cannot drift apart. Nothing here opens the database or re-implements
// a resolution rule.
//
// The write subset (`keel model provider add|update|remove`, and the read-only
// probe `keel model provider test`) goes through the one provider write path
// this deployment serves: PUT /v1/settings/models, the tenant models document,
// read-modify-write with the revision guard. The CLI edits the document's
// providers[] entries and sends everything else back verbatim, so a write never
// drops configuration the command does not manage, and the server's validation
// (plus its reference protection) stays the only authority on what is legal.
//
// The declared capability tree and this file are one unit: a command declared
// in the manifest must exist here, and a command added here must be declared
// there (TestCapabilityPluginsDeclareTheirCLI guards one direction,
// cmd/keel/model_test.go exercises every declared command against a stub
// server).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/jixoai/keel/internal/modelcfg"
)

// --- dispatch ---------------------------------------------------------------

func runModelCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		modelUsage(stderr)
		return 2
	}
	switch args[0] {
	case "providers":
		return runModelProviders(args[1:], stdout, stderr)
	case "provider":
		return runModelProviderCmd(args[1:], stdout, stderr)
	case "list":
		return runModelList(args[1:], stdout, stderr)
	case "show":
		return runModelShow(args[1:], stdout, stderr)
	case "catalog":
		return runModelCatalog(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		modelUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel model: unknown command %q\n\n", args[0])
		modelUsage(stderr)
		return 2
	}
}

func modelUsage(w io.Writer) {
	fmt.Fprint(w, `keel model — read the model registry, and manage its provider entries

Usage:
  keel model providers [--json]                 (alias of: keel model provider list)
  keel model list [--json]
  keel model show <providerId>/<modelId>
  keel model catalog [<providerId>] [--refresh] [--json]
  keel model provider list [--json]
  keel model provider show <id>
  keel model provider add <id> [flags]
  keel model provider update <id> [flags]
  keel model provider remove <id> [--repoint-selection <providerId>/<modelId>]
  keel model provider test <id> [--json]

Commands:
  providers   every configured provider: family, executable state, model count
  list        the flat model registry: family, session panel, selectability
  show        one model by "provider/model" (a bare model id must be unique)
  catalog     one provider's resolved model catalog (or the live upstream
              catalog with --refresh, which sends one model-list request)
  provider list      list the configured providers (same data as providers)
  provider show      print one provider's full resolved view as JSON
  provider add       declare a provider entry in the tenant models document
  provider update    edit one entry (or start a patch of a catalog provider)
  provider remove    drop one entry the document declares (reference protected)
  provider test      probe one provider for real: one minimal upstream
                     model-list request through the server's credential

Provider flags:
  --name <display name>     display name
  --type <family>           provider family (llm, image, tts, ...; plugins may
                            declare more — the CLI keeps no enum)
  --api-type <type>         wire API: openai-chat-completions | openai-responses |
                            anthropic-messages
  --base-url <url>          upstream base URL
  --env <NAME>              credential env var NAME (a reference, never a value)
  --ref <secret:NAME>       tenant secret reference
  --model <id>              model id this provider serves (repeatable; replaces the list)
  --header <Key:Value>      static request header (repeatable)
  --visibility <value>      visible | hidden
  On update only the flags passed are applied; everything else, including the
  fields this CLI does not edit, is sent back verbatim.

Common flags:
  --server <addr>           keel serve address (default $KEEL_SERVE_ADDR or 127.0.0.1:13750)
  --json                    print raw JSON instead of a table

The write subset edits the tenant models document (PUT /v1/settings/models,
revision guarded): that document is the one provider write path this deployment
serves. Credentials are only ever references (env names, secret refs); the CLI
neither accepts nor prints a value. `+"`provider test`"+` and `+"`catalog --refresh`"+`
ask the server to make one minimal upstream request, so the credential is
resolved on the server and never travels through the CLI.
Authentication: KEEL_SERVE_PASSWORD is exchanged for the session cookie
(POST /v1/auth/login); it is never taken from a flag and never printed.
`)
}

// --- wire shapes ------------------------------------------------------------

// providerListBody / modelListBody mirror the AgentAPI responses (the fields
// this CLI renders; the JSON decodes the whole object).
type providerListBody struct {
	Providers []modelcfg.ProviderView `json:"providers"`
	Issues    []modelcfg.Issue        `json:"issues"`
}

type modelListBody struct {
	Models []modelcfg.ModelView `json:"models"`
	Issues []modelcfg.Issue     `json:"issues"`
}

// --- commands ----------------------------------------------------------------

func runModelProviders(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model providers", stderr)
	server, asJSON := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body providerListBody
	if err := client.do(http.MethodGet, "/v1/providers", "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	printProviderTable(stdout, body.Providers)
	return 0
}

// runModelProviderList is `keel model provider list`: the same resolved
// settings projection as `keel model providers`, with an optional family
// filter. Filtering is presentation only; the server's projection stays the
// single source of what a provider is.
func runModelProviderList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model provider list", stderr)
	server, asJSON := commonFlags(fs)
	family := fs.String("type", "", "only providers of this family (llm, image, tts, ...)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body providerListBody
	if err := client.do(http.MethodGet, "/v1/providers", "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if wanted := strings.TrimSpace(*family); wanted != "" {
		filtered := make([]modelcfg.ProviderView, 0, len(body.Providers))
		for _, view := range body.Providers {
			if strings.EqualFold(view.ProviderType, wanted) {
				filtered = append(filtered, view)
			}
		}
		body.Providers = filtered
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	printProviderTable(stdout, body.Providers)
	return 0
}

// runModelProviderShow prints one provider's full resolved view as JSON: the
// same facts the settings page renders, with credential *references* only.
// A missing (or hidden) id is a miss, never an empty success.
func runModelProviderShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model provider show", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "usage: keel model provider show <id>")
		return 2
	}
	providerID := strings.TrimSpace(positional[0])
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body providerListBody
	if err := client.do(http.MethodGet, "/v1/providers", "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	for _, view := range body.Providers {
		if view.ProviderID != providerID {
			continue
		}
		if err := printJSON(stdout, view); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stderr,
		"keel model provider show: this server does not serve a provider %q (see keel model provider list)\n", providerID)
	return 1
}

func runModelList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model list", stderr)
	server, asJSON := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body modelListBody
	if err := client.do(http.MethodGet, "/v1/models", "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	printModelTable(stdout, body.Models)
	return 0
}

func runModelShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model show", stderr)
	server, _ := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: keel model show <providerId>/<modelId>")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body modelListBody
	if err := client.do(http.MethodGet, "/v1/models", "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	matches := matchModels(body.Models, positional[0])
	switch len(matches) {
	case 0:
		fmt.Fprintf(stderr, "keel model show: %q does not name a model on this server (see keel model list)\n", positional[0])
		return 1
	case 1:
		if err := printJSON(stdout, matches[0]); err != nil {
			return reportError(stderr, err)
		}
		return 0
	default:
		// A bare model id that several providers carry is ambiguous: name the
		// candidates instead of guessing (the picker groups by provider for the
		// same reason).
		fmt.Fprintf(stderr, "keel model show: %q matches %d models; qualify with the provider:\n", positional[0], len(matches))
		for _, model := range matches {
			fmt.Fprintf(stderr, "  %s/%s\n", model.ProviderID, model.ModelID)
		}
		return 2
	}
}

// matchModels resolves the show argument: "provider/model" is exact, a bare id
// matches every provider that carries it.
func matchModels(models []modelcfg.ModelView, query string) []modelcfg.ModelView {
	if provider, modelID, ok := strings.Cut(query, "/"); ok {
		for _, model := range models {
			if model.ProviderID == provider && model.ModelID == modelID {
				return []modelcfg.ModelView{model}
			}
		}
		return nil
	}
	out := make([]modelcfg.ModelView, 0, 1)
	for _, model := range models {
		if model.ModelID == query {
			out = append(out, model)
		}
	}
	return out
}

// --- catalog -----------------------------------------------------------------

// catalogBody is the resolved catalog answer: the server's own model views.
// source is always "resolved" — the projection was assembled from the tenant
// document plus the built-in catalog, not fetched from an upstream.
type catalogBody struct {
	ProviderID string               `json:"providerId,omitempty"`
	Source     string               `json:"source"`
	Models     []modelcfg.ModelView `json:"models"`
	Issues     []modelcfg.Issue     `json:"issues,omitempty"`
}

// catalogRefreshBody is the live answer of `--refresh`: the upstream model ids
// the probe discovered, plus the measured latency. It is deliberately not the
// resolved view: refresh exists to see the provider's current lineup.
type catalogRefreshBody struct {
	ProviderID string   `json:"providerId"`
	Source     string   `json:"source"`
	LatencyMS  int64    `json:"latencyMs"`
	Models     []string `json:"models"`
}

// runModelCatalog prints one provider's model catalog. Without --refresh it
// reads the resolved projection (the same facts the settings page and the
// picker read); with --refresh it asks the server to re-fetch the provider's
// live model list and prints that. A provider id is required for --refresh:
// refreshing every provider at once would multiply upstream calls the operator
// did not ask for.
func runModelCatalog(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model catalog", stderr)
	server, asJSON := commonFlags(fs)
	refresh := fs.Bool("refresh", false, "re-fetch the provider's live model list from upstream")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintln(stderr, "usage: keel model catalog [<providerId>] [--refresh]")
		return 2
	}
	providerID := ""
	if len(positional) == 1 {
		providerID = strings.TrimSpace(positional[0])
	}
	if *refresh && providerID == "" {
		fmt.Fprintln(stderr, "keel model catalog: --refresh needs a provider id (one upstream request per provider)")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	if *refresh {
		var probe modelcfg.ProviderProbe
		if err := client.do(http.MethodPost, "/v1/providers/"+url.PathEscape(providerID)+"/test", "", nil, &probe); err != nil {
			return reportError(stderr, err)
		}
		if !probe.OK {
			printProviderProbe(stderr, probe)
			return 1
		}
		answer := catalogRefreshBody{
			ProviderID: probe.ProviderID,
			Source:     "upstream",
			LatencyMS:  probe.LatencyMS,
			Models:     probe.Models,
		}
		if *asJSON {
			if err := printJSON(stdout, answer); err != nil {
				return reportError(stderr, err)
			}
			return 0
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PROVIDER/MODEL\tSOURCE")
		for _, modelID := range answer.Models {
			fmt.Fprintf(tw, "%s/%s\tupstream\n", answer.ProviderID, modelID)
		}
		_ = tw.Flush()
		return 0
	}

	var providers providerListBody
	if err := client.do(http.MethodGet, "/v1/providers", "", nil, &providers); err != nil {
		return reportError(stderr, err)
	}
	answer := catalogBody{ProviderID: providerID, Source: "resolved", Models: make([]modelcfg.ModelView, 0)}
	if providerID == "" {
		for _, view := range providers.Providers {
			answer.Models = append(answer.Models, view.Models...)
		}
	} else {
		found := false
		for _, view := range providers.Providers {
			if view.ProviderID != providerID {
				continue
			}
			found = true
			answer.Models = append(answer.Models, view.Models...)
			answer.Issues = append(answer.Issues, view.Issues...)
			break
		}
		if !found {
			fmt.Fprintf(stderr,
				"keel model catalog: this server does not serve a provider %q (see keel model provider list)\n", providerID)
			return 1
		}
	}
	if *asJSON {
		if err := printJSON(stdout, answer); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	printModelTable(stdout, answer.Models)
	return 0
}

// --- rendering ----------------------------------------------------------------

func printProviderTable(w io.Writer, views []modelcfg.ProviderView) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tMODELS\tEXECUTABLE\tSELECTABLE")
	for _, view := range views {
		name := view.Name
		if name == "" {
			name = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			view.ProviderID, name, view.ProviderType, len(view.Models),
			yesNo(view.Executable), yesNo(view.Selectable))
	}
	_ = tw.Flush()
}

func printModelTable(w io.Writer, views []modelcfg.ModelView) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER/MODEL\tTYPE\tSURFACE\tSELECTABLE\tENABLED\tEXECUTABLE")
	for _, view := range views {
		fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\t%s\t%s\n",
			view.ProviderID, view.ModelID, view.ProviderType, view.SurfaceKind,
			yesNo(view.Selectable), yesNo(view.Enabled), yesNo(view.Executable))
	}
	_ = tw.Flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// --- provider management (the document write subset) -------------------------

// settingsModelsResponse is the GET /v1/settings/models body (the fields this
// CLI uses; the document travels whole).
type settingsModelsResponse struct {
	Revision  int64           `json:"revision"`
	Source    string          `json:"source"`
	Document  json.RawMessage `json:"document"`
	Selection json.RawMessage `json:"selection,omitempty"`
}

// settingsModelsWriteResponse is the PUT /v1/settings/models body.
type settingsModelsWriteResponse struct {
	Revision            int64  `json:"revision"`
	Source              string `json:"source"`
	ProvidersExecutable int    `json:"providersExecutable"`
	ModelsSelectable    int    `json:"modelsSelectable"`
}

// settingsDocument is the models document as this CLI edits it: every top-level
// key is preserved verbatim (modelRules, selection, schemaVersion, ...) and only
// providers[] is decoded, so a write never drops configuration the command does
// not manage.
type settingsDocument struct {
	fields map[string]json.RawMessage
}

func parseSettingsDocument(raw json.RawMessage) (*settingsDocument, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, errors.New("the server's models document is not a JSON object")
	}
	return &settingsDocument{fields: fields}, nil
}

// providers decodes providers[] as one map per entry, so unknown keys inside an
// entry survive an edit.
func (d *settingsDocument) providers() ([]map[string]any, error) {
	raw, ok := d.fields["providers"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []map[string]any{}, nil
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("providers is not an array of objects: %w", err)
	}
	return entries, nil
}

func (d *settingsDocument) setProviders(entries []map[string]any) error {
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	d.fields["providers"] = raw
	return nil
}

func (d *settingsDocument) encode() (json.RawMessage, error) {
	return json.Marshal(d.fields)
}

// providerFlags are the editable entry fields both add and update accept.
type providerFlags struct {
	name       string
	family     string
	apiType    string
	baseURL    string
	env        string
	ref        string
	visibility string
	models     stringList
	headers    stringList
}

func registerProviderFlags(fs *flag.FlagSet, values *providerFlags) {
	fs.StringVar(&values.name, "name", "", "display name")
	fs.StringVar(&values.family, "type", "", "provider family (llm, image, tts, ...; plugin-declared, open vocabulary)")
	fs.StringVar(&values.apiType, "api-type", "", "wire API (openai-chat-completions, openai-responses, anthropic-messages)")
	fs.StringVar(&values.baseURL, "base-url", "", "upstream base URL")
	fs.StringVar(&values.env, "env", "", "credential env var NAME (a reference, never a value)")
	fs.StringVar(&values.ref, "ref", "", "tenant secret reference (secret:NAME)")
	fs.Var(&values.models, "model", "model id this provider serves (repeatable; replaces the list)")
	fs.Var(&values.headers, "header", "static request header Key:Value (repeatable; replaces the set)")
	fs.StringVar(&values.visibility, "visibility", "", "visible | hidden")
}

// apply overlays the flags the user actually passed onto one provider entry.
// A flag set to the empty string clears the field (the document's own
// vocabulary); a flag not passed leaves it untouched, which is what makes
// update a read-modify-write instead of a full replace.
func (values providerFlags) apply(entry map[string]any, set map[string]bool) error {
	if set["env"] && set["ref"] {
		return errors.New("--env and --ref name the same credential; pass exactly one")
	}
	if set["name"] {
		setOrClear(entry, "name", strings.TrimSpace(values.name))
	}
	if set["type"] {
		setOrClear(entry, "type", strings.TrimSpace(values.family))
	}
	if set["visibility"] {
		setOrClear(entry, "visibility", strings.TrimSpace(values.visibility))
	}
	if set["api-type"] || set["base-url"] || set["header"] {
		api, _ := entry["api"].(map[string]any)
		if api == nil {
			api = map[string]any{}
		}
		if set["api-type"] {
			setOrClear(api, "type", strings.TrimSpace(values.apiType))
		}
		if set["base-url"] {
			setOrClear(api, "baseUrl", strings.TrimSpace(values.baseURL))
		}
		if set["header"] {
			headers := map[string]any{}
			for _, pair := range values.headers {
				key, value, ok := strings.Cut(pair, ":")
				if !ok || strings.TrimSpace(key) == "" {
					return fmt.Errorf("--header %q is not Key:Value", pair)
				}
				headers[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
			if len(headers) == 0 {
				delete(api, "headers")
			} else {
				api["headers"] = headers
			}
		}
		if len(api) == 0 {
			delete(entry, "api")
		} else {
			entry["api"] = api
		}
	}
	if set["env"] || set["ref"] {
		access := map[string]any{"type": "api-key"}
		if strings.TrimSpace(values.env) != "" {
			access["env"] = strings.TrimSpace(values.env)
		}
		if strings.TrimSpace(values.ref) != "" {
			access["ref"] = strings.TrimSpace(values.ref)
		}
		if len(access) == 1 {
			// Both halves empty: the credential reference is cleared.
			delete(entry, "access")
		} else {
			entry["access"] = access
		}
	}
	if set["model"] {
		list := make([]any, 0, len(values.models))
		for _, id := range values.models {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				list = append(list, trimmed)
			}
		}
		if len(list) == 0 {
			delete(entry, "modelIds")
		} else {
			entry["modelIds"] = list
		}
	}
	return nil
}

// setOrClear writes a string field, or drops it when the value is empty: the
// document's vocabulary has no empty strings, only absent fields.
func setOrClear(target map[string]any, key, value string) {
	if value == "" {
		delete(target, key)
		return
	}
	target[key] = value
}

func runModelProviderCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		modelProviderUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runModelProviderList(args[1:], stdout, stderr)
	case "show":
		return runModelProviderShow(args[1:], stdout, stderr)
	case "add":
		return runModelProviderAdd(args[1:], stdout, stderr)
	case "update":
		return runModelProviderUpdate(args[1:], stdout, stderr)
	case "remove":
		return runModelProviderRemove(args[1:], stdout, stderr)
	case "test":
		return runModelProviderTest(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		modelProviderUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel model provider: unknown command %q (list|show|add|update|remove|test)\n\n", args[0])
		modelProviderUsage(stderr)
		return 2
	}
}

func modelProviderUsage(w io.Writer) {
	fmt.Fprint(w, `keel model provider — read the provider registry, and manage its document entries

Usage:
  keel model provider list [--json]
  keel model provider show <id>
  keel model provider add <id> [flags]
  keel model provider update <id> [flags]
  keel model provider remove <id> [--repoint-selection <providerId>/<modelId>]
  keel model provider test <id> [--json]

Read the registry with `+"`keel model provider list`"+` and `+"`keel model list`"+`;
read one provider's model catalog with `+"`keel model catalog <id>`"+`.

Flags: keel model --help lists the provider flags (--name, --type, --api-type,
--base-url, --env, --ref, --model, --header, --visibility), plus --server and
--json. The document is written with the revision guard, and the server is the
only authority on what the resulting configuration may be: a rejected write
prints the server's code and field-level issues.

remove refuses a provider an agent or a team still binds (the refusal lists the
referrers): repoint those bindings, or disable the provider instead. When the
document's default selection names the provider being removed,
--repoint-selection must name its replacement — the command never guesses one.
test sends one minimal model-list request with the provider's resolved
credential and reports the measured health: status, latency, live model count
and issues. Without a resolvable credential it fails as needs_auth without
sending anything; it never reports a success it did not observe.
`)
}

// --- add ---------------------------------------------------------------------

func runModelProviderAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model provider add", stderr)
	server, asJSON := commonFlags(fs)
	var values providerFlags
	registerProviderFlags(fs, &values)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "usage: keel model provider add <id> [flags]")
		return 2
	}
	providerID := strings.TrimSpace(positional[0])
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	document, revision, err := readSettingsDocument(client)
	if err != nil {
		return reportError(stderr, err)
	}
	entries, err := document.providers()
	if err != nil {
		return reportError(stderr, err)
	}
	for _, entry := range entries {
		if strings.TrimSpace(stringOf(entry["id"])) == providerID {
			fmt.Fprintf(stderr,
				"keel model provider add: provider %q is already declared by the models document; use update\n", providerID)
			return 1
		}
	}
	entry := map[string]any{"id": providerID}
	if err := values.apply(entry, visitSet(fs)); err != nil {
		return reportError(stderr, err)
	}
	entries = append(entries, entry)
	if err := document.setProviders(entries); err != nil {
		return reportError(stderr, err)
	}
	write, err := writeSettingsDocument(client, document, revision)
	if err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, write); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "added provider %s (revision %d; %d providers executable, %d models selectable)\n",
		providerID, write.Revision, write.ProvidersExecutable, write.ModelsSelectable)
	return 0
}

// --- update ------------------------------------------------------------------

func runModelProviderUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model provider update", stderr)
	server, asJSON := commonFlags(fs)
	var values providerFlags
	registerProviderFlags(fs, &values)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "usage: keel model provider update <id> [flags]")
		return 2
	}
	providerID := strings.TrimSpace(positional[0])
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	document, revision, err := readSettingsDocument(client)
	if err != nil {
		return reportError(stderr, err)
	}
	entries, err := document.providers()
	if err != nil {
		return reportError(stderr, err)
	}
	index := -1
	for i, entry := range entries {
		if strings.TrimSpace(stringOf(entry["id"])) == providerID {
			index = i
			break
		}
	}
	if index < 0 {
		// The entry may still exist in the built-in catalog: the document is a
		// patch layer, so editing one starts a patch entry of the same id
		// (the documented incremental-adoption path) instead of failing.
		entries = append(entries, map[string]any{"id": providerID})
		index = len(entries) - 1
	}
	if err := values.apply(entries[index], visitSet(fs)); err != nil {
		return reportError(stderr, err)
	}
	if err := document.setProviders(entries); err != nil {
		return reportError(stderr, err)
	}
	write, err := writeSettingsDocument(client, document, revision)
	if err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, write); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "updated provider %s (revision %d; %d providers executable, %d models selectable)\n",
		providerID, write.Revision, write.ProvidersExecutable, write.ModelsSelectable)
	return 0
}

// --- remove ------------------------------------------------------------------

func runModelProviderRemove(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model provider remove", stderr)
	server, asJSON := commonFlags(fs)
	repoint := fs.String("repoint-selection", "",
		"replace the document's default selection with <providerId>/<modelId> (required when it names the removed provider)")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "usage: keel model provider remove <id>")
		return 2
	}
	providerID := strings.TrimSpace(positional[0])
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	document, revision, err := readSettingsDocument(client)
	if err != nil {
		return reportError(stderr, err)
	}
	entries, err := document.providers()
	if err != nil {
		return reportError(stderr, err)
	}
	kept := make([]map[string]any, 0, len(entries))
	found := false
	for _, entry := range entries {
		if strings.TrimSpace(stringOf(entry["id"])) == providerID {
			found = true
			continue
		}
		kept = append(kept, entry)
	}
	if !found {
		fmt.Fprintf(stderr,
			"keel model provider remove: the models document does not declare provider %q; a provider shipped by the built-in catalog cannot be removed this way (hide it with --visibility hidden)\n",
			providerID)
		return 1
	}
	// A document whose default selection names the removed provider cannot be
	// valid without a replacement: fail before writing rather than let the
	// server report an unresolvable selection the caller has no way to fix from
	// this command. The replacement is stated by the caller, never guessed.
	selectionProvider, _ := documentSelection(document)
	if repointSelection := strings.TrimSpace(*repoint); repointSelection != "" {
		replacementProvider, replacementModel, ok := strings.Cut(repointSelection, "/")
		if !ok || strings.TrimSpace(replacementProvider) == "" || strings.TrimSpace(replacementModel) == "" {
			fmt.Fprintln(stderr, "keel model provider remove: --repoint-selection needs <providerId>/<modelId>")
			return 2
		}
		if err := document.setSelection(replacementProvider, replacementModel); err != nil {
			return reportError(stderr, err)
		}
	} else if selectionProvider == providerID {
		fmt.Fprintf(stderr,
			"keel model provider remove: the models document's default selection names provider %q; pass --repoint-selection <providerId>/<modelId> to move it (repointing it first is the only way to keep the document valid)\n",
			providerID)
		return 2
	}
	if err := document.setProviders(kept); err != nil {
		return reportError(stderr, err)
	}
	write, err := writeSettingsDocument(client, document, revision)
	if err != nil {
		return reportError(stderr, err)
	}
	// The write landing does not prove the provider is gone: the built-in
	// catalog may still serve the id. Re-read the projection and say so, rather
	// than claiming a removal that did not happen.
	stillServed, err := providerStillServed(client, providerID)
	if err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, map[string]any{
			"removed": providerID, "revision": write.Revision, "stillServedByCatalog": stillServed,
		}); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "removed provider %s from the models document (revision %d)\n", providerID, write.Revision)
	if stillServed {
		fmt.Fprintf(stdout,
			"note: %s is still served — the built-in catalog ships it, so this entry's values no longer apply to it\n",
			providerID)
	}
	return 0
}

// providerStillServed reports whether the resolved projection still carries the
// provider id (the built-in catalog case).
func providerStillServed(client *cliClient, providerID string) (bool, error) {
	var body providerListBody
	if err := client.do(http.MethodGet, "/v1/providers", "", nil, &body); err != nil {
		return false, err
	}
	for _, view := range body.Providers {
		if view.ProviderID == providerID {
			return true, nil
		}
	}
	return false, nil
}

// --- test --------------------------------------------------------------------

// runModelProviderTest asks the server to probe one provider for real: one
// minimal upstream model-list request through the server's own credential
// resolution. The CLI never receives the credential, so the request body is
// empty and the answer is a measured state (ready|needs_auth|unreachable|
// invalid_config), never a guess.
func runModelProviderTest(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel model provider test", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "usage: keel model provider test <id>")
		return 2
	}
	providerID := strings.TrimSpace(positional[0])
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var probe modelcfg.ProviderProbe
	if err := client.do(http.MethodPost, "/v1/providers/"+url.PathEscape(providerID)+"/test", "", nil, &probe); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, probe); err != nil {
			return reportError(stderr, err)
		}
		if probe.OK {
			return 0
		}
		return 1
	}
	printProviderProbe(stdout, probe)
	if probe.OK {
		return 0
	}
	return 1
}

func printProviderProbe(w io.Writer, probe modelcfg.ProviderProbe) {
	endpoint := probe.Endpoint
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "—"
	}
	fmt.Fprintf(w, "provider %s (%s)\n", probe.ProviderID, probe.ProviderType)
	fmt.Fprintf(w, "endpoint %s · api %s\n", endpoint, probe.APIType)
	fmt.Fprintf(w, "status %s", probe.Status)
	requestSent := probe.APIType != modelcfg.APITypeDemo &&
		(probe.Status == modelcfg.ProbeStatusReady || probe.Status == modelcfg.ProbeStatusUnreachable)
	if probe.LatencyMS > 0 || requestSent {
		fmt.Fprintf(w, " · latency %dms", probe.LatencyMS)
	}
	if probe.ModelCount > 0 {
		fmt.Fprintf(w, " · models %d", probe.ModelCount)
	}
	fmt.Fprintln(w)
	if len(probe.Issues) > 0 {
		fmt.Fprintln(w, "issues")
		for _, issue := range probe.Issues {
			if issue.Field == "" {
				fmt.Fprintf(w, "  %s\n", issue.Message)
				continue
			}
			fmt.Fprintf(w, "  %s: %s\n", issue.Field, issue.Message)
		}
	}
	if probe.Error != "" {
		fmt.Fprintf(w, "error: %s\n", probe.Error)
	}
	if probe.OK {
		if probe.APIType == modelcfg.APITypeDemo {
			fmt.Fprintln(w, "ok: the in-process demo provider answers without an upstream request")
			return
		}
		fmt.Fprintln(w, "ok: the provider answered a model-list request")
		return
	}
	fmt.Fprintln(w, "not ok: the provider did not answer a model-list request")
}

// --- settings document plumbing ----------------------------------------------

// readSettingsDocument fetches the models document and its revision: the input
// of a read-modify-write.
func readSettingsDocument(client *cliClient) (*settingsDocument, int64, error) {
	var body settingsModelsResponse
	if err := client.do(http.MethodGet, "/v1/settings/models", "", nil, &body); err != nil {
		return nil, 0, err
	}
	document, err := parseSettingsDocument(body.Document)
	if err != nil {
		return nil, 0, err
	}
	return document, body.Revision, nil
}

// writeSettingsDocument PUTs the document under the revision it was read at
// (the store's optimistic-concurrency guard: a concurrent writer's work is
// never clobbered silently).
func writeSettingsDocument(client *cliClient, document *settingsDocument, revision int64) (settingsModelsWriteResponse, error) {
	encoded, err := document.encode()
	if err != nil {
		return settingsModelsWriteResponse{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return settingsModelsWriteResponse{}, err
	}
	rawRevision, err := json.Marshal(revision)
	if err != nil {
		return settingsModelsWriteResponse{}, err
	}
	fields["expectedRevision"] = rawRevision
	body, err := json.Marshal(fields)
	if err != nil {
		return settingsModelsWriteResponse{}, err
	}
	var write settingsModelsWriteResponse
	if err := client.do(http.MethodPut, "/v1/settings/models", "", json.RawMessage(body), &write); err != nil {
		return settingsModelsWriteResponse{}, err
	}
	return write, nil
}

// documentSelection reads the document's default selection halves ("" when the
// document names none).
func documentSelection(d *settingsDocument) (providerID, modelID string) {
	raw, ok := d.fields["selection"]
	if !ok {
		return "", ""
	}
	var selection struct {
		ProviderID string `json:"providerId"`
		ModelID    string `json:"modelId"`
	}
	if err := json.Unmarshal(raw, &selection); err != nil {
		return "", ""
	}
	return strings.TrimSpace(selection.ProviderID), strings.TrimSpace(selection.ModelID)
}

// setSelection replaces the document's default selection with one pair. The
// reasoning level is deliberately not carried over: a level that belonged to
// the removed model has no meaning for its replacement, and the server would
// refuse it; the new model's own default applies.
func (d *settingsDocument) setSelection(providerID, modelID string) error {
	raw, err := json.Marshal(map[string]string{
		"providerId": strings.TrimSpace(providerID),
		"modelId":    strings.TrimSpace(modelID),
	})
	if err != nil {
		return err
	}
	d.fields["selection"] = raw
	return nil
}

// stringOf reads a string field of a decoded provider entry.
func stringOf(value any) string {
	text, _ := value.(string)
	return text
}
