package architecture

// The agent-team change (openspec/changes/keel-agent-team-v1) splits the
// registry into a pure domain layer and the layers that persist and run it.
// The package documentation states the rule; these tests enforce it against the
// real import graph:
//
//   - internal/agentteam is "pure data and rules": types, normalization,
//     deterministic derivation and validation, with no IO (agent.go). It must
//     not import the store that persists it, and it must not import the runtime
//     agent layers (internal/agent, internal/agentext) — either edge would drag
//     the kernel and the product wiring into what is supposed to be a value
//     package.
//   - internal/modelprovider is the registry-level provider contract: provider
//     types, tenant-scoped instances, model catalogs, parameter schemas and
//     health state, likewise "no IO" (provider.go). It must not import the
//     store.
//   - internal/store is the persistence layer. It owns the roster/team rows
//     and must not reach up into the domain package; mapping rows to domain
//     values belongs to the caller.
//
// A guard covers the full transitive graph, not just direct imports: an
// indirect path (agentteam -> helper -> store) is the same layering breach as a
// direct one. When a guard fails it prints the import chain that proves the
// violation, so the leak can be located without rerunning go list by hand.

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

// Import paths named by the agent-team layering guards. They are spelled out
// once so the tests read as rules rather than as string soup.
const (
	storeImportPath         = "github.com/jixoai/keel/internal/store"
	agentTeamImportPath     = "github.com/jixoai/keel/internal/agentteam"
	modelProviderImportPath = "github.com/jixoai/keel/internal/modelprovider"
	agentImportPath         = "github.com/jixoai/keel/internal/agent"
	agentExtImportPath      = "github.com/jixoai/keel/internal/agentext"
)

// TestStoreDoesNotImportAgentTeam pins the persistence side of the boundary:
// internal/store owns the agent/team rows, so it must not depend on the domain
// package that gives them meaning.
func TestStoreDoesNotImportAgentTeam(t *testing.T) {
	graph := packageGraph(t, storeImportPath)
	requireNoImport(t, graph, storeImportPath, agentTeamImportPath)
}

// TestAgentTeamDoesNotImportStoreOrRuntimeLayers pins the domain side: the
// agent-team package is a value package, so it must stay clear of persistence
// (internal/store) and of the runtime agent layers (internal/agent,
// internal/agentext).
func TestAgentTeamDoesNotImportStoreOrRuntimeLayers(t *testing.T) {
	graph := packageGraph(t, agentTeamImportPath)
	for _, forbidden := range []string{storeImportPath, agentImportPath, agentExtImportPath} {
		requireNoImport(t, graph, agentTeamImportPath, forbidden)
	}
}

// TestModelProviderDoesNotImportStore pins the provider registry contract:
// modelprovider describes provider types, instances and catalogs, so it must
// not depend on the store that persists instances.
func TestModelProviderDoesNotImportStore(t *testing.T) {
	graph := packageGraph(t, modelProviderImportPath)
	requireNoImport(t, graph, modelProviderImportPath, storeImportPath)
}

// TestAgentTeamGuardedPackagesResolve keeps the guards above from going vacuous
// if a package is renamed or moved: the names they assert on must still resolve
// in this module.
func TestAgentTeamGuardedPackagesResolve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	guarded := []string{storeImportPath, agentTeamImportPath, modelProviderImportPath}
	out, err := exec.CommandContext(ctx, "go", append([]string{"list"}, guarded...)...).Output()
	if err != nil {
		t.Fatalf("go list guarded packages: %v", err)
	}
	if got := len(strings.Fields(string(out))); got != len(guarded) {
		t.Fatalf("resolved %d guarded packages, want %d: %s", got, len(guarded), out)
	}
}

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
// package "to" in graph, or nil when "to" is not reachable. The chain starts at
// "from" and every consecutive pair is one import edge, so a printed chain reads
// as the actual dependency path.
func importChain(graph map[string][]string, from, to string) []string {
	if from == to {
		return []string{from}
	}
	// previous[X] is the package through which X was first reached; the empty
	// string marks the start of the search.
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
