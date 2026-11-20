//go:build keelperf

// Test-only pprof seam for the G5 acceptance harness (scripts/mcp-apps-perf.sh).
//
// It is compiled ONLY with `-tags keelperf`; a normal `go build ./cmd/keel`
// does not contain this file and pays nothing. The listener binds the address
// in KEEL_PPROF_ADDR and is meant to be loopback-only inside the disposable
// perf container; the harness uses it to capture in-use heap profiles at the
// 100-session, 1000-session and post-generation checkpoints.
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"
)

func init() {
	addr := strings.TrimSpace(os.Getenv("KEEL_PPROF_ADDR"))
	if addr == "" {
		return
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		fmt.Fprintf(os.Stderr, "keelperf: pprof listening on %s\n", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "keelperf: pprof server: %v\n", err)
		}
	}()
}
