package harnessext

// G0 contract gate: a plugin declares what it does; it cannot reach a sibling
// plugin's internal state. The type surface is the enforcement point — Plugin is
// a declarative value (id/version/provides/requires/tools/hooks/triggers/
// effects/prompt sections plus Start/Stop functions), and the only environment a
// plugin receives is PluginEnv. If someone adds a Manager/Runtime/sibling
// *Plugin handle to either type, the same-layer isolation is gone and this test
// fails before the change can ship.
//
// The negative control is the test itself: adding `Manager *Manager` (or a
// sibling `*Plugin`) to PluginEnv is exactly the edit that makes
// TestPluginTypesExposeNoSiblingStateHandle fail.

import (
	"reflect"
	"testing"
)

// forbiddenPluginFieldTypes are the same-layer internal handles a plugin must
// never receive or hold. The manager owns lifecycle/registry state, the runtime
// owns assembly/rollback state, the sidecar loader owns child-process state, and
// a *Plugin would hand one plugin another's tools/hooks/triggers.
var forbiddenPluginFieldTypes = []reflect.Type{
	reflect.TypeOf((*Manager)(nil)),
	reflect.TypeOf((*Runtime)(nil)),
	reflect.TypeOf((*SidecarLoader)(nil)),
	reflect.TypeOf((*Plugin)(nil)),
}

func TestPluginTypesExposeNoSiblingStateHandle(t *testing.T) {
	for _, target := range []struct {
		name  string
		value any
	}{
		{"Plugin", Plugin{}},
		{"PluginEnv", PluginEnv{}},
		{"RuntimeOptions", RuntimeOptions{}},
	} {
		typ := reflect.TypeOf(target.value)
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			for _, forbidden := range forbiddenPluginFieldTypes {
				if field.Type == forbidden {
					t.Fatalf("%s.%s exposes the same-layer handle %s; a plugin must not reach sibling/internal state",
						target.name, field.Name, forbidden)
				}
			}
		}
	}
	// The declarative plugin has no method that returns a live registry: a
	// plugin can only be handed PluginEnv by the runtime at Start.
	pluginType := reflect.TypeOf(Plugin{})
	for index := 0; index < pluginType.NumMethod(); index++ {
		method := pluginType.Method(index)
		if method.Type.NumOut() > 0 {
			for _, forbidden := range forbiddenPluginFieldTypes {
				if method.Type.Out(0) == forbidden {
					t.Fatalf("Plugin.%s returns the same-layer handle %s", method.Name, forbidden)
				}
			}
		}
	}
}
