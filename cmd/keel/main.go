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
	"strings"
	"time"

	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/logsafe"
)

// version is the keel product version. It is deliberately separate from the
// HarnessAPI contract version (harnessapi.APIVersion): a UI pins /v1 and asks
// /v1/capabilities, never a binary version.
var version = "0.6.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runDefaultCmd(stdout, stderr)
	}
	// The client commands accept --username/--password-file before any
	// subcommand flag parsing; the values live for this invocation only (a
	// second run in the same process, as tests do, starts clean).
	switch args[0] {
	case "bot", "team-bot", "model", "skills":
		cliAuthUsername, cliAuthPasswordFile = "", ""
		rest, err := extractAuthFlags(args[1:])
		if err != nil {
			fmt.Fprintf(stderr, "keel %s: %v\n", args[0], err)
			return 2
		}
		args = append([]string{args[0]}, rest...)
	}
	switch args[0] {
	case "serve":
		return runServeCmd(args[1:], stdout, stderr)
	case "start":
		return runStartCmd(args[1:], stdout, stderr)
	case "install":
		return runInstallCmd(args[1:], stdout, stderr)
	case "uninstall":
		return runInstallCmd(append([]string{"--uninstall"}, args[1:]...), stdout, stderr)
	case "tui":
		return runTUICmd(args[1:], stdout, stderr)
	case "bot":
		return runBotCmd(args[1:], stdout, stderr)
	case "team-bot":
		return runTeamBotCmd(args[1:], stdout, stderr)
	case "model":
		return runModelCmd(args[1:], stdout, stderr)
	case "skills":
		return runSkillsCmd(args[1:], stdout, stderr)
	case "admin":
		return runAdminCmd(args[1:], stdout, stderr)
	case "db":
		return runDBCmd(args[1:], stdout, stderr)
	case "update":
		return runUpdateCmd(args[1:], stdout, stderr)
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
	fmt.Fprint(w, `keel — the Bot product shell over the Keel runtime

Usage:
  keel start [flags]     One-command local product: bundled PostgreSQL + web, opens the browser
  keel install [flags]   Register this binary as the platform app (macOS .app; no notarization)
  keel uninstall         Remove the installed app (keeps data unless --purge)
  keel serve [flags]     Host the Harness API (/v1), the web UI and SSE on one origin
  keel tui [flags]       Run the NSH TUI client against a running keel serve
  keel bot <cmd>         Manage Bots (list|show|create|update|archive|start|keys|key-create|key-revoke)
  keel team-bot <cmd>    Manage TeamBots (list|show|create|update|archive|roster|start)
  keel model <cmd>       Read/manage the model registry (providers|list|show|catalog|provider ...)
  keel skills <cmd>      Manage the tenant skill library (list|show|install|update|pin|unpin|remove|sources)
  keel admin <cmd>       Provision tenants, API keys and quotas (operator)
  keel db <cmd>          Manage the local development PostgreSQL 18 cluster
                         (start|stop|status|dsn|env|reset)
  keel update [flags]    Self-update from GitHub releases (--check|--force|--manifest)
  keel doctor [flags]    Diagnose configuration and build artifacts
  keel version           Print version and build information

Environment (serve):
  KEEL_DSN                 PostgreSQL DSN (required)
  KEEL_SERVE_ADDR          listen address (default 127.0.0.1:13750)
  KEEL_SERVE_TENANT        tenant UUID the server acts as (required with auth=required)
  KEEL_AUTH                required|local-only|off (default required)
  KEEL_INSECURE            allow auth=off (loopback only; never expose it)
  KEEL_SECRET_KEY          32-byte base64/hex key for cookie/CSRF signing (required)
  KEEL_BOOTSTRAP_TOKEN     one-time setup token for a container (auth=required, no super admin)
  KEEL_NO_AUTO_DB          disable auto-init of a local PostgreSQL cluster when KEEL_DSN is unset
  KEEL_SERVE_WEB_DIR       static build directory (default web/dist)
  KEEL_OAR_JOURNAL_DIR     OAR record journal directory (session-<ref>.ndjson);
                           empty answers 501 on the external projection route
  KEEL_MODEL / KEEL_PROVIDER / KEEL_BASE_URL / KEEL_PROTOCOL / KEEL_API_KEY
                           model selection (fake provider when unset)
  KEEL_MODELS_CONFIG       tenant models document (providers/rules/selection)
  KEEL_CONFIG_FILE         config.toml path (default $KEEL_HOME/config.toml)
  KEEL_SKILLS_ROOTS        OS path list of skill roots (PI-compatible)
  KEEL_SKILLS_ROOT         managed skill library root (default $KEEL_HOME/skills)
  KEEL_SKILLS_SOURCE_ROOT  managed subscription root (default $KEEL_SKILLS_ROOT/sources)

Environment (tui, bot, team-bot, model, skills):
  KEEL_TUI_BIN             path to the NSH TUI binary
  KEEL_AGENT_URL           Harness API base URL (default http://127.0.0.1:13750)
  KEEL_ACCOUNT_USERNAME    human account login name (keel bot/team-bot/model/skills)
  KEEL_ACCOUNT_PASSWORD    human account password (or pass --password-file)
  KEEL_AGENT_TOKEN         bot API key (keel_bot_...) for machine clients
  KEEL_SERVE_ADDR          server address for keel bot / keel team-bot

Run "keel bot --help" or "keel team-bot --help" for the full command surface.
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
	fmt.Fprintf(w, "  harness-api: %s (%s)\n", harnessapi.APIVersion, harnessapi.Version)
}

func runServeCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		addr             = fs.String("addr", envOr("KEEL_SERVE_ADDR", defaultServeAddr), "listen address")
		dsn              = fs.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN")
		webDir           = fs.String("web-dir", defaultWebDir(), "static web build directory")
		casDir           = fs.String("cas", os.Getenv("KEEL_CAS_DIR"), "content-addressed store directory (large originals)")
		oarJournalDir    = fs.String("oar-journal-dir", os.Getenv("KEEL_OAR_JOURNAL_DIR"), "OAR record journal directory (session-<ref>.ndjson); empty answers 501 on the external projection route")
		tenantRaw        = fs.String("tenant", os.Getenv("KEEL_SERVE_TENANT"), "tenant UUID the server acts as")
		authRaw          = fs.String("auth", envOr("KEEL_AUTH", string(authModeRequired)), "authentication mode: required|local-only|off")
		insecure         = fs.Bool("insecure", envBool("KEEL_INSECURE"), "allow auth=off (loopback only)")
		runs             = fs.Int("runs", envInt("KEEL_RUNS", 64), "max concurrent agent runs")
		noWorker         = fs.Bool("no-worker", false, "do not start the agent worker (API-only process)")
		noAutoDB         = fs.Bool("no-auto-db", envBool("KEEL_NO_AUTO_DB"), "do not auto-initialize a local PostgreSQL cluster when KEEL_DSN is unset")
		model            = fs.String("model", os.Getenv("KEEL_MODEL"), "model id")
		provider         = fs.String("provider", os.Getenv("KEEL_PROVIDER"), "provider name")
		baseURL          = fs.String("base-url", os.Getenv("KEEL_BASE_URL"), "provider base URL")
		protocol         = fs.String("protocol", os.Getenv("KEEL_PROTOCOL"), "provider protocol")
		apiKey           = fs.String("api-key", os.Getenv("KEEL_API_KEY"), "provider API key")
		prompt           = fs.String("system-prompt", envOr("KEEL_SYSTEM_PROMPT", ""), "system prompt override")
		skills           = fs.String("skills-roots", os.Getenv("KEEL_SKILLS_ROOTS"), "OS path list of skill roots")
		skillsRoot       = fs.String("skills-root", defaultSkillsRoot(), "managed skill library root ($KEEL_HOME/skills)")
		skillsSourceRoot = fs.String("skills-source-root", os.Getenv("KEEL_SKILLS_SOURCE_ROOT"), "managed subscription root (default <skills-root>/sources)")
		loadoutID        = fs.String("loadout-id", envOr("KEEL_LOADOUT_ID", ""), "loadout id")
		loadoutRev       = fs.Int64("loadout-revision", envInt64("KEEL_LOADOUT_REVISION", 1), "loadout revision")
		mcpConfig        = fs.String("mcp", os.Getenv("KEEL_MCP_CONFIG"), "global mcp.json path (empty disables MCP)")
		mcpProject       = fs.String("mcp-project", os.Getenv("KEEL_MCP_PROJECT"), "project mcp.json path")
		// modelsConfig carries the tenant models document (providers/rules/selection).
		modelsConfig = fs.String("models-config", os.Getenv("KEEL_MODELS_CONFIG"), "tenant models document (providers/rules/selection; empty disables provider configuration)")
		configFile   = fs.String("config-file", defaultConfigFile(), "config.toml path (default $KEEL_HOME/config.toml; missing = empty)")
		exampleBot   = fs.String("example-bot-id", os.Getenv("KEEL_EXAMPLE_BOT_ID"), "Bot handle the example plugin's triggers fire as (empty: the example plugin declares no trigger)")
		exampleInt   = fs.Duration("example-interval", envDuration("KEEL_EXAMPLE_INTERVAL", 0), "also register the example plugin's interval trigger (e.g. 5m)")
		pluginsRoot  = fs.String("plugins-root", defaultPluginsRoot(), "managed plugins directory ($KEEL_HOME/plugins)")
		// subagentMaxDepth is the sub-agent mode's depth setting: -1 unlimited,
		// 0 forbidden, positive a concrete cap. Empty keeps the 3-level default.
		subagentDepthRaw = fs.String("subagent-max-depth", os.Getenv("KEEL_SUBAGENT_MAX_DEPTH"), "sub-agent max depth: -1 unlimited, 0 forbidden, positive cap (default 3)")
		// subagentChildrenRaw is the active direct-children cap per parent.
		subagentChildrenRaw = fs.String("subagent-max-children", os.Getenv("KEEL_SUBAGENT_MAX_CHILDREN"), "sub-agent max active direct children per parent (default 8)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	subagentMaxDepth, err := parseSubagentMaxDepth(*subagentDepthRaw)
	if err != nil {
		fmt.Fprintf(stderr, "keel serve: %v\n", err)
		return 2
	}
	subagentMaxChildren, err := parseSubagentMaxChildren(*subagentChildrenRaw)
	if err != nil {
		fmt.Fprintf(stderr, "keel serve: %v\n", err)
		return 2
	}
	tenantID, err := parseTenant(*tenantRaw)
	if err != nil {
		fmt.Fprintf(stderr, "keel serve: %v\n", err)
		return 2
	}
	mode, err := parseAuthMode(*authRaw)
	if err != nil {
		fmt.Fprintf(stderr, "keel serve: %v\n", err)
		return 2
	}
	dsnValue := strings.TrimSpace(*dsn)
	if dsnValue == "" {
		if *noAutoDB {
			fmt.Fprintln(stderr, "keel serve: KEEL_DSN (or --dsn) is required; --no-auto-db/KEEL_NO_AUTO_DB disables the local development cluster")
			return 2
		}
		autoDSN, dir, err := ensureAutoDevDB(stderr)
		if err != nil {
			fmt.Fprintf(stderr, "keel serve: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "keel serve: no KEEL_DSN, using the local development database at %s\n", dir)
		dsnValue = autoDSN
	}
	log := slog.New(logsafe.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	opts := serveOptions{
		Addr:                *addr,
		DSN:                 dsnValue,
		WebDir:              *webDir,
		CASDir:              *casDir,
		OARJournalDir:       *oarJournalDir,
		TenantID:            tenantID,
		AuthMode:            mode,
		Insecure:            *insecure,
		Runs:                *runs,
		NoWorker:            *noWorker,
		Model:               *model,
		Provider:            *provider,
		BaseURL:             *baseURL,
		Protocol:            *protocol,
		APIKey:              *apiKey,
		SystemPrompt:        *prompt,
		SkillsRoots:         splitPaths(*skills),
		SkillsRoot:          *skillsRoot,
		SkillsSourceRoot:    *skillsSourceRoot,
		LoadoutID:           *loadoutID,
		LoadoutRevision:     *loadoutRev,
		MCPConfig:           *mcpConfig,
		MCPProject:          *mcpProject,
		ModelsConfig:        *modelsConfig,
		ConfigFile:          *configFile,
		PluginsRoot:         *pluginsRoot,
		ExampleBotID:        strings.TrimSpace(*exampleBot),
		ExampleInterval:     *exampleInt,
		SubagentMaxDepth:    subagentMaxDepth,
		SubagentMaxChildren: subagentMaxChildren,
		Log:                 log,
	}
	if err := runServe(context.Background(), opts); err != nil {
		log.Error("keel serve failed", "err", err)
		return 1
	}
	return 0
}

// defaultConfigFile resolves the config.toml path: KEEL_CONFIG_FILE wins, then
// $KEEL_HOME/config.toml, then ~/.keel/config.toml. The file is optional: a
// missing path is an empty configuration, while an invalid file fails startup.
func defaultConfigFile() string {
	if path := strings.TrimSpace(os.Getenv("KEEL_CONFIG_FILE")); path != "" {
		return path
	}
	home := os.Getenv("KEEL_HOME")
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(dir, ".keel")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "config.toml")
}

// defaultPluginsRoot resolves the managed plugins root: KEEL_PLUGINS_ROOT wins,
// then $KEEL_HOME/plugins, then ~/.keel/plugins. The root is where installed
// packages live (one directory per plugin id); it is created on first install.
func defaultPluginsRoot() string {
	if root := os.Getenv("KEEL_PLUGINS_ROOT"); root != "" {
		return root
	}
	home := os.Getenv("KEEL_HOME")
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(dir, ".keel")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "plugins")
}

// defaultSkillsRoot resolves the managed skill library root: KEEL_SKILLS_ROOT
// wins, then $KEEL_HOME/skills, then ~/.keel/skills. Each tenant's installed
// skills live below <root>/<tenant>/active, which is a skill discovery root.
func defaultSkillsRoot() string {
	if root := os.Getenv("KEEL_SKILLS_ROOT"); root != "" {
		return root
	}
	home := os.Getenv("KEEL_HOME")
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(dir, ".keel")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "skills")
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

// parseSubagentMaxDepth parses the mode's depth setting. Empty keeps the
// 3-level default (nil); -1 is unlimited, 0 forbids spawning, and a positive
// value is the concrete cap. Anything below -1 is rejected at boot instead of
// being silently clamped.
func parseSubagentMaxDepth(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("subagent-max-depth must be an integer >= -1")
	}
	if value < -1 {
		return nil, fmt.Errorf("subagent-max-depth must be an integer >= -1")
	}
	return &value, nil
}

// parseSubagentMaxChildren parses the direct-children setting. Empty keeps the
// mode default (nil); a value below 1 is rejected at boot.
func parseSubagentMaxChildren(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return nil, fmt.Errorf("subagent-max-children must be a positive integer")
	}
	return &value, nil
}
