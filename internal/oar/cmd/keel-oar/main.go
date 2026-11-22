// Command keel-oar is the OAR sidecar: an agent_backend that speaks the
// external-gateway protocol and reports an out-of-band record stream for one
// external runtime.
//
// Usage:
//
//	keel-oar sidecar -runtime=fake
//	keel-oar sidecar -runtime=codex -codex-path /usr/local/bin/codex -workdir /tmp/project
//
// The host normally spawns it through gateway.SpawnStdio (see internal/oar),
// which gives it its own process group and the documented shutdown ladder.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jixoai/keel/internal/oar"
	"github.com/jixoai/keel/internal/oar/sidecar"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "sidecar" {
		args = args[1:]
	}
	os.Exit(run(args))
}

func run(args []string) int {
	flags := flag.NewFlagSet("keel-oar", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runtime := flags.String("runtime", sidecar.RuntimeFake, "runtime adapter: fake|codex")
	name := flags.String("name", "keel-oar-sidecar", "initialize server name")
	version := flags.String("version", "1.0.0", "initialize server version")
	runtimeVersion := flags.String("runtime-version", "", "underlying runtime version reported in the OAR declaration")
	capabilities := flags.String("capabilities", "", "comma-separated capability override (negative tests)")
	simulated := flags.Bool("simulated", false, "force simulated=true (never claims live_model)")
	usage := flags.Bool("usage", false, "declare and emit token usage (fake driver)")

	codexPath := flags.String("codex-path", "", "codex executable (default: PATH)")
	workdir := flags.String("workdir", "", "default runtime working directory")
	sandbox := flags.String("sandbox", "read-only", "codex sandbox mode")
	model := flags.String("model", "", "codex model override")
	timeout := flags.Duration("timeout", 10*time.Minute, "one codex turn timeout")
	codexArgs := stringList{}
	flags.Var(&codexArgs, "codex-arg", "extra codex exec argument (repeatable)")
	codexPIDFile := flags.String("codex-pid-file", "", "write the running codex pid here (host reaper)")

	fakeEpoch := flags.String("fake-epoch", "2026-01-01T00:00:00Z", "fake driver timestamp base (RFC3339)")
	stepDelay := flags.Duration("step-delay", 0, "delay between fake records")
	promptPrefix := flags.String("prompt-prefix", "", "prefix for the fake answer")
	crashAfter := flags.Int("crash-after", 0, "crash after N fake records (0 = never)")
	ignoreCancel := flags.Bool("ignore-cancel", false, "fake driver ignores cancellation")
	spawnChild := flags.Bool("spawn-child", false, "spawn a long-lived child (process-group reaping test)")
	childPIDFile := flags.String("child-pid-file", "", "write the spawned child pid here")
	maxOutputBytes := flags.Int("max-output-bytes", 64<<10, "bound one embedded tool/model output")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	epoch, err := time.Parse(time.RFC3339, *fakeEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "keel-oar: invalid -fake-epoch: %v\n", err)
		return 2
	}
	var declared []oar.Capability
	if strings.TrimSpace(*capabilities) != "" {
		for _, part := range strings.Split(*capabilities, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			capability, ok := oar.ParseCapability(part)
			if !ok {
				fmt.Fprintf(os.Stderr, "keel-oar: unknown capability %q\n", part)
				return 2
			}
			declared = append(declared, capability)
		}
	}
	cfg := sidecar.Config{
		Runtime:        *runtime,
		Name:           *name,
		Version:        *version,
		RuntimeVersion: *runtimeVersion,
		Capabilities:   declared,
		Simulated:      *simulated,
		Usage:          *usage,
		Epoch:          epoch,
		StepDelay:      *stepDelay,
		PromptPrefix:   *promptPrefix,
		CrashAfter:     *crashAfter,
		IgnoreCancel:   *ignoreCancel,
		SpawnChild:     *spawnChild,
		ChildPIDFile:   *childPIDFile,
		CodexPIDFile:   *codexPIDFile,
		MaxOutputBytes: *maxOutputBytes,
		Codex: oar.CodexConfig{
			Path:      *codexPath,
			Workdir:   *workdir,
			Sandbox:   *sandbox,
			Model:     *model,
			Timeout:   *timeout,
			ExtraArgs: codexArgs,
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sidecar.Serve(ctx, os.Stdin, os.Stdout, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "keel-oar: %v\n", err)
		return 1
	}
	return 0
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}
