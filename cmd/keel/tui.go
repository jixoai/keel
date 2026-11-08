package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// `keel tui` runs the NSH TUI client. NSH builds the terminal target from the
// same chat-surface code as the web UI; this command is the thin launcher: it
// resolves the artifact, points it at the AgentAPI and hands over the terminal.
//
// The artifact is not part of this Go repository. When it is missing, the
// command explains exactly what to build and how to point keel at it — it never
// silently starts nothing.
func runTUICmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keel tui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		bin    = fs.String("bin", os.Getenv("KEEL_TUI_BIN"), "path to the NSH TUI binary")
		url    = fs.String("url", envOr("KEEL_AGENT_URL", "http://"+defaultServeAddr), "AgentAPI base URL")
		token  = fs.String("token", os.Getenv("KEEL_AGENT_TOKEN"), "bearer token (the keel serve passphrase)")
		webDir = fs.String("web-dir", envOr("KEEL_SERVE_WEB_DIR", defaultServeWebDir), "web build directory (a tui/ subdirectory is searched)")
		dryRun = fs.Bool("print", false, "print the resolved command instead of executing it")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, searched := resolveTUI(*bin, *webDir)
	if path == "" {
		fmt.Fprintf(stderr, `keel tui: the NSH TUI artifact is not available.

The terminal UI is built by NSH (pin main/nightly) from packages/chat-surface:

    nsh package --target tui      # in the NSH checkout, produces the TUI binary

Then point keel at it, either persistently:

    export KEEL_TUI_BIN=/path/to/keel-tui

or per invocation:

    keel tui --bin /path/to/keel-tui --url %s

Searched:
  %s

The AgentAPI itself is available now: keel serve serves /v1 and the web build.
`, *url, strings.Join(searched, "\n  "))
		return 3
	}
	if *dryRun {
		fmt.Fprintf(stdout, "%s (KEEL_AGENT_URL=%s, token=%s)\n", path, *url, redactToken(*token))
		return 0
	}
	cmd := exec.Command(path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := append(os.Environ(), "KEEL_AGENT_URL="+*url)
	if *token != "" {
		env = append(env, "KEEL_AGENT_TOKEN="+*token)
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "keel tui: run %s: %v\n", path, err)
		return 1
	}
	return 0
}

// resolveTUI finds the TUI binary and returns the paths it tried.
func resolveTUI(explicit, webDir string) (string, []string) {
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if webDir != "" {
		candidates = append(candidates,
			filepath.Join(webDir, "tui", "keel-tui"),
			filepath.Join(webDir, "tui", "keel-tui.exe"),
			filepath.Join(webDir, "tui", "nsh-tui"),
		)
	}
	if found, err := exec.LookPath("keel-tui"); err == nil {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, candidates
		}
	}
	return "", candidates
}

func redactToken(token string) string {
	if token == "" {
		return "(none)"
	}
	if len(token) <= 4 {
		return "****"
	}
	return "****" + token[len(token)-4:]
}
