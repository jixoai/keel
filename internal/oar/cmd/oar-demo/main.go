// Command oar-demo is the developer smoke for the OAR vertical slice: start the
// sidecar through oar.Service, open one session, send one prompt, and print the
// record stream as JSON lines. The journal path is printed to stderr.
//
// It is deliberately small and not part of the product runtime; a host process
// uses oar.Service (or externalgw + Config.ExternalBackendConfig) directly.
//
// Usage:
//
//	go build -o /tmp/keel-oar ./internal/oar/cmd/keel-oar
//	go run ./internal/oar/cmd/oar-demo -runtime=fake -sidecar=/tmp/keel-oar \
//	    -journal=/tmp/oar-journal -prompt="inspect the repo"
//	go run ./internal/oar/cmd/oar-demo -runtime=codex -sidecar=/tmp/keel-oar \
//	    -journal=/tmp/oar-journal-codex -timeout=10s -prompt="Reply with OAR-OK"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jixoai/keel/internal/oar"
)

func main() {
	runtime := flag.String("runtime", "fake", "runtime adapter: fake|codex")
	sidecar := flag.String("sidecar", "", "keel-oar executable (required)")
	journal := flag.String("journal", "", "journal directory (default: temp)")
	prompt := flag.String("prompt", "inspect the repo", "user prompt")
	codexPath := flag.String("codex-path", "", "codex executable (default: PATH)")
	timeout := flag.Duration("timeout", 20*time.Second, "one turn timeout")
	model := flag.String("model", "", "optional model override")
	flag.Parse()

	if *sidecar == "" {
		fmt.Fprintln(os.Stderr, "oar-demo: -sidecar is required (build ./internal/oar/cmd/keel-oar first)")
		os.Exit(2)
	}
	journalDir := *journal
	if journalDir == "" {
		journalDir = os.TempDir() + "/keel-oar-demo-journal"
	}
	service, err := oar.NewService(oar.Config{
		Runtime:     *runtime,
		SidecarPath: *sidecar,
		JournalDir:  journalDir,
		Workdir:     os.TempDir(),
		Model:       *model,
		Codex:       oar.CodexConfig{Path: *codexPath, Timeout: *timeout},
		Stderr:      os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	if err := service.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	defer service.Stop(context.Background())
	fmt.Fprintf(os.Stderr, "runtime=%s sidecar_pid=%d declaration=%+v\n",
		service.Runtime(), service.PID(), service.Declaration())

	session, err := service.Open(ctx, oar.OpenRequest{
		Reference:     "demo-ref",
		HostSessionID: "demo-host-session",
		TaskID:        "demo-task-card",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	message, _ := json.Marshal(map[string]string{"text": *prompt})
	if _, err := service.Send(ctx, session.Ref, message); err != nil {
		fmt.Fprintln(os.Stderr, "send:", err)
		os.Exit(1)
	}
	deadline := time.After(*timeout + 5*time.Second)
	for {
		select {
		case record := <-service.Records():
			raw, _ := json.Marshal(record)
			fmt.Println(string(raw))
			if record.Kind == oar.KindStatus && record.Name == "turn.finished" {
				fmt.Fprintf(os.Stderr, "journal=%s\n", service.JournalPath(session.Ref))
				return
			}
		case err := <-service.Errors():
			fmt.Fprintln(os.Stderr, "record error:", err)
		case <-deadline:
			fmt.Fprintln(os.Stderr, "timed out")
			os.Exit(2)
		}
	}
}
