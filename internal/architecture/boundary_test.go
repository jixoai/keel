package architecture

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// kernelPackages is the loop + ledger core: the packages that must stay usable
// as a library ("keel is one loop"). Everything a product needs sits on top of
// them, never inside.
var kernelPackages = []string{
	"github.com/jixoai/keel/internal/loop",
	"github.com/jixoai/keel/internal/agentcore",
	"github.com/jixoai/keel/internal/agenttool",
	"github.com/jixoai/keel/internal/kernel",
	"github.com/jixoai/keel/internal/agent",
	"github.com/jixoai/keel/internal/store",
}

// productPackages are the layers above the kernel: the product API, its
// extensions (plugins/triggers/sub-agents/hooks), the service host and the CLI.
var productPackages = []string{
	"github.com/jixoai/keel/internal/agentapi",
	"github.com/jixoai/keel/internal/agentext",
	"github.com/jixoai/keel/internal/servicehost",
	"github.com/jixoai/keel/cmd/keel",
}

// modelConfigPackage is the product layer's provider configuration (identity,
// rules, resolution). The kernel seam is plain structs and interfaces on
// purpose: the runtime consumes a resolved ActiveModel, never the
// configuration vocabulary that produced it.
const modelConfigPackage = "github.com/jixoai/keel/internal/modelcfg"

// TestKernelDoesNotImportProductLayers is the structural half of the kernel
// purity rule: the kernel may be used by the product, never the other way
// around. A violation here means a product concern leaked into the loop (the
// "second development surface" the kernel is not allowed to grow).
func TestKernelDoesNotImportProductLayers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := append([]string{"list", "-deps"}, kernelPackages...)
	out, err := exec.CommandContext(ctx, "go", args...).Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, dep := range deps {
		for _, forbidden := range productPackages {
			if dep == forbidden {
				t.Errorf("kernel imports the product layer: %s", forbidden)
			}
		}
		if dep == modelConfigPackage {
			t.Errorf("kernel imports the provider configuration layer: %s", modelConfigPackage)
		}
	}
}

// TestModelConfigStaysIndependent pins the other direction: the provider
// configuration package is pure data (no kernel dependency), so the host field
// carrying it can stay a plain pass-through.
func TestModelConfigStaysIndependent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-deps", modelConfigPackage).Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, dep := range deps {
		for _, forbidden := range kernelPackages {
			if dep == forbidden {
				t.Errorf("provider configuration imports the kernel: %s", forbidden)
			}
		}
	}
}

// TestProductLayersCompileOnTopOfTheKernel is the direction check: the product
// packages exist and resolve (so the guard above is not vacuous because the
// names drifted).
func TestProductLayersCompileOnTopOfTheKernel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", append([]string{"list"}, productPackages...)...).Output()
	if err != nil {
		t.Fatalf("go list product packages: %v", err)
	}
	if got := len(strings.Fields(string(out))); got != len(productPackages) {
		t.Fatalf("resolved %d product packages, want %d: %s", got, len(productPackages), out)
	}
}
