package oar

import (
	"errors"
	"testing"
)

func TestDeclarationForKnownRuntimesIsValid(t *testing.T) {
	for _, runtime := range KnownRuntimes() {
		declaration := DeclarationFor(runtime, "test")
		if err := declaration.Validate(); err != nil {
			t.Fatalf("%s declaration is invalid: %v", runtime, err)
		}
	}
	codex := DeclarationFor(RuntimeCodex, "0.166.0")
	if codex.Simulated {
		t.Fatal("codex must not be simulated")
	}
	if !codex.Has(CapabilityLiveModel) {
		t.Fatal("codex must declare live_model")
	}
	fake := DeclarationFor(RuntimeFake, "test")
	if !fake.Simulated || fake.Has(CapabilityLiveModel) {
		t.Fatal("fake must be simulated and must not declare live_model")
	}
}

func TestDeclarationRejectsUnknownCapabilityAndDuplicates(t *testing.T) {
	declaration := DeclarationFor(RuntimeFake, "test")
	declaration.Capabilities = append(declaration.Capabilities, Capability("telepathy"))
	if err := declaration.Validate(); !IsCode(err, CodeInvalidCapability) {
		t.Fatalf("unknown capability: %v, want invalid_capability", err)
	}

	declaration = DeclarationFor(RuntimeFake, "test")
	declaration.Capabilities = append(declaration.Capabilities, CapabilityRecords)
	if err := declaration.Validate(); !IsCode(err, CodeInvalidCapability) {
		t.Fatalf("duplicate capability: %v, want invalid_capability", err)
	}
}

func TestSimulatedRuntimeCannotClaimLiveModel(t *testing.T) {
	declaration := DeclarationFor(RuntimeFake, "test")
	declaration.Capabilities = append(declaration.Capabilities, CapabilityLiveModel)
	if err := declaration.Validate(); !IsCode(err, CodeInvalidCapability) {
		t.Fatalf("simulated live_model: %v, want invalid_capability", err)
	}
}

func TestRequireReturnsTypedRejection(t *testing.T) {
	fake := DeclarationFor(RuntimeFake, "test")
	err := fake.Require([]Capability{CapabilityStreaming})
	if !IsCode(err, CodeUnsupportedCapability) {
		t.Fatalf("missing streaming: %v, want unsupported_capability", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || len(typed.Required) != 1 || typed.Required[0] != CapabilityStreaming {
		t.Fatalf("typed error does not name the missing capability: %#v", typed)
	}
	if err := fake.Require([]Capability{Capability("telepathy")}); !IsCode(err, CodeInvalidCapability) {
		t.Fatalf("unknown requirement: %v, want invalid_capability", err)
	}
	if err := fake.Require([]Capability{CapabilityLiveModel}); !IsCode(err, CodeSimulatedRuntime) {
		t.Fatalf("live model on fake: %v, want simulated_runtime", err)
	}
	if err := fake.Require(BaseCapabilities); err != nil {
		t.Fatalf("base capabilities must be satisfied by fake: %v", err)
	}
}

func TestParseDeclarationRejectsEmpty(t *testing.T) {
	if _, err := ParseDeclaration(nil); !IsCode(err, CodeProtocol) {
		t.Fatalf("empty declaration: %v, want protocol", err)
	}
}
