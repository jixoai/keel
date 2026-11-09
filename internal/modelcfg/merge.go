package modelcfg

import "encoding/json"

// This file implements RFC 7386 JSON Merge Patch over decoded JSON values.
// The implementation is deliberately local (stdlib only) so the merge rules
// are visible in code: absent keys inherit, null clears, concrete values
// replace, and arrays replace wholesale (there is no element merge).

// mergePatch applies patch to target and returns the merged value. Neither
// input is mutated: target is cloned before being changed. Values are the
// shapes produced by encoding/json (map[string]any, []any, string, bool,
// json.Number, float64, nil).
func mergePatch(target, patch any) any {
	patchMap, ok := patch.(map[string]any)
	if !ok {
		// A non-object patch (including null) replaces the target verbatim.
		return cloneJSONValue(patch)
	}
	targetMap, ok := target.(map[string]any)
	if !ok || targetMap == nil {
		targetMap = map[string]any{}
	}
	out := cloneJSONMap(targetMap)
	for key, value := range patchMap {
		if value == nil {
			delete(out, key)
			continue
		}
		out[key] = mergePatch(out[key], value)
	}
	return out
}

// mergeJSONObjects is the typed convenience used by rule evaluation and the
// factory. It returns a fresh map and never mutates dst.
func mergeJSONObjects(dst, patch map[string]any) map[string]any {
	if len(patch) == 0 {
		return cloneJSONMap(dst)
	}
	merged, _ := mergePatch(cloneJSONMap(dst), patch).(map[string]any)
	if merged == nil {
		return map[string]any{}
	}
	return merged
}

// cloneJSONValue deep-copies the JSON value shapes produced by encoding/json.
// Scalars are immutable and returned as-is.
func cloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneJSONMap(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = cloneJSONValue(v[i])
		}
		return out
	default:
		return v
	}
}

// cloneJSONMap deep-copies a decoded JSON object.
func cloneJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneJSONValue(value)
	}
	return out
}

// normalizeJSONNumbers converts integral floats and json.Number values into
// ints. Request patches are values callers inspect directly (and encode back
// to JSON), so the natural Go representation is preferable to float64.
func normalizeJSONNumbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key := range v {
			v[key] = normalizeJSONNumbers(v[key])
		}
		return v
	case []any:
		for i := range v {
			v[i] = normalizeJSONNumbers(v[i])
		}
		return v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
		if f, err := v.Float64(); err == nil {
			return f
		}
		return v
	case float64:
		if i, ok := floatToInt(v); ok {
			return i
		}
		return v
	default:
		return v
	}
}
