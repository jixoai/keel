// Command runtime is the Keel agent runtime server: a stateless HTTP/SSE shell
// over PostgreSQL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
	"github.com/jixoai/keel/internal/telemetry"
)

func main() {
	var (
		addr       = flag.String("addr", ":13740", "listen address")
		dsn        = flag.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN")
		casDir     = flag.String("cas", os.Getenv("KEEL_CAS_DIR"), "content-addressed store directory (large originals)")
		budget     = flag.Int64("bandwidth", envInt64("KEEL_BANDWIDTH", 0), "outbound bytes/sec budget (0 = unlimited)")
		streamCap  = flag.Int("streams", envInt("KEEL_STREAMS", 1000), "max concurrent streams")
		batch      = flag.Int("chunk-batch", envInt("KEEL_CHUNK_BATCH", 1), "chunks per PG query (2 = 1Gbps mode)")
		admitRate  = flag.Int("admit-rate", envInt("KEEL_ADMIT_RATE", 200), "new streams per second")
		memLimit   = flag.Int64("memlimit", envInt64("GOMEMLIMIT", 0), "soft memory limit in bytes (0 = Go default)")
		runs       = flag.Int("runs", envInt("KEEL_RUNS", 64), "max concurrent agent runs")
		noWorker   = flag.Bool("no-worker", false, "do not start the agent worker")
		mcpConfig  = flag.String("mcp", os.Getenv("KEEL_MCP_CONFIG"), "global mcp.json path (empty disables MCP)")
		mcpProject = flag.String("mcp-project", os.Getenv("KEEL_MCP_PROJECT"), "project mcp.json path (trusted projects only)")
	)
	flag.Parse()
	if *dsn == "" {
		slog.Error("KEEL_DSN (or -dsn) is required")
		os.Exit(2)
	}
	if *memLimit > 0 {
		debug.SetMemoryLimit(*memLimit)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Config{DSN: *dsn})
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	hub := httpapi.NewHub(st, log)
	go hub.Run(ctx)

	streamFn, providerName, model := agent.FakeStream(), "fake", "fake"
	if m := os.Getenv("KEEL_MODEL"); m != "" {
		p, modelID, err := provider.ResolveProvider(m, os.Getenv("KEEL_BASE_URL"),
			os.Getenv("KEEL_PROTOCOL"), os.Getenv("KEEL_PROVIDER"), os.Getenv)
		if err != nil {
			log.Error("resolve provider", "err", err)
			os.Exit(2)
		}
		streamFn, providerName, model = provider.StreamFnFromProvider(p), p.Name(), modelID
	}
	runner := agent.NewRunner(st, streamFn, providerName, model, os.Getenv("KEEL_API_KEY"), log)

	// Host layer: tenant prompt, skills (progressive disclosure), and tools. The
	// runtime stays stateless: the host is assembled at boot from configuration
	// paths, exactly as pi's coding-agent assembles its prompt and tool loadout.
	sections, err := hostSections()
	if err != nil {
		log.Error("parse KEEL_SYSTEM_SECTIONS", "err", err)
		os.Exit(2)
	}
	// MCP servers: configuration is the shared mcpServers shape; connections and
	// tool adaptation happen once at boot, exactly as pi's host connects them.
	var mcpManager *mcp.Manager
	var mcpTools []agentcore.AgentTool
	if *mcpConfig != "" {
		loaded := mcp.LoadConfigFiles(*mcpConfig, *mcpProject)
		for _, message := range loaded.Errors {
			log.Warn("mcp config", "err", message)
		}
		mcpManager = mcp.Connect(ctx, loaded.Servers, mcp.ConnectOptions{
			ClientName:    "keel",
			ClientVersion: "1.0.0",
			Logger:        log,
		})
		defer mcpManager.Close()
		mcpTools = mcpManager.Tools()
		if section := mcpManager.PromptSection(); section != "" {
			sections = append(sections, host.Section{Name: "mcp_servers", Content: section})
		}
		for _, message := range mcpManager.Errors() {
			log.Error("mcp server", "err", message)
		}
	}
	asm, warnings, err := host.New(host.Config{
		Preamble:        envOr("KEEL_SYSTEM_PROMPT", defaultSystemPrompt),
		Sections:        sections,
		SkillRoots:      skillRoots(),
		FileReadTool:    os.Getenv("KEEL_FILE_READ_TOOL"),
		ExtraTools:      mcpTools,
		IncludeBuiltins: true,
	})
	if err != nil {
		log.Error("assemble host", "err", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		log.Warn("skill discovery", "warning", w.String())
	}
	runner.Assembly = asm
	if os.Getenv("KEEL_TELEMETRY") == "memory" {
		memory := telemetry.NewMemory()
		runner.Telemetry = &agent.RunTelemetry{Context: memory, Metrics: memory}
	}
	log.Info("host assembled", "skills", len(asm.Skills()), "visible", len(asm.VisibleSkills()), "tools", len(asm.Tools().List()))

	if !*noWorker {
		worker := agent.NewWorker(st, runner, *runs, log)
		go worker.Run(ctx)
		log.Info("agent worker started", "provider", providerName, "model", model, "concurrency", *runs)
	}

	metrics := &stream.Metrics{}
	srv := httpapi.New(httpapi.Config{
		Store:     st,
		Hub:       hub,
		Blobs:     blob.NewReader(st, *streamCap, *batch),
		Uploader:  &blob.Uploader{Store: st, CASDir: *casDir, TempDir: os.TempDir()},
		Scheduler: stream.NewScheduler(*streamCap),
		Admitter:  stream.NewAdmitter(*admitRate, *admitRate*2),
		Limiter:   stream.NewLimiter(*budget),
		Metrics:   metrics,
		Logger:    log,
		CASDir:    *casDir,
	})

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		// SSE and file streams must not be cut by a global WriteTimeout; every
		// individual write carries its own deadline instead.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    75 * time.Second,
		MaxHeaderBytes: 16 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			setSendBuffer(c, 64<<10)
			return ctx
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("runtime listening", "addr", *addr, "streams", *streamCap, "batch", *batch,
		"bandwidth", *budget, "pid", os.Getpid())

	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	log.Info("stopped")
}

// setSendBuffer caps the kernel send buffer. Left at the OS autotuned default
// (up to 4MB/connection on Linux), 1000 streams would hold gigabytes of kernel
// memory that GOMEMLIMIT cannot see.
func setSendBuffer(c net.Conn, size int) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetWriteBuffer(size)
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

const defaultSystemPrompt = "You are an AI agent running on the Keel runtime. Use the available tools when they help."

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// skillRoots reads the PI-style skill search path (OS path list).
func skillRoots() []string {
	raw := os.Getenv("KEEL_SKILLS_ROOTS")
	if raw == "" {
		return nil
	}
	return filepath.SplitList(raw)
}

// hostSections parses additional prompt sections as a JSON array of
// {"name": "...", "content": "..."} objects.
func hostSections() ([]host.Section, error) {
	raw := os.Getenv("KEEL_SYSTEM_SECTIONS")
	if raw == "" {
		return nil, nil
	}
	var sections []host.Section
	if err := json.Unmarshal([]byte(raw), &sections); err != nil {
		return nil, err
	}
	return sections, nil
}
