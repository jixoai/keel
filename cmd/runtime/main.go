// Command runtime is the fde2e agent runtime server: a stateless HTTP/SSE shell
// over PostgreSQL.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/jixoai/fde2e-agent-runtime/internal/blob"
	"github.com/jixoai/fde2e-agent-runtime/internal/httpapi"
	"github.com/jixoai/fde2e-agent-runtime/internal/store"
	"github.com/jixoai/fde2e-agent-runtime/internal/stream"
)

func main() {
	var (
		addr      = flag.String("addr", ":13740", "listen address")
		dsn       = flag.String("dsn", os.Getenv("FDE2E_DSN"), "PostgreSQL DSN")
		casDir    = flag.String("cas", os.Getenv("FDE2E_CAS_DIR"), "content-addressed store directory (large originals)")
		budget    = flag.Int64("bandwidth", envInt64("FDE2E_BANDWIDTH", 0), "outbound bytes/sec budget (0 = unlimited)")
		streamCap = flag.Int("streams", envInt("FDE2E_STREAMS", 1000), "max concurrent streams")
		batch     = flag.Int("chunk-batch", envInt("FDE2E_CHUNK_BATCH", 1), "chunks per PG query (2 = 1Gbps mode)")
		admitRate = flag.Int("admit-rate", envInt("FDE2E_ADMIT_RATE", 200), "new streams per second")
		memLimit  = flag.Int64("memlimit", envInt64("GOMEMLIMIT", 0), "soft memory limit in bytes (0 = Go default)")
	)
	flag.Parse()
	if *dsn == "" {
		slog.Error("FDE2E_DSN (or -dsn) is required")
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
