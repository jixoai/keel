package assembly

import "github.com/jixoai/keel/internal/pluginpkg"

// Layer is the plugin's architectural layer (kernel/harness/provider/ui/
// service). It is an alias of pluginpkg.Layer on purpose: internal/pluginpkg is
// the frozen vocabulary owner (docs/PLUGIN-EXTENSIBILITY.md §1–§2), so the
// assembler re-uses it instead of keeping a second copy of the five names. The
// alias is read-only: this package never mutates or extends that vocabulary.
type Layer = pluginpkg.Layer

// The five architectural layers, re-exported for callers that do not want to
// import pluginpkg just to name one.
const (
	LayerKernel   = pluginpkg.LayerKernel
	LayerHarness  = pluginpkg.LayerHarness
	LayerProvider = pluginpkg.LayerProvider
	LayerUI       = pluginpkg.LayerUI
	LayerService  = pluginpkg.LayerService
)

// Layers returns the closed set of architectural layers, in declaration order.
func Layers() []Layer { return pluginpkg.Layers() }

// parseLayer validates an architectural layer string.
func parseLayer(raw string) (Layer, bool) { return pluginpkg.ParseLayer(raw) }

// Stage is one layer of the four-stage assembly stack. The stack is applied in
// the declared order; each later stage may override rows written earlier.
type Stage string

const (
	// StageBase is the default assembly: kernel seams, memory hygiene and the
	// product pieces every mode shares. It is the only stage that may declare
	// new rows.
	StageBase Stage = "base"
	// StageMode is one bundle of a run mode (serve/headless/tui/sdk).
	StageMode Stage = "mode"
	// StagePreset is a behavior overlay (tools, prompts, skills) for a session
	// or preset selection.
	StagePreset Stage = "preset"
	// StageConfig is the user configuration document (config.toml).
	StageConfig Stage = "config"
)

// Stages returns the closed stack in canonical application order.
func Stages() []Stage { return []Stage{StageBase, StageMode, StagePreset, StageConfig} }

// ParseStage validates a stage string.
func ParseStage(raw string) (Stage, bool) {
	for _, stage := range Stages() {
		if string(stage) == raw {
			return stage, true
		}
	}
	return "", false
}

// Index returns the stage's position in the stack (base=0). Unknown stages
// return -1.
func (s Stage) Index() int {
	for i, stage := range Stages() {
		if stage == s {
			return i
		}
	}
	return -1
}

// Mode is a run mode of the assembled harness. A mode document's Name must be
// one of these values.
type Mode string

const (
	// ModeServe is the long-running server process.
	ModeServe Mode = "serve"
	// ModeHeadless runs the harness with no interactive UI.
	ModeHeadless Mode = "headless"
	// ModeTUI is the terminal client.
	ModeTUI Mode = "tui"
	// ModeSDK is the embedding SDK.
	ModeSDK Mode = "sdk"
)

// Modes returns the closed set of modes in declaration order.
func Modes() []Mode { return []Mode{ModeServe, ModeHeadless, ModeTUI, ModeSDK} }

// ParseMode validates a mode string.
func ParseMode(raw string) (Mode, bool) {
	for _, mode := range Modes() {
		if string(mode) == raw {
			return mode, true
		}
	}
	return "", false
}
