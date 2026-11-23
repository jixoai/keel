// Package assembly is the pure assembly-plane model of keel-harness-v2 W0.5:
// the four-layer row overlay (base / mode / preset / config.toml) and the
// capability seam registry (fs / shell / subprocess / exec-surface / terminal).
//
// The package owns no runtime state: Build validates row documents, resolves
// the effective row set, derives activation order from declared availability
// and exposes the result as immutable values. It imports internal/pluginpkg
// (the frozen owner of the plugin layer vocabulary) and internal/capturepolicy
// (the closed capture-policy vocabulary used by the config.toml setting); no
// harness, product, servicehost, servicegraph or layerfs package is imported,
// so the assembler cannot become a second dependency-injection runtime.
//
// Contract documents:
//   - docs/ASSEMBLY.md (machine/person contract: row schema, overlay rules,
//     config.toml format, rotation identity, seam capability matrix)
//   - docs/PLUGIN-EXTENSIBILITY.md §6 (the frozen four-layer contract)
//   - openspec/changes/keel-harness-v2/adr/0003-four-layer-row-overlay.md
//
// The hosting process maps its production configuration onto this model; the
// bounded production adapter is internal/loadout (service rows), not a second
// runtime. File-level config.toml parsing/rotation semantics live here and the
// disk adapter is internal/configfile (W3); seam enforcement at process startup
// remains W8/W9 work and is named as such in docs/ASSEMBLY.md.
package assembly
