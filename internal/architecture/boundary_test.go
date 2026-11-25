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
	"github.com/jixoai/keel/internal/harnessapi",
	"github.com/jixoai/keel/internal/harnessext",
	"github.com/jixoai/keel/internal/collab",
	"github.com/jixoai/keel/internal/access",
	"github.com/jixoai/keel/internal/auth",
	"github.com/jixoai/keel/internal/botteam",
	"github.com/jixoai/keel/internal/placement",
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

// TestCollabCoreStaysIndependent pins the contract half of keel-collab-core:
// a mode plugin must not reach the kernel/store to create a session. The collab
// package is the port vocabulary every mode shares, so it may only depend on
// the standard library and uuid; the Host adapter in harnessext and the
// registry assembly in cmd/keel are the product-layer side.
func TestCollabCoreStaysIndependent(t *testing.T) {
	const collabPackage = "github.com/jixoai/keel/internal/collab"
	forbidden := append([]string{}, kernelPackages...)
	forbidden = append(forbidden,
		"github.com/jixoai/keel/internal/harnessapi",
		"github.com/jixoai/keel/internal/harnessext",
		"github.com/jixoai/keel/internal/access",
		"github.com/jixoai/keel/internal/accesswire",
		"github.com/jixoai/keel/internal/toolauth",
		"github.com/jixoai/keel/internal/toolsurface",
		"github.com/jixoai/keel/cmd/keel",
	)
	graph := packageGraph(t, collabPackage)
	for _, dependency := range forbidden {
		requireNoImport(t, graph, collabPackage, dependency)
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

// skillPackages are the skill library's two halves: the pure bundle/parser and
// the tenant registry (which also implements the `skills` tool backend).
var skillPackages = []string{
	"github.com/jixoai/keel/internal/skills",
	"github.com/jixoai/keel/internal/skilllib",
}

// TestSkillPackagesDoNotExecuteScripts is the structural half of the W7 rule
// "a skill's script is executed by the bash tool, never by the skills plugin".
// The registry stores a scripts/*.sh resource as bytes; any process spawn from
// these packages (os/exec, or the bash adapter itself) would be a second
// execution path outside the sandbox, job manager, timeout-to-background and
// output bounds. The runtime half of the rule is tested in skilllib
// (TestManageBackendCreatesVersionsAndNeverExecutes): the closed `manage`
// operation set has no run member.
func TestSkillPackagesDoNotExecuteScripts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", append([]string{"list", "-deps"}, skillPackages...)...).Output()
	if err != nil {
		t.Fatalf("go list -deps skills: %v", err)
	}
	forbidden := map[string]string{
		"os/exec": "spawns a process",
		"github.com/jixoai/keel/internal/bashbridge": "reaches the bash adapter directly",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if why, bad := forbidden[dep]; bad {
			t.Errorf("skills package imports %s (%s); execution belongs to the bash tool's policy", dep, why)
		}
	}
}
