package assembly_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/assembly"
)

func fullCapabilities(seam assembly.Seam) []assembly.Capability {
	return assembly.KnownCapabilities(seam)
}

func fullProvider(seam assembly.Seam, impl string) assembly.Provider {
	return assembly.Provider{Seam: seam, Impl: impl, Capabilities: fullCapabilities(seam)}
}

func fullRegistry(t *testing.T) *assembly.Registry {
	t.Helper()
	registry, err := assembly.NewRegistry(
		fullProvider(assembly.SeamFS, "local"),
		fullProvider(assembly.SeamShell, "local"),
		fullProvider(assembly.SeamSubprocess, "local"),
		fullProvider(assembly.SeamExecSurface, "local"),
		fullProvider(assembly.SeamTerminal, "local"),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

func TestSeamVocabularyIsClosed(t *testing.T) {
	seams := assembly.Seams()
	if len(seams) != 5 {
		t.Fatalf("seams = %v, want the frozen five", seams)
	}
	want := []assembly.Seam{
		assembly.SeamFS, assembly.SeamShell, assembly.SeamSubprocess,
		assembly.SeamExecSurface, assembly.SeamTerminal,
	}
	for i, seam := range want {
		if seams[i] != seam {
			t.Fatalf("seam[%d] = %q, want %q", i, seams[i], seam)
		}
		capabilities := assembly.KnownCapabilities(seam)
		if len(capabilities) == 0 {
			t.Fatalf("seam %q has no capability vocabulary", seam)
		}
		seen := map[assembly.Capability]bool{}
		for _, capability := range capabilities {
			if seen[capability] {
				t.Fatalf("seam %q repeats capability %q", seam, capability)
			}
			seen[capability] = true
			parsed, ok := assembly.ParseCapability(seam, string(capability))
			if !ok || parsed != capability {
				t.Fatalf("ParseCapability(%q, %q) = %q/%v", seam, capability, parsed, ok)
			}
		}
	}
	if _, ok := assembly.ParseSeam("network"); ok {
		t.Fatal("unknown seam parsed")
	}
	if _, ok := assembly.ParseCapability(assembly.SeamFS, "spawn"); ok {
		t.Fatal("a capability from another seam parsed")
	}
}

func TestCapabilityMatrixListsEveryCapability(t *testing.T) {
	remoteCaps := []assembly.Capability{
		assembly.CapFSRead, assembly.CapFSWrite, assembly.CapFSEdit,
		assembly.CapFSStat, assembly.CapFSList,
	}
	registry, err := assembly.NewRegistry(
		fullProvider(assembly.SeamFS, "local"),
		assembly.Provider{Seam: assembly.SeamFS, Impl: "remote", Capabilities: remoteCaps},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	matrix := registry.Matrix()
	if len(matrix) != 2 {
		t.Fatalf("matrix rows = %d, want 2", len(matrix))
	}
	if matrix[0].Seam != assembly.SeamFS || matrix[0].Impl != "local" || matrix[1].Impl != "remote" {
		t.Fatalf("matrix order = %+v", matrix)
	}
	if got, want := len(matrix[0].Capabilities), len(assembly.KnownCapabilities(assembly.SeamFS)); got != want {
		t.Fatalf("local cells = %d, want every known capability %d", got, want)
	}
	support := map[assembly.Capability]bool{}
	for _, cell := range matrix[1].Capabilities {
		support[cell.Capability] = cell.Supported
	}
	if len(support) != len(assembly.KnownCapabilities(assembly.SeamFS)) {
		t.Fatalf("remote matrix does not cover the vocabulary: %v", support)
	}
	if !support[assembly.CapFSRead] || support[assembly.CapFSSymlink] || support[assembly.CapFSAtomicWrite] {
		t.Fatalf("remote support = %v", support)
	}
	if !registry.Supports(assembly.SeamFS, "local", assembly.CapFSSymlink) {
		t.Fatal("local fs should support symlink")
	}
	if registry.Supports(assembly.SeamFS, "remote", assembly.CapFSSymlink) {
		t.Fatal("remote fs must not claim symlink")
	}
}

func TestEverySeamCapabilityGapFailsBeforeStartup(t *testing.T) {
	base := base(row("keel.seam#row", "local", assembly.LayerHarness))
	for _, seam := range assembly.Seams() {
		t.Run(string(seam), func(t *testing.T) {
			capabilities := assembly.KnownCapabilities(seam)
			missing := capabilities[len(capabilities)-1]
			provider := assembly.Provider{
				Seam:         seam,
				Impl:         "remote",
				Capabilities: capabilities[:len(capabilities)-1],
			}
			registry, err := assembly.NewRegistry(provider)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			built, err := assembly.Prepare([]assembly.Document{base}, registry,
				[]assembly.Requirement{assembly.Need(seam, "remote", capabilities...)})
			if built != nil {
				t.Fatalf("Prepare returned an assembly for an unsatisfiable %s requirement", seam)
			}
			var unsupported *assembly.UnsupportedCapabilityError
			if !errors.As(err, &unsupported) {
				t.Fatalf("error = %T %v, want *UnsupportedCapabilityError", err, err)
			}
			if unsupported.Code() != assembly.CodeUnsupportedCapability {
				t.Fatalf("code = %q", unsupported.Code())
			}
			if len(unsupported.Gaps) != 1 || unsupported.Gaps[0].Seam != seam || unsupported.Gaps[0].Impl != "remote" {
				t.Fatalf("gap = %+v", unsupported.Gaps)
			}
			if len(unsupported.Gaps[0].Missing) != 1 || unsupported.Gaps[0].Missing[0] != missing {
				t.Fatalf("missing = %v, want %q", unsupported.Gaps[0].Missing, missing)
			}
			if !strings.Contains(err.Error(), string(seam)) || !strings.Contains(err.Error(), string(missing)) {
				t.Fatalf("error message does not name the combination: %v", err)
			}
		})
	}
}

func TestUnknownProviderIsUnsupportedCapability(t *testing.T) {
	registry := fullRegistry(t)
	err := registry.Check(assembly.Need(assembly.SeamFS, "ghost", assembly.CapFSRead))
	var unsupported *assembly.UnsupportedCapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T %v", err, err)
	}
	if len(unsupported.Gaps) != 1 || !unsupported.Gaps[0].UnknownProvider {
		t.Fatalf("gap = %+v", unsupported.Gaps)
	}
}

func TestMalformedCapabilityVocabularyIsInvalidCapability(t *testing.T) {
	registry := fullRegistry(t)

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "unknown capability in a requirement",
			call: func() error {
				return registry.Check(assembly.Need(assembly.SeamFS, "local", "teleport"))
			},
		},
		{
			name: "unknown seam in a requirement",
			call: func() error {
				return registry.Check(assembly.Need("network", "local", assembly.CapFSRead))
			},
		},
		{
			name: "invalid provider id in a requirement",
			call: func() error {
				return registry.Check(assembly.Need(assembly.SeamFS, "Not Valid", assembly.CapFSRead))
			},
		},
		{
			name: "unknown capability declaration",
			call: func() error {
				_, err := assembly.NewRegistry(assembly.Provider{Seam: assembly.SeamFS, Impl: "bad", Capabilities: []assembly.Capability{"teleport"}})
				return err
			},
		},
		{
			name: "unknown seam declaration",
			call: func() error {
				_, err := assembly.NewRegistry(assembly.Provider{Seam: "network", Impl: "bad"})
				return err
			},
		},
		{
			name: "duplicate provider",
			call: func() error {
				_, err := assembly.NewRegistry(
					fullProvider(assembly.SeamFS, "local"),
					fullProvider(assembly.SeamFS, "local"),
				)
				return err
			},
		},
		{
			name: "duplicate capability declaration",
			call: func() error {
				_, err := assembly.NewRegistry(assembly.Provider{
					Seam: assembly.SeamFS, Impl: "local",
					Capabilities: []assembly.Capability{assembly.CapFSRead, assembly.CapFSRead},
				})
				return err
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			var invalid *assembly.InvalidCapabilityError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %T %v, want *InvalidCapabilityError", err, err)
			}
			if invalid.Code() != assembly.CodeInvalidCapability {
				t.Fatalf("code = %q", invalid.Code())
			}
		})
	}
}

func TestCheckMergesRepeatedGapsAndAcceptsSatisfiedRequirements(t *testing.T) {
	registry, err := assembly.NewRegistry(
		assembly.Provider{
			Seam:         assembly.SeamExecSurface,
			Impl:         "remote",
			Capabilities: []assembly.Capability{assembly.CapExecMaterialize, assembly.CapExecCapture},
		},
		fullProvider(assembly.SeamFS, "local"),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Check(
		assembly.Need(assembly.SeamFS, "local", assembly.CapFSRead, assembly.CapFSWrite),
		assembly.Need(assembly.SeamExecSurface, "remote", assembly.CapExecMaterialize, assembly.CapExecCapture),
	); err != nil {
		t.Fatalf("satisfied requirements rejected: %v", err)
	}

	err = registry.Check(
		assembly.Need(assembly.SeamExecSurface, "remote", assembly.CapExecSymlink),
		assembly.Need(assembly.SeamExecSurface, "remote", assembly.CapExecExecBit),
	)
	var unsupported *assembly.UnsupportedCapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T %v", err, err)
	}
	if len(unsupported.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want one merged gap", unsupported.Gaps)
	}
	if got := strings.Join(capabilityStrings(unsupported.Gaps[0].Missing), ","); got != "symlink,exec-bit" {
		t.Fatalf("missing = %s", got)
	}
}

func TestPrepareWithoutRequirementsDoesNotNeedARegistry(t *testing.T) {
	built, err := assembly.Prepare([]assembly.Document{base(row("keel.a#row", "v1", assembly.LayerHarness))}, nil, nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if built == nil || built.Len() != 1 {
		t.Fatalf("assembly = %+v", built)
	}
	built, err = assembly.Prepare([]assembly.Document{base(row("keel.a#row", "v1", assembly.LayerHarness))}, nil,
		[]assembly.Requirement{assembly.Need(assembly.SeamFS, "local", assembly.CapFSRead)})
	if built != nil || err == nil {
		t.Fatalf("requirements without a registry = %v/%v, want a typed failure", built, err)
	}
	var invalid *assembly.InvalidCapabilityError
	if !errors.As(err, &invalid) || invalid.Code() != assembly.CodeInvalidCapability {
		t.Fatalf("error = %T %v", err, err)
	}
}

func capabilityStrings(capabilities []assembly.Capability) []string {
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		out = append(out, string(capability))
	}
	return out
}
