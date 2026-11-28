package architecture

import (
	"strings"
	"testing"
)

// Plugin split (change keel-plugin-split) import boundaries.
//
// The board plugin owns the task/channel/thread substrate and must not import
// the superagent plugin (or its store). The superagent plugin may depend on the
// board's public service package, but must not import the board host
// integration (which owns pgadapter/Surface) and must never import a board
// storage package. The old superagentboard package is deleted, so any edge to
// it is a regression.
const (
	boardPluginImportPath       = "github.com/jixoai/keel/internal/boardplugin"
	boardIntegrationImportPath  = "github.com/jixoai/keel/internal/boardintegration"
	superagentPluginImportPath  = "github.com/jixoai/keel/internal/superagentplugin"
	superagentStoreImportPath   = "github.com/jixoai/keel/internal/superagentstore"
	legacySuperagentBoardImport = "github.com/jixoai/keel/internal/superagentboard"
	superagentImportPath        = "github.com/jixoai/keel/internal/superagent"
)

func TestBoardPluginDoesNotImportSuperagent(t *testing.T) {
	graph := packageGraph(t, boardPluginImportPath)
	for _, forbidden := range []string{
		superagentImportPath,
		superagentPluginImportPath,
		superagentStoreImportPath,
		legacySuperagentBoardImport,
		boardIntegrationImportPath,
	} {
		requireNoImport(t, graph, boardPluginImportPath, forbidden)
	}
}

func TestSuperagentPluginDoesNotImportBoardStorage(t *testing.T) {
	graph := packageGraph(t, superagentPluginImportPath)
	// A board storage adapter (the board module's pgadapter) would mean the
	// dependent reads board tables instead of the board service. The board
	// plugin's own service package is allowed to carry the board log; only the
	// dependent's direct imports are checked so that ownership chain is not
	// mistaken for a table read.
	for _, forbidden := range []string{
		"github.com/jixoai/keel-board/pgadapter",
		"github.com/jixoai/keel-board/pgadapter/logstore",
		"github.com/jixoai/keel-board/store",
		boardIntegrationImportPath,
	} {
		for _, direct := range graph[superagentPluginImportPath] {
			if direct == forbidden {
				t.Fatalf("%s directly imports %s", superagentPluginImportPath, forbidden)
			}
		}
	}
	for _, forbidden := range []string{legacySuperagentBoardImport} {
		requireNoImport(t, graph, superagentPluginImportPath, forbidden)
	}
}

func TestSuperagentStoreHasNoBoardDependency(t *testing.T) {
	graph := packageGraph(t, superagentStoreImportPath)
	for _, forbidden := range []string{
		boardPluginImportPath,
		boardIntegrationImportPath,
		superagentPluginImportPath,
		legacySuperagentBoardImport,
		"github.com/jixoai/keel-board",
	} {
		// The board module itself (any subpackage) is the storage/API owner; the
		// superagent store must not reach it.
		if forbidden == "github.com/jixoai/keel-board" {
			for pkg := range graph {
				if strings.HasPrefix(pkg, forbidden) {
					t.Fatalf("%s transitively imports %s", superagentStoreImportPath, pkg)
				}
			}
			continue
		}
		requireNoImport(t, graph, superagentStoreImportPath, forbidden)
	}
}

func TestPluginSplitGuardedPackagesResolve(t *testing.T) {
	out, err := goList(boardPluginImportPath, superagentPluginImportPath, superagentStoreImportPath)
	if err != nil {
		t.Fatalf("go list guarded packages: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("resolved %d guarded packages, want 3: %v", len(out), out)
	}
}
