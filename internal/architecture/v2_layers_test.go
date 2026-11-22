package architecture

// Layer-boundary guards for the five plugin layers frozen by keel-harness-v2
// W0.5 (docs/PLUGIN-EXTENSIBILITY.md §7). The guards are direction checks, not
// a second module graph: they read `go list -deps` and fail on a reachable
// import, printing the chain.
//
// Registered Phase A exception: layerfs is an in-process Go CAS today, so the
// harness imports it directly. The service layer is still asserted not to
// import the harness/product layers; the Rust+gRPC "service only via gRPC"
// boundary is deferred (evidence/w05-contract.md).

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// v2HarnessPackages are the tool/service/placement packages of the Harness
// layer. They may import kernel and provider seams; nothing below may import
// them.
var v2HarnessPackages = []string{
	"github.com/jixoai/keel/internal/harnessapi",
	"github.com/jixoai/keel/internal/harnessext",
	"github.com/jixoai/keel/internal/servicehost",
	"github.com/jixoai/keel/internal/toolsurface",
	"github.com/jixoai/keel/internal/fileplugin",
	"github.com/jixoai/keel/internal/bashbridge",
	"github.com/jixoai/keel/internal/tasklog",
	"github.com/jixoai/keel/internal/notify",
	"github.com/jixoai/keel/internal/approval",
	"github.com/jixoai/keel/internal/placement",
	"github.com/jixoai/keel/internal/jobqueue",
	"github.com/jixoai/keel/internal/mcphost",
	"github.com/jixoai/keel/internal/skilllib",
	"github.com/jixoai/keel/internal/skills",
}

// v2ProviderPackages are the plugin-provider boundary packages. The plugin
// provider layer is mostly out-of-process: `internal/gateway` is the sidecar
// protocol both sides speak. `internal/provider` is the in-process LLM adapter
// used by the kernel loop, so it is a kernel seam, not this layer;
// `internal/modelprovider` is the provider-type schema vocabulary shared by
// manifests and settings, not a process boundary.
var v2ProviderPackages = []string{
	"github.com/jixoai/keel/internal/gateway",
}

// v2ServicePackages are the content-addressed storage services (Phase A Go
// implementation of the layerfs contract).
var v2ServicePackages = []string{
	"github.com/jixoai/keel/internal/layerfs",
	"github.com/jixoai/keel/internal/blob",
}

const v2UIImportPrefix = "github.com/jixoai/keel/web"

// TestKernelDoesNotImportV2Layers extends the kernel-purity rule to every
// non-kernel layer: the loop may be used by the product, never the other way.
func TestKernelDoesNotImportV2Layers(t *testing.T) {
	forbidden := append(append([]string{}, v2HarnessPackages...), v2ProviderPackages...)
	forbidden = append(forbidden, v2ServicePackages...)
	forbidden = append(forbidden, "github.com/jixoai/keel/internal/externalgw", "github.com/jixoai/keel/internal/loadout", "github.com/jixoai/keel/internal/servicegraph")

	for _, kernel := range kernelPackages {
		graph := packageGraph(t, kernel)
		for _, dependency := range forbidden {
			requireNoImport(t, graph, kernel, dependency)
		}
	}
}

// TestServiceLayerDoesNotImportHarness pins the downward direction: a storage
// service is below the tools that use it, so it cannot reach into the Harness.
func TestServiceLayerDoesNotImportHarness(t *testing.T) {
	for _, service := range v2ServicePackages {
		graph := packageGraph(t, service)
		for _, harness := range v2HarnessPackages {
			requireNoImport(t, graph, service, harness)
		}
	}
}

// TestHarnessDoesNotImportCommands pins the top of the graph: the Harness
// packages are assembled by cmd/keel and cmd/runtime, never the other way. A
// harness package importing a command would make the command impossible to
// reuse and hide startup policy inside a library.
func TestHarnessDoesNotImportCommands(t *testing.T) {
	forbidden := []string{
		"github.com/jixoai/keel/cmd/keel",
		"github.com/jixoai/keel/cmd/runtime",
	}
	for _, harness := range v2HarnessPackages {
		graph := packageGraph(t, harness)
		for _, command := range forbidden {
			requireNoImport(t, graph, harness, command)
		}
	}
}

// TestProviderLayerDoesNotImportHarness pins the same direction for the
// provider adapters: a sidecar protocol or provider schema cannot depend on the
// Harness that hosts it.
func TestProviderLayerDoesNotImportHarness(t *testing.T) {
	for _, provider := range v2ProviderPackages {
		graph := packageGraph(t, provider)
		for _, harness := range v2HarnessPackages {
			requireNoImport(t, graph, provider, harness)
		}
	}
}

// TestNoGoPackageImportsTheUIClient is the UI boundary: the client is a
// separate NSH application under web/, reachable only over HTTP/SSE and the
// host bridge. No Go package may import it (a Go import would be a second
// coupling path around the protocol).
func TestNoGoPackageImportsTheUIClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps ./...: %v", err)
	}
	for _, dependency := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(dependency, v2UIImportPrefix) {
			t.Errorf("a Go package imports the UI client: %s", dependency)
		}
	}
}

// TestV2LayerPackagesResolve keeps the guards above non-vacuous: a renamed
// package would otherwise make every `requireNoImport` below it pass by
// accident (go list fails, or the graph has no node to inspect).
func TestV2LayerPackagesResolve(t *testing.T) {
	all := append(append(append([]string{}, v2HarnessPackages...), v2ProviderPackages...), v2ServicePackages...)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", append([]string{"list"}, all...)...).Output()
	if err != nil {
		t.Fatalf("go list v2 layer packages: %v", err)
	}
	if got := len(strings.Fields(string(out))); got != len(all) {
		t.Fatalf("resolved %d v2 layer packages, want %d: %s", got, len(all), out)
	}
}
