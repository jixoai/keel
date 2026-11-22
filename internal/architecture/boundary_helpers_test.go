package architecture

// Shared import-graph helpers for the layering guards. They live in their own
// file so every guard (botteam, provider, kernel) uses one implementation: a
// guard that cannot resolve its package would otherwise pass vacuously.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Import paths named by the layering guards. They are spelled out once so the
// tests read as rules rather than as string soup.
const (
	storeImportPath         = "github.com/jixoai/keel/internal/store"
	modelProviderImportPath = "github.com/jixoai/keel/internal/modelprovider"
	agentImportPath         = "github.com/jixoai/keel/internal/agent"
	harnessExtImportPath    = "github.com/jixoai/keel/internal/harnessext"
)

// packageGraph returns the direct import edges of pkg and of every package it
// pulls in, read from `go list -deps -json`. The result is a closed graph: every
// import named by an edge is itself a key, because -deps lists the whole
// transitive closure.
func packageGraph(t *testing.T, pkg string) map[string][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-deps", "-json", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps -json %s: %v\n%s", pkg, err, bytes.TrimSpace(out))
	}
	graph := make(map[string][]string)
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var listed struct {
			ImportPath string
			Imports    []string
		}
		if err := decoder.Decode(&listed); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode go list -deps -json %s output: %v", pkg, err)
		}
		graph[listed.ImportPath] = listed.Imports
	}
	if _, ok := graph[pkg]; !ok {
		t.Fatalf("go list -deps -json %s did not report the package itself", pkg)
	}
	return graph
}

// requireNoImport fails when forbidden is reachable anywhere in pkg's import
// graph and prints the chain that proves it.
func requireNoImport(t *testing.T, graph map[string][]string, pkg, forbidden string) {
	t.Helper()
	chain := importChain(graph, pkg, forbidden)
	if chain == nil {
		return
	}
	t.Errorf("boundary violated: %s must not depend on %s\nimport chain:\n\t%s",
		pkg, forbidden, strings.Join(chain, "\n\t-> "))
}

// importChain returns the shortest import chain from the package "from" to the
// package "to" in graph, or nil when "to" is not reachable.
func importChain(graph map[string][]string, from, to string) []string {
	if from == to {
		return []string{from}
	}
	previous := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, imported := range graph[current] {
			if _, seen := previous[imported]; seen {
				continue
			}
			previous[imported] = current
			if imported == to {
				return buildChain(previous, to)
			}
			queue = append(queue, imported)
		}
	}
	return nil
}

// buildChain walks the previous links back from to and returns the chain in
// dependency direction (importer first).
func buildChain(previous map[string]string, to string) []string {
	chain := []string{to}
	for at := previous[to]; at != ""; at = previous[at] {
		chain = append(chain, at)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}
