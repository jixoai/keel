package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/logsafe"
)

// version is the keel product version. It is deliberately separate from the
// AgentAPI contract version (agentapi.APIVersion): a UI pins /v1 and asks
// /v1/capabilities, never a binary version.
var version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServeCmd(args[1:], stdout, stderr)
	case "tui":
		return runTUICmd(args[1:], stdout, stderr)
	case "doctor":
		return runDoctorCmd(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		printVersion(stdout)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `keel — the agent product shell over the Keel runtime

Usage:
  keel serve [flags]     Host the AgentAPI (/v1), the web UI and SSE on one origin
  keel tui [flags]       Run the NSH TUI client against a running keel serve
  keel doctor [flags]    Diagnose configuration and build artifacts
  keel version           Print version and build information

Environment (serve):
  KEEL_DSN                 PostgreSQL DSN (required)
  KEEL_SERVE_ADDR          listen address (default 127.0.0.1:13750)
  KEEL_SERVE_TENANT        tenant UUID the server acts as (required with auth)
  KEEL_SERVE_PASSWORD      local passphrase; unset disables auth (loopback only)
  KEEL_SERVE_WEB_DIR       static build directory (default web/dist)
  KEEL_MODEL / KEEL_PROVIDER / KEEL_BASE_URL / KEEL_PROTOCOL / KEEL_API_KEY
                           model selection (fake provider when unset)
  KEEL_MODELS_CONFIG       tenant models document (providers/rules/selection)
  KEEL_SKILLS_ROOTS        OS path list of skill roots (PI-compatible)

Environment (tui):
  KEEL_TUI_BIN             path to the NSH TUI binary
  KEEL_AGENT_URL           AgentAPI base URL (default http://127.0.0.1:13750)
  KEEL_AGENT_TOKEN         bearer token (the serve passphrase)
`)
}

func printVersion(w io.Writer) {
	commit, modified := "", false
	if _, revision, dirty := buildInfo(); revision != "" {
		commit, modified = revision, dirty
	}
	fmt.Fprintf(w, "keel %s\n", version)
	if commit == "" {
		fmt.Fprintf(w, "  commit:   unknown (built outside a VCS checkout)\n")
	} else {
		suffix := ""
		if modified {
			suffix = " (dirty)"
		}
		fmt.Fprintf(w, "  commit:   %s%s\n", commit, suffix)
	}
	fmt.Fprintf(w, "  go:       %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "  agent-api: %s (%s)\n", agentapi.APIVersion, agentapi.Version)
}

func runServeCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		addr       = fs.String("addr", envOr("KEEL_SERVE_ADDR", defaultServeAddr), "listen address")
		dsn        = fs.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN")
		webDir     = fs.String("web-dir", envOr("KEEL_SERVE_WEB_DIR", defaultServeWebDir), "static web build directory")
		casDir     = fs.String("cas", os.Getenv("KEEL_CAS_DIR"), "content-addressed store directory (large originals)")
		tenantRaw  = fs.String("tenant", os.Getenv("KEEL_SERVE_TENANT"), "tenant UUID the server acts as")
		password   = fs.String("password", os.Getenv("KEEL_SERVE_PASSWORD"), "local passphrase (empty disables auth)")
		secure     = fs.Bool("cookie-secure", envBool("KEEL_SERVE_COOKIE_SECURE"), "mark the auth cookie Secure (TLS deployments)")
		runs       = fs.Int("runs", envInt("KEEL_RUNS", 64), "max concurrent agent runs")
		noWorker   = fs.Bool("no-worker", false, "do not start the agent worker (API-only process)")
		model      = fs.String("model", os.Getenv("KEEL_MODEL"), "model id")
		provider   = fs.String("provider", os.Getenv("KEEL_PROVIDER"), "provider name")
		baseURL    = fs.String("base-url", os.Getenv("KEEL_BASE_URL"), "provider base URL")
		protocol   = fs.String("protocol", os.Getenv("KEEL_PROTOCOL"), "provider protocol")
		apiKey     = fs.String("api-key", os.Getenv("KEEL_API_KEY"), "provider API key")
		prompt     = fs.String("system-prompt", envOr("KEEL_SYSTEM_PROMPT", ""), "system prompt override")
		skills     = fs.String("skills-roots", os.Getenv("KEEL_SKILLS_ROOTS"), "OS path list of skill roots")
		loadoutID  = fs.String("loadout-id", envOr("KEEL_LOADOUT_ID", ""), "loadout id")
		loadoutRev = fs.Int64("loadout-revision", envInt64("KEEL_LOADOUT_REVISION", 1), "loadout revision")
		mcpConfig  = fs.String("mcp", os.Getenv("KEEL_MCP_CONFIG"), "global mcp.json path (empty disables MCP)")
		mcpProject = fs.String("mcp-project", os.Getenv("KEEL_MCP_PROJECT"), "project mcp.json path")
		// modelsConfig carries the tenant models document (providers/rules/selection).
		modelsConfig = fs.String("models-config", os.Getenv("KEEL_MODELS_CONFIG"), "tenant models document (providers/rules/selection; empty disables provider configuration)")
		exampleInt   = fs.Duration("example-interval", envDuration("KEEL_EXAMPLE_INTERVAL", 0), "also register the example plugin's interval trigger (e.g. 5m)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tenantID, err := parseTenant(*tenantRaw)
	if err != nil {
		fmt.Fprintf(stderr, "keel serve: %v\n", err)
		return 2
	}
	log := slog.New(logsafe.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	opts := serveOptions{
		Addr:            *addr,
		DSN:             *dsn,
		WebDir:          *webDir,
		CASDir:          *casDir,
		TenantID:        tenantID,
		Password:        *password,
		SecureCookies:   *secure,
		Runs:            *runs,
		NoWorker:        *noWorker,
		Model:           *model,
		Provider:        *provider,
		BaseURL:         *baseURL,
		Protocol:        *protocol,
		APIKey:          *apiKey,
		SystemPrompt:    *prompt,
		SkillsRoots:     splitPaths(*skills),
		LoadoutID:       *loadoutID,
		LoadoutRevision: *loadoutRev,
		MCPConfig:       *mcpConfig,
		MCPProject:      *mcpProject,
		ModelsConfig:    *modelsConfig,
		ExampleInterval: *exampleInt,
		Log:             log,
	}
	if err := runServe(context.Background(), opts); err != nil {
		log.Error("keel serve failed", "err", err)
		return 1
	}
	return 0
}

func envOr(name, def string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return def
}

func envInt(name string, def int) int {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if value := os.Getenv(name); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return def
}

func splitPaths(raw string) []string {
	if raw == "" {
		return nil
	}
	return filepath.SplitList(raw)
}
