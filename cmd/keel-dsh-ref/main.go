// Command keel-dsh-ref is the reference external sidecar: a minimal
// AgentBackend / ToolProvider implementation used by the runtime's external
// gateway smoke test and by the wiring's automated tests.
//
// It is not the real DSH runtime. The real sidecar is a Node process pinned to
// @deepseek-ai/dsh (see docs/DSH-REFERENCE.md); this command exists so the
// protocol wiring, process lifecycle, cancellation and reaping can be
// exercised without Node.
//
// Usage:
//
//	keel-dsh-ref -kind=agent -usage
//	keel-dsh-ref -kind=tool
package main

import (
	"flag"
	"strings"

	"github.com/jixoai/keel/internal/dshref"
)

func main() {
	var (
		kind            = flag.String("kind", "agent", "provider kind: agent|tool")
		name            = flag.String("name", dshref.DefaultName, "provider name reported in initialize")
		version         = flag.String("version", dshref.DefaultVersion, "provider version reported in initialize")
		protocolVersion = flag.String("protocol-version", "", "answer this protocol revision instead of the current one (version-mismatch tests)")
		usage           = flag.Bool("usage", false, "declare and report token usage (metering=external)")
		metering        = flag.String("metering", "", "override the declared metering mark (external|none)")
		slow            = flag.Duration("slow", 0, "delay each streamed chunk")
		ignoreCancel    = flag.Bool("ignore-cancel", false, "answer cancel without stopping the turn")
		crashOnSend     = flag.Bool("crash-on-send", false, "die on message.send (crash isolation tests)")
		crashAfter      = flag.Int("crash-after", 0, "die after this many emitted events (0 = never)")
		prompt          = flag.String("prompt", "", "prefix for the echoed answer")
		forbidEnv       = flag.String("forbid-env", "", "comma-separated environment variables whose presence aborts startup")
		requireEnv      = flag.String("require-env", "", "comma-separated environment variables that must be present")
		handshakeDelay  = flag.Duration("handshake-delay", 0, "delay the initialize answer (stuck-provider tests)")
		openDelay       = flag.Duration("open-delay", 0, "delay the session.open answer (stuck-provider tests)")
		openLog         = flag.String("open-log", "", "append one line per created session to this file (test support)")
		spawnChild      = flag.Bool("spawn-child", false, "spawn a long-lived grandchild (process-group reaping test)")
	)
	flag.Parse()
	cfg := dshref.Config{
		Kind:             *kind,
		Name:             *name,
		Version:          *version,
		ProtocolVersion:  *protocolVersion,
		Usage:            *usage,
		Metering:         *metering,
		Slow:             *slow,
		IgnoreCancel:     *ignoreCancel,
		CrashOnSend:      *crashOnSend,
		CrashAfterEvents: *crashAfter,
		Prompt:           *prompt,
		ForbiddenEnv:     splitList(*forbidEnv),
		RequiredEnv:      splitList(*requireEnv),
		HandshakeDelay:   *handshakeDelay,
		OpenDelay:        *openDelay,
		OpenLog:          *openLog,
		SpawnChild:       *spawnChild,
	}
	if cfg.Slow < 0 {
		cfg.Slow = 0
	}
	dshref.ServeCommand(cfg)
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
