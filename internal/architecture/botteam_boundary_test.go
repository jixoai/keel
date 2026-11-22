package architecture

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Import paths named by the bot-identity layering guards.
const (
	botTeamImportPath = "github.com/jixoai/keel/internal/botteam"
	httpAPIImportPath = "github.com/jixoai/keel/internal/httpapi"
)

// TestBotteamStaysPure pins the domain side of the bot-identity model:
// internal/botteam is values and rules (normalization, validation, content
// hash), so it must not reach into persistence or transport. The check is over
// the transitive graph, so a helper package cannot reintroduce the edge.
func TestBotteamStaysPure(t *testing.T) {
	graph := packageGraph(t, botTeamImportPath)
	for _, forbidden := range []string{
		storeImportPath,
		httpAPIImportPath,
		"net/http",
		agentImportPath,
		harnessExtImportPath,
		"github.com/jixoai/keel/internal/agentcore",
		"github.com/jixoai/keel/internal/agentloop",
		"github.com/jixoai/keel/internal/agenttool",
	} {
		requireNoImport(t, graph, botTeamImportPath, forbidden)
	}
}

// TestStoreDoesNotImportBotteam keeps the persistence layer document-blind:
// a row is tenant + ids + JSON documents, and mapping it to a domain value
// belongs to the caller. If store imported botteam, the domain could grow an IO
// dependency through the back door.
func TestStoreDoesNotImportBotteam(t *testing.T) {
	graph := packageGraph(t, storeImportPath)
	requireNoImport(t, graph, storeImportPath, botTeamImportPath)
}

// TestBotteamGuardedPackagesResolve keeps the guards above from going vacuous
// if the package is renamed or moved.
func TestBotteamGuardedPackagesResolve(t *testing.T) {
	out, err := goList(botTeamImportPath, storeImportPath)
	if err != nil {
		t.Fatalf("go list guarded packages: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("resolved %d guarded packages, want 2: %v", len(out), out)
	}
}

// goList resolves package paths to their canonical import paths.
func goList(packages ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", append([]string{"list"}, packages...)...).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
