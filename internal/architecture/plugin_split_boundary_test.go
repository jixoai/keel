package architecture

import (
	"os"
	"path/filepath"
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

// TestCoreStoreDoesNotReadPluginOwnedTables is the storage half of the split:
// after the import boundary, the core store must not grow a second write/read
// path into the superagent plugin's tables or the board log. The readiness
// listing (ready.go) and the RLS table list (rls.go) name the tables without
// querying them; schema.sql is DDL, not a read path.
func TestCoreStoreDoesNotReadPluginOwnedTables(t *testing.T) {
	dir := filepath.Join("..", "store")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read core store dir: %v", err)
	}
	allowed := map[string]bool{
		"ready.go": true, // the readiness table list, not a query
		"rls.go":   true, // the RLS apply list, not a query
	}
	// The board log's table names and the superagent plugin's tables. A core
	// store query naming one of these is a boundary regression.
	forbidden := []string{
		"superagent_",
		"board_heads", "board_events", "board_requests",
		"board_actors", "board_watchers", "board_watch_deliveries",
		"board_schedules", "board_audit", "board_sync_",
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, table := range forbidden {
			if strings.Contains(string(content), table) {
				t.Errorf("internal/store/%s references plugin-owned table %q; the owning plugin's adapter must serve it", name, table)
			}
		}
	}
}

// TestImporterUsesBoardTaskLink pins the import idempotency mechanism: the
// importer must look up a task's ticket link before creating a ticket (the
// self-projection regression), not only the source-id key after the insert.
func TestImporterUsesBoardTaskLink(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "superagentplugin", "importer.go"))
	if err != nil {
		t.Fatalf("read importer: %v", err)
	}
	source := string(content)
	if !strings.Contains(source, "GetBoardLinkByTask(") {
		t.Fatal("importer does not call GetBoardLinkByTask; a projected board task would be imported as a second ticket")
	}
	if !strings.Contains(source, "projectedBoardTask(") {
		t.Fatal("importer has no projected-task label check")
	}
}
