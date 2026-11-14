package modelcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// tenantRuleKeys are the ordered rule sets a tenant overlay may extend.
var tenantRuleKeys = []string{"modelRules", "modelApiRules", "siteRules", "providerModelRules"}

// documentModelKeys is the full vocabulary of the provider-configuration slice
// of a tenant document: the rule sets, the providers, and the default
// selection. schemaVersion is deliberately not part of it — declaring a schema
// version configures nothing.
var documentModelKeys = append([]string{"providers"}, append(tenantRuleKeys, "selection")...)

// validateTenantDocument checks the shape of a sparse tenant overlay. Unlike
// the catalog document it must NOT require completeness: a tenant entry is a
// patch, and completeness is only required after the overlay is applied.
func validateTenantDocument(doc map[string]any) error {
	if doc == nil {
		return nil
	}
	if raw, ok := doc["providers"]; ok && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return errors.New("providers must be an array")
		}
		seen := make(map[string]struct{}, len(items))
		for i, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("providers[%d] must be an object", i)
			}
			id := strings.TrimSpace(stringValue(obj["id"]))
			if id == "" {
				return fmt.Errorf("providers[%d].id is required", i)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("providers[%d].id %q is duplicated", i, id)
			}
			seen[id] = struct{}{}
			if err := validateProviderPatch(obj); err != nil {
				return fmt.Errorf("providers[%d]: %w", i, err)
			}
		}
	}
	for i, key := range tenantRuleKeys {
		kind := ruleKind(i)
		if _, err := decodeRules(doc, key, kind); err != nil {
			return err
		}
	}
	if raw, ok := doc["selection"]; ok && raw != nil {
		if _, err := decodeSelection(raw); err != nil {
			return err
		}
		// The document's selection speaks the same vocabulary as an input's
		// selection fact: a key the resolver never reads (a stored baseUrl, say)
		// must not be storable — it would look like it applied while nothing
		// reads it.
		encoded, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("selection: %w", err)
		}
		if err := checkSelectionKeys(encoded, selectionKeys); err != nil {
			return fmt.Errorf("selection: %w", err)
		}
	}
	return nil
}

// validateProviderPatch checks the types of the fields a tenant may set. Absent
// fields are inheritance, null clears, so neither is an error here.
func validateProviderPatch(obj map[string]any) error {
	if raw, ok := obj["name"]; ok && raw != nil {
		if _, ok := raw.(string); !ok {
			return errors.New("name must be a string")
		}
	}
	if raw, ok := obj["type"]; ok && raw != nil {
		value, ok := raw.(string)
		if !ok {
			return errors.New("type must be a string")
		}
		if err := validateProviderType(value); err != nil {
			return fmt.Errorf("type: %w", err)
		}
	}
	if raw, ok := obj["visibility"]; ok && raw != nil {
		value, ok := raw.(string)
		if !ok || (value != VisibilityVisible && value != VisibilityHidden) {
			return fmt.Errorf("visibility must be %q or %q", VisibilityVisible, VisibilityHidden)
		}
	}
	if raw, ok := obj["modelIds"]; ok && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return errors.New("modelIds must be an array")
		}
		for j, item := range items {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("modelIds[%d] must be a string", j)
			}
		}
	}
	if raw, ok := obj["api"]; ok && raw != nil {
		api, ok := raw.(map[string]any)
		if !ok {
			return errors.New("api must be an object")
		}
		if t, ok := api["type"]; ok && t != nil {
			value, ok := t.(string)
			if !ok || !validAPIType(value) {
				return fmt.Errorf("api.type must be one of %q, %q, %q", APITypeOpenAIChatCompletions, APITypeOpenAIResponses, APITypeAnthropicMessages)
			}
		}
		if base, ok := api["baseUrl"]; ok && base != nil {
			if _, ok := base.(string); !ok {
				return errors.New("api.baseUrl must be a string")
			}
		}
		if headers, ok := api["headers"]; ok && headers != nil {
			h, ok := headers.(map[string]any)
			if !ok {
				return errors.New("api.headers must be an object")
			}
			for key, value := range h {
				if _, ok := value.(string); !ok {
					return fmt.Errorf("api.headers[%q] must be a string", key)
				}
			}
		}
	}
	if raw, ok := obj["access"]; ok && raw != nil {
		access, ok := raw.(map[string]any)
		if !ok {
			return errors.New("access must be an object")
		}
		if t, ok := access["type"]; ok && t != nil {
			value, ok := t.(string)
			if !ok || !validAccessType(value) {
				return fmt.Errorf("access.type must be %q or %q", AccessTypeAPIKey, AccessTypeNone)
			}
		}
		for _, field := range []string{"env", "ref"} {
			if value, ok := access[field]; ok && value != nil {
				if _, ok := value.(string); !ok {
					return fmt.Errorf("access.%s must be a string", field)
				}
			}
		}
	}
	return nil
}

// mergeTenant applies a tenant overlay to a catalog document with ADDITIVE
// array semantics:
//
//   - providers merge by id: a tenant entry is a sparse patch of the catalog
//     entry with the same id, and a new id is appended;
//   - modelIds union (catalog members first, then tenant additions), so a
//     tenant can add a model without restating the endpoint; disabling a
//     catalog model is expressed by a model rule with enabled=false;
//   - rule sets append (later rules patch over earlier ones, so tenant rules
//     win inside their layer);
//   - selection replaces (null clears it).
//
// A null tenant array means "no additions". There is intentionally no way to
// delete a built-in provider or rule: hiding (visibility) and disabling
// (enabled) express intent without making a tenant able to remove the shipped
// catalog.
func mergeTenant(base, patch map[string]any) map[string]any {
	out := cloneJSONMap(base)
	if out == nil {
		out = map[string]any{}
	}
	if raw, ok := patch["providers"]; ok && raw != nil {
		out["providers"] = mergeProviderEntries(out["providers"], raw)
	}
	for _, key := range tenantRuleKeys {
		if raw, ok := patch[key]; ok && raw != nil {
			out[key] = appendJSONLists(out[key], raw)
		}
	}
	if raw, ok := patch["selection"]; ok {
		out["selection"] = raw
	}
	return out
}

func mergeProviderEntries(base, patch any) []any {
	baseList := jsonList(base)
	patchList := jsonList(patch)
	out := make([]any, 0, len(baseList)+len(patchList))
	for _, item := range baseList {
		out = append(out, item)
	}
	index := make(map[string]int, len(out))
	for i, item := range out {
		if obj, ok := item.(map[string]any); ok {
			index[strings.TrimSpace(stringValue(obj["id"]))] = i
		}
	}
	for _, item := range patchList {
		obj, ok := item.(map[string]any)
		if !ok {
			// Shape is validated before merging; keep the entry rather than
			// silently dropping it so the final decode reports the defect.
			out = append(out, item)
			continue
		}
		id := strings.TrimSpace(stringValue(obj["id"]))
		pos, exists := index[id]
		if !exists || id == "" {
			index[id] = len(out)
			out = append(out, cloneJSONMap(obj))
			continue
		}
		baseObj, _ := out[pos].(map[string]any)
		merged, _ := mergePatch(baseObj, obj).(map[string]any)
		if merged == nil {
			merged = cloneJSONMap(obj)
		}
		if additions, ok := obj["modelIds"].([]any); ok {
			merged["modelIds"] = unionJSONList(jsonList(baseObj["modelIds"]), additions)
		}
		out[pos] = merged
	}
	return out
}

func appendJSONLists(base, extra any) []any {
	out := make([]any, 0, len(jsonList(base))+len(jsonList(extra)))
	out = append(out, jsonList(base)...)
	out = append(out, jsonList(extra)...)
	return out
}

func jsonList(value any) []any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

// unionJSONList keeps the first occurrence order and drops duplicates by their
// canonical JSON encoding.
func unionJSONList(base, extra []any) []any {
	out := make([]any, 0, len(base)+len(extra))
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, item := range append(append([]any{}, base...), extra...) {
		raw, err := json.Marshal(item)
		if err != nil {
			out = append(out, item)
			continue
		}
		key := string(bytes.TrimSpace(raw))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
