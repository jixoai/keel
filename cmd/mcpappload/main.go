// Command mcpappload is the G5 (scale/chaos) load fixture for the MCP Apps
// surface (change feat-mcp-apps-events, design §5.2).
//
// It has three modes, all driven by scripts/mcp-apps-perf.sh:
//
//	mcpserver  a real Streamable HTTP MCP server (modern 2026-07-28 dialect)
//	           exposing N app resources plus one app-visible tool per app. It
//	           persists its document revision across SIGKILL restarts, can
//	           drop every listen stream on command, and can reject listen
//	           attempts for a while so the host's backoff cap is observable.
//	provider   an OpenAI-compatible chat-completions stub. A model name
//	           containing "slow" sleeps for the configured slow delay; the
//	           rest answer immediately. It lets the runtime run at its run
//	           concurrency without any upstream credential.
//	load       the driver: sessions, runs, bridge traffic, catalogue/heap
//	           snapshots, deliberate over-limit probes and a chaos phase that
//	           SIGKILLs the MCP server and drops listen streams while it
//	           watches the runtime, the catalogue and the metrics.
//
// Nothing here reads or logs a DSN, cookie or API key: the harness runs the
// server with authentication disabled on a loopback-only container network.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mcpappload <mcpserver|provider|load> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "mcpserver":
		err = runMCPServer(os.Args[2:])
	case "provider":
		err = runProvider(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, "usage: mcpappload <mcpserver|provider|load> [flags]")
		return
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpappload:", err)
		os.Exit(1)
	}
}
