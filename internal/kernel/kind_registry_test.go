package kernel

import "testing"

// TestKindRegistryDeclaresClassAndKey pins A5's per-kind contract: every
// registered kind names a commit class and an idempotency key space, the
// classes the four lanes depend on are exactly the declared ones, and the
// dual-lane kinds are the task/capture facts the system lane is allowed to
// carry.
func TestKindRegistryDeclaresClassAndKey(t *testing.T) {
	classes := map[CommitClass]bool{
		ClassEdge: true, ClassOwner: true, ClassExternal: true, ClassSystem: true,
	}
	keys := map[KeyKind]bool{
		"": true,
		KeyRequest: true, KeyGeneration: true, KeyToolCall: true, KeyRun: true,
		KeyTask: true, KeyFSWrite: true, KeyFSCapture: true, KeyFSCaptureFailed: true,
		KeyConfig: true, KeyPlacement: true, KeySupervision: true, KeyGuardrail: true,
		KeyResume: true, KeyNotificationAck: true, KeyNotificationPush: true,
	}

	for _, kind := range Kinds() {
		spec, ok := kindRegistry[kind]
		if !ok {
			t.Fatalf("%s is in Kinds() but not in kindRegistry", kind)
		}
		if !classes[spec.class] {
			t.Fatalf("%s has unknown commit class %q", kind, spec.class)
		}
		if !keys[spec.key] {
			t.Fatalf("%s has unknown key space %q", kind, spec.key)
		}
	}
	if len(kindRegistry) != len(Kinds()) {
		t.Fatalf("kindRegistry has %d kinds, Kinds() has %d", len(kindRegistry), len(Kinds()))
	}

	// The class/key pairs the lanes and R1 rely on.
	expected := []struct {
		kind  EventKind
		class CommitClass
		key   KeyKind
	}{
		{KindUserMessage, ClassEdge, KeyRequest},
		{KindAssistantMessage, ClassOwner, KeyGeneration},
		{KindToolResult, ClassOwner, KeyToolCall},
		{KindRunFinished, ClassOwner, KeyRun},
		{KindTaskCreated, ClassOwner, KeyTask},
		{KindTaskUpdated, ClassOwner, KeyTask},
		{KindTaskClosed, ClassOwner, KeyTask},
		{KindFSWrite, ClassOwner, KeyFSWrite},
		{KindFSCapture, ClassOwner, KeyFSCapture},
		{KindExternalEvent, ClassExternal, KeyRequest},
		{KindConfigRotated, ClassSystem, KeyConfig},
		{KindPlacementJoined, ClassSystem, KeyPlacement},
		{KindSupervisionEscalated, ClassSystem, KeySupervision},
		{KindGuardrailTripped, ClassSystem, KeyGuardrail},
		{KindResumeAttempt, ClassSystem, KeyResume},
		{KindNotificationAck, ClassSystem, KeyNotificationAck},
		{KindNotificationPushSent, ClassSystem, KeyNotificationPush},
	}
	for _, want := range expected {
		spec := kindRegistry[want.kind]
		if spec.class != want.class || spec.key != want.key {
			t.Fatalf("%s = class %q key %q, want class %q key %q",
				want.kind, spec.class, spec.key, want.class, want.key)
		}
	}

	// The edge whitelist is exactly the ClassEdge entries.
	if edge := EdgeKinds(); len(edge) != 1 || edge[0] != KindUserMessage {
		t.Fatalf("EdgeKinds() = %v, want exactly [%s]", edge, KindUserMessage)
	}

	// The dual-lane kinds are the task-card facts and fs.capture; a pure
	// system kind never carries the system flag (it is always system-laned).
	for _, kind := range []EventKind{
		KindTaskCreated, KindTaskUpdated, KindTaskAttached, KindTaskDetached,
		KindTaskClosed, KindTaskNote, KindFSCapture,
	} {
		if !kindRegistry[kind].system {
			t.Fatalf("%s must be allowed on the system lane", kind)
		}
	}
	for kind, spec := range kindRegistry {
		if spec.system && spec.class == ClassSystem {
			t.Fatalf("%s: the system flag is meaningless on a system-class kind", kind)
		}
	}
	for _, kind := range []EventKind{KindAssistantMessage, KindToolResult, KindRunFinished, KindFSWrite} {
		if kindRegistry[kind].system {
			t.Fatalf("%s must not be allowed on the system lane", kind)
		}
	}
}
