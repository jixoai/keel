// Command verifypkg validates the official plugin packages with the kernel's
// own validators and, for `kind: sidecar` packages, performs a real gateway
// handshake and tools/list against the packaged sidecar.
//
// It deliberately reuses the kernel instead of re-implementing a lenient check:
//
//	manifest      internal/pluginpkg.LoadDir   (parse + field validation + tree hash)
//	version gate  Manifest.CheckEngines        (the same gate install/enable run)
//	sidecar       internal/gateway.SpawnStdio + Conn.Connect + ToolClient.ListTools
//	              (the same primitives internal/agentext's sidecar loader uses)
//
// A package that passes here passes the package-level half of the install path;
// the runtime half (enable, tool dispatch, permissions) is exercised by the
// `keel serve` walkthrough in the plugin READMEs and the change report.
//
// Usage:
//
//	go run ./plugins/tools/verifypkg [-root plugins] [-keel-version 0.1.0] [-timeout 20s]
//
// Exit code 0 means every package is usable, every sidecar negotiated the
// tool_provider contract, and every declared tool is offered (and vice versa).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/pluginpkg"
)

func main() {
	root := flag.String("root", "plugins", "directory whose immediate subdirectories are plugin packages")
	keelVersion := flag.String("keel-version", "0.1.0", "running keel version used by the engines.keel gate")
	timeout := flag.Duration("timeout", 20*time.Second, "handshake + tools/list budget per sidecar")
	flag.Parse()

	dirs, err := packageDirs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifypkg: %v\n", err)
		os.Exit(2)
	}
	if len(dirs) == 0 {
		fmt.Fprintf(os.Stderr, "verifypkg: no plugin.yaml found under %s\n", *root)
		os.Exit(2)
	}

	failures := 0
	for _, dir := range dirs {
		if err := verifyPackage(dir, *keelVersion, *timeout); err != nil {
			failures++
			fmt.Printf("FAIL %s\n  %v\n", dir, err)
			continue
		}
		fmt.Printf("OK   %s\n", dir)
	}
	fmt.Printf("\n%d package(s) checked, %d failure(s)\n", len(dirs), failures)
	if failures > 0 {
		os.Exit(1)
	}
}

// packageDirs returns the immediate subdirectories that carry a plugin.yaml.
func packageDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, pluginpkg.PluginFileName)); err != nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func verifyPackage(dir, keelVersion string, timeout time.Duration) error {
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: keelVersion})
	if !pkg.Usable() {
		problem := pkg.Problem
		if len(problem.Fields) > 0 {
			fields, _ := json.Marshal(problem.Fields)
			return fmt.Errorf("%s: %s", problem.Reason, fields)
		}
		return fmt.Errorf("%s", problem.Reason)
	}
	manifest := pkg.Manifest
	if err := manifest.CheckEngines(keelVersion); err != nil {
		return fmt.Errorf("engines.keel: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	details := map[string]any{
		"id":      manifest.ID,
		"version": manifest.Version,
		"kind":    manifest.Kind,
		"hash":    pkg.Hash,
		"tools":   manifest.Provides.Tools,
	}
	switch manifest.Kind {
	case pluginpkg.KindSidecar:
		offered, err := verifySidecar(ctx, pkg, keelVersion, timeout)
		if err != nil {
			return err
		}
		details["offered"] = offered
		if err := checkSurface(manifest, offered); err != nil {
			return err
		}
	case pluginpkg.KindInProcess, pluginpkg.KindWebview:
		// No process to speak to: the manifest check is the whole package check.
	}
	raw, _ := json.Marshal(details)
	fmt.Printf("     %s\n", raw)
	return nil
}

// verifySidecar spawns the package's sidecar exactly like the agentext loader
// (same env allowlist, same cwd, same host identity variables), negotiates the
// handshake and reads the tool catalogue.
func verifySidecar(ctx context.Context, pkg pluginpkg.Package, keelVersion string, timeout time.Duration) ([]string, error) {
	manifest := pkg.Manifest
	if len(manifest.Entry.Command) == 0 {
		return nil, errors.New("kind sidecar requires entry.command")
	}
	argv := append(append([]string(nil), manifest.Entry.Command...), manifest.Entry.Args...)
	head := argv[0]
	if !filepath.IsAbs(head) && strings.ContainsRune(head, '/') {
		head = filepath.Join(pkg.Dir, head)
	}
	env := gateway.MinimalEnv(os.Environ())
	for _, name := range manifest.Entry.Env {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	env = append(env,
		"KEEL_PLUGIN_ID="+manifest.ID,
		"KEEL_PLUGIN_VERSION="+manifest.Version,
		"KEEL_PLUGIN_DIR="+pkg.Dir,
		"KEEL_PLUGIN_PROTOCOL="+gateway.ProtocolVersion,
		"KEEL_VERSION="+keelVersion,
	)

	transport, err := gateway.SpawnStdio(gateway.SpawnOptions{
		Command:    head,
		Args:       argv[1:],
		Dir:        pkg.Dir,
		Env:        env,
		CloseGrace: 2 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn sidecar: %w", err)
	}
	conn := gateway.NewConn(gateway.ConnOptions{
		ClientInfo:      gateway.Implementation{Name: "keel-plugin-verify", Version: "1.0.0"},
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeToolProvider},
		RequestTimeout:  timeout,
	})
	handshake, err := conn.Connect(ctx, transport)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	client := gateway.NewToolProvider(conn)
	definitions, err := client.ListTools(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	offered := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		offered = append(offered, definition.Name)
	}
	sort.Strings(offered)

	// Close goes through the protocol's `close` notification and the ladder; the
	// process must be gone before this returns, or the verifier itself would
	// leave an orphan.
	_ = conn.Close()
	select {
	case <-transport.Done():
	case <-time.After(5 * time.Second):
		return offered, errors.New("the sidecar did not exit within 5s of close")
	}
	if state := transport.ExitState(); state != nil && !state.Success() {
		return offered, fmt.Errorf("the sidecar exited abnormally: %s", state.String())
	}
	if handshake.ProtocolVersion != gateway.ProtocolVersion {
		return offered, fmt.Errorf("negotiated protocol %q, want %q", handshake.ProtocolVersion, gateway.ProtocolVersion)
	}
	return offered, nil
}

// checkSurface compares the manifest's declared tools with what the sidecar
// offers, in both directions: a declaration without an implementation is a lie,
// and an implementation without a declaration is refused by the loader (the
// sidecar catalogue would silently shrink). Both are failures here.
func checkSurface(manifest *pluginpkg.Manifest, offered []string) error {
	declared := manifest.Provides.Tools
	if len(declared) != len(offered) {
		return fmt.Errorf("provides.tools declares %d tool(s) %v but the sidecar offers %d %v", len(declared), declared, len(offered), offered)
	}
	for _, name := range offered {
		if !declaredMatches(declared, manifest.ID, name) {
			return fmt.Errorf("the sidecar offers %q, which provides.tools does not declare", name)
		}
	}
	for _, name := range declared {
		if !offeredMatches(offered, manifest.ID, name) {
			return fmt.Errorf("provides.tools declares %q, which the sidecar does not offer", name)
		}
	}
	return nil
}

func declaredMatches(declared []string, pluginID, local string) bool {
	qualified := pluginID + "." + local
	bare := strings.TrimPrefix(local, pluginID+".")
	for _, name := range declared {
		if name == local || name == qualified || name == bare {
			return true
		}
	}
	return false
}

func offeredMatches(offered []string, pluginID, declared string) bool {
	local := strings.TrimPrefix(declared, pluginID+".")
	for _, name := range offered {
		if name == declared || name == local {
			return true
		}
	}
	return false
}
