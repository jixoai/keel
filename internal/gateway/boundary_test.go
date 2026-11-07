// Boundary guard: the gateway is a leaf. An external process must never reach
// the event ledger, leases or tenant storage through this package, so its
// runtime code must not even import the packages that own them (external-gateway
// spec: 外部服务 MUST NOT 获得事件账本、lease、租户存储或内核管理接口).

package gateway

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
		"github.com/jixoai/keel/internal/store":   "event ledger, blobs, tasks",
		"github.com/jixoai/keel/internal/kernel":  "contract kernel (commit/lease/fencing)",
		"github.com/jixoai/keel/internal/durable": "durable runtime and lease handling",
		"github.com/jixoai/keel/internal/mcp":     "the MCP client (the wire contract is pinned by protocol_test.go instead)",
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
				t.Errorf("%s imports %s (%s): the gateway must not depend on it", name, path, why)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no runtime files were checked; the boundary guard is not running")
	}
}
