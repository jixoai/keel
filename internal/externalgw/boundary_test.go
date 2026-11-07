// Boundary guard: the external gateway service is a consumer of narrow kernel
// seams, never of the ledger itself. Its runtime code must not import the
// packages that own tables, run rows or leases (external-gateway spec: 外部
// 服务 MUST NOT 获得事件账本、lease、租户存储或内核管理接口). The kernel
// package is allowed solely because it defines the Committer / ExternalSessions
// interfaces the service receives; it never receives *Kernel or a *store.Store.

package externalgw

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRuntimeCodeHasNoLedgerDependencies(t *testing.T) {
	banned := map[string]string{
		"github.com/jixoai/keel/internal/store":   "event ledger, run rows, blobs",
		"github.com/jixoai/keel/internal/durable": "durable runtime and lease handling",
		"github.com/jixoai/keel/internal/handoff": "handoff registry and runtime ownership",
		"github.com/jixoai/keel/internal/httpapi": "the HTTP edge (it depends on this package, not the other way around)",
		"github.com/jackc/pgx/v5":                 "the ledger database driver",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, importSpec := range file.Imports {
			path, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			if why, found := banned[path]; found {
				t.Errorf("%s imports %s (%s): the external gateway must not depend on it", name, path, why)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no runtime files were checked; the boundary guard is not running")
	}
}
