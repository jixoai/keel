// `keel model ...`: the read-only CLI of the model registry — the keel.model
// plugin's declared command surface (provides.cli in
// internal/pluginpkg/builtin/keel.model.yaml).
//
// It is a thin HTTP client of a running `keel serve`, exactly like `keel agent`
// and `keel team`: GET /v1/providers and GET /v1/models are the same resolved
// projections the settings page and the session picker read, so the three
// surfaces cannot drift apart. Nothing here opens the database or re-implements
// a resolution rule.
//
// The declared capability tree and this file are one unit: a command declared
// in the manifest must exist here, and a command added here must be declared
// there (TestCapabilityPluginsDeclareTheirCLI guards one direction,
// cmd/keel/model_test.go exercises every declared command against a stub
// server).
package main

import (
	"fmt"
	"io"
	"net/http"
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
	case "list":
		return runModelList(args[1:], stdout, stderr)
	case "show":
		return runModelShow(args[1:], stdout, stderr)
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
	fmt.Fprint(w, `keel model — read the model registry over a running keel serve

Usage:
  keel model providers [--json]
  keel model list [--json]
  keel model show <providerId>/<modelId>

Commands:
  providers   every configured provider: family, executable state, model count
  list        the flat model registry: family, session panel, selectability
  show        one model by "provider/model" (a bare model id must be unique)

Common flags:
  --server <addr>           keel serve address (default $KEEL_SERVE_ADDR or 127.0.0.1:13750)
  --json                    print raw JSON instead of a table

This surface is read-only by declaration: it never writes configuration.
The settings page is the document's only writer.
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
