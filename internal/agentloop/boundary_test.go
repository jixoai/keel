package agentloop_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentloop"
)

// ledgerPackages are the packages a loop must never reach: the event ledger
// (store), the contract kernel (writes, leases, fencing), the durable task
// store, the HTTP edge and the host runner. A loop that imported one of them
// could bypass kernel.Commit — the W3 adversarial-review question "can the
// loop still bypass Commit?" is answered structurally here.
var ledgerPackages = []string{
	"github.com/jixoai/keel/internal/store",
	"github.com/jixoai/keel/internal/kernel",
	"github.com/jixoai/keel/internal/durable",
	"github.com/jixoai/keel/internal/httpapi",
	"github.com/jixoai/keel/internal/agent",
	"github.com/jixoai/keel/internal/host",
}

func forbiddenPackage(pkg string) string {
	for _, forbidden := range ledgerPackages {
		if pkg == forbidden || strings.HasPrefix(pkg, forbidden+"/") {
			return forbidden
		}
	}
	return ""
}

// TestLoopSurfaceHasNoLedgerTypes walks every exported type of the service
// surface — the interface, the input/output types, the default implementation —
// and fails when a field, parameter or result reaches into a ledger/host
// package. Adding store.Store (or kernel.Kernel) to Input would fail this test.
func TestLoopSurfaceHasNoLedgerTypes(t *testing.T) {
	surface := []reflect.Type{
		reflect.TypeOf((*agentloop.Loop)(nil)).Elem(),
		reflect.TypeOf(agentloop.LoopFunc(nil)),
		reflect.TypeOf(agentloop.Input{}),
		reflect.TypeOf(agentloop.Context{}),
		reflect.TypeOf(agentloop.ModelConfig{}),
		reflect.TypeOf(agentloop.Limits{}),
		reflect.TypeOf(agentloop.CompactionConfig{}),
		reflect.TypeOf(agentloop.Hooks{}),
		reflect.TypeOf(agentloop.RunStream{}),
		reflect.TypeOf((*agentloop.PiLoop)(nil)),
	}
	seen := map[reflect.Type]bool{}
	for _, typ := range surface {
		walkLoopSurface(t, typ, seen)
	}
}

func walkLoopSurface(t *testing.T, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if typ == nil || seen[typ] {
		return
	}
	seen[typ] = true
	if pkg := typ.PkgPath(); pkg != "" {
		if bad := forbiddenPackage(pkg); bad != "" {
			t.Errorf("service surface reaches ledger/host package %q via %v", bad, typ)
			return
		}
	}
	switch typ.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Chan:
		walkLoopSurface(t, typ.Elem(), seen)
	case reflect.Map:
		walkLoopSurface(t, typ.Key(), seen)
		walkLoopSurface(t, typ.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			walkLoopSurface(t, typ.Field(i).Type, seen)
		}
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i++ {
			walkLoopSurface(t, typ.In(i), seen)
		}
		for i := 0; i < typ.NumOut(); i++ {
			walkLoopSurface(t, typ.Out(i), seen)
		}
	case reflect.Interface:
		for i := 0; i < typ.NumMethod(); i++ {
			walkLoopSurface(t, typ.Method(i).Type, seen)
		}
	}
}

// TestLoopImplementationImportsStayInsideTheServiceBoundary parses the loop
// implementation sources (non-test files of agentloop and testloop) and fails
// when one imports a ledger/host package: the boundary must hold in the import
// graph, not only in the type surface.
func TestLoopImplementationImportsStayInsideTheServiceBoundary(t *testing.T) {
	files := 0
	for _, dir := range []string{".", "testloop"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			files++
			for _, imp := range file.Imports {
				pkg, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: import path %q: %v", path, imp.Path.Value, err)
				}
				if bad := forbiddenPackage(pkg); bad != "" {
					t.Errorf("%s imports %q; the loop must not reach the ledger/host", path, bad)
				}
			}
		}
	}
	if files < 4 {
		t.Fatalf("parsed %d implementation files, want at least 4 (loop.go, pi.go, descriptor.go, testloop/testloop.go)", files)
	}
}
