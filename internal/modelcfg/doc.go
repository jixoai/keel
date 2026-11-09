// Package modelcfg is the pure-data core of Keel's provider/model
// configuration. It performs no IO, imports no kernel package, and does not
// construct providers, clients or agents.
//
// # Layering
//
// Configuration is resolved in three layers:
//
//  1. Catalog — the catalog compiled into the binary (EmbeddedBuiltin, or a
//     validated LoadCatalog document) holds static provider facts and ordered
//     model rules. It is the only source of built-in endpoints and baseline
//     capabilities.
//  2. Tenant overlay — OverlayFromDocument parses the tenant's document. It is
//     applied to the catalog with RFC 7386 JSON Merge Patch semantics: absent
//     keys inherit, null clears, concrete values replace, and arrays replace
//     wholesale. The overlay is allowed to be incomplete.
//  3. Selection — a Selection names exactly one identity plus an optional
//     reasoning level. Factory.Resolve snaps it to an immutable ActiveModel,
//     freezing endpoint, capabilities, options and the request-body patch for
//     the lifetime of a run.
//
// Model rules are ordered and are applied in a fixed layer order:
//
//	modelRules (model glob)
//	→ modelApiRules (model glob + apiType)
//	→ siteRules (model glob + exact normalized baseUrl)
//	→ providerModelRules (providerId + modelId)
//
// Within one list, array order is the priority: later matching rules patch
// over earlier ones. Model globs support only "*" (any sequence), match the
// whole string and are case-insensitive. There is no dynamic priority
// algorithm and no rule reordering.
//
// # Identity
//
// A model is identified by (providerId, modelId), never by modelId alone.
// The same model id served by two gateways resolves to two independent
// Effective configurations with their own protocol, endpoint and capabilities.
// A selection without a provider id is accepted only when the model id maps to
// exactly one provider; ambiguity fails closed instead of guessing.
//
// # Completeness
//
// Resolution produces one set of provider/model views for both the settings
// surface and the executable registry. Incomplete or disabled entries remain
// in Settings with issues and field paths (for example "api.baseUrl" or
// "options.reasoningLevel.map.high"), but they are not selectable and Factory
// refuses to execute them. Providers with no models, no protocol, no endpoint
// or no credential reference are therefore visible but inert.
//
// # Deliberate v1 simplifications
//
//   - No expression engine (CEL or otherwise). Option specs carry closed JSON
//     Merge Patch templates: reasoningLevel.values maps each value to a patch
//     object, and maxOutputTokens carries a max plus an optional default patch.
//     An unknown reasoning level fails closed. A future rule language can
//     replace this without changing the resolved types.
//   - No account/entitlement overlay. The overlay layer exists and is shaped
//     for more sources, but v1 implements only catalog → tenant; there is no
//     account source and no per-account projection.
//
// The package is deliberately data-only so the kernel and agent loop stay
// unaware of provider configuration. Wiring (service assembly, HTTP views,
// execution) belongs to the product layer.
package modelcfg
