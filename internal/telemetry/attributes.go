// Ported from pi/packages/telemetry/src/index.ts @ 55a5c6914570e2ae31ca36
// (attribute union and undefined-value filtering) and memory.ts
// copyAttributes/copyStatus (deep copies at every boundary).

package telemetry

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// normalizeAttributeValue copies one attribute value and normalizes numbers to
// float64, matching PI's single number type. It reports false for nil and for
// values outside PI's attribute union.
func normalizeAttributeValue(value AttributeValue) (AttributeValue, bool) {
	switch v := value.(type) {
	case nil:
		return nil, false
	case string:
		return v, true
	case bool:
		return v, true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case uintptr:
		return float64(v), true
	case json.Number:
		number, err := v.Float64()
		if err != nil {
			return nil, false
		}
		return number, true
	case []string:
		return append([]string(nil), v...), true
	case []float64:
		return append([]float64(nil), v...), true
	case []float32:
		out := make([]float64, len(v))
		for i := range v {
			out[i] = float64(v[i])
		}
		return out, true
	case []int:
		out := make([]float64, len(v))
		for i := range v {
			out[i] = float64(v[i])
		}
		return out, true
	case []int64:
		out := make([]float64, len(v))
		for i := range v {
			out[i] = float64(v[i])
		}
		return out, true
	case []bool:
		return append([]bool(nil), v...), true
	case []AttributeValue:
		return normalizeInterfaceSlice(v)
	case []any:
		values := make([]AttributeValue, len(v))
		for i := range v {
			values[i] = v[i]
		}
		return normalizeInterfaceSlice(values)
	default:
		return nil, false
	}
}

// normalizeInterfaceSlice narrows a decoded JSON-ish slice to one of PI's
// homogeneous array types.
func normalizeInterfaceSlice(values []AttributeValue) (AttributeValue, bool) {
	normalized := make([]AttributeValue, len(values))
	allStrings, allNumbers, allBooleans := true, true, true
	for i, raw := range values {
		value, ok := normalizeAttributeValue(raw)
		if !ok {
			return nil, false
		}
		normalized[i] = value
		switch value.(type) {
		case string:
			allNumbers = false
			allBooleans = false
		case float64:
			allStrings = false
			allBooleans = false
		case bool:
			allStrings = false
			allNumbers = false
		default:
			return nil, false
		}
	}
	switch {
	case len(normalized) == 0:
		return []AttributeValue{}, true
	case allStrings:
		out := make([]string, len(normalized))
		for i := range normalized {
			out[i] = normalized[i].(string)
		}
		return out, true
	case allNumbers:
		out := make([]float64, len(normalized))
		for i := range normalized {
			out[i] = normalized[i].(float64)
		}
		return out, true
	case allBooleans:
		out := make([]bool, len(normalized))
		for i := range normalized {
			out[i] = normalized[i].(bool)
		}
		return out, true
	default:
		return nil, false
	}
}

// copyAttributes returns a detached copy without nil/unsupported values.
// PI's copyAttributes does the same for undefined values (memory.ts:141-147).
func copyAttributes(attributes SpanAttributes) SpanAttributes {
	copied := make(SpanAttributes, len(attributes))
	for name, value := range attributes {
		normalized, ok := normalizeAttributeValue(value)
		if !ok {
			continue
		}
		copied[name] = normalized
	}
	return copied
}

// mergeAttributes copies update into target, keeping target values for keys
// whose update value is nil. PI filters undefined and merges with spread
// (memory.ts:250-253), so a nil attribute never deletes an existing key.
func mergeAttributes(target, update SpanAttributes) {
	for name, value := range copyAttributes(update) {
		target[name] = value
	}
}

// attributesMatch reports whether every query attribute equals the stored
// attribute of the same name (PI's single-span equality filter,
// memory.ts:420-423).
func attributesMatch(stored, query SpanAttributes) bool {
	for name, raw := range query {
		expected, ok := normalizeAttributeValue(raw)
		if !ok {
			return false
		}
		actual, exists := stored[name]
		if !exists || !reflect.DeepEqual(actual, expected) {
			return false
		}
	}
	return true
}

// attributesKey builds a deterministic identity for an attribute set, used to
// group measurements into StoredMetric intervals.
func attributesKey(attributes SpanAttributes) string {
	names := make([]string, 0, len(attributes))
	for name := range attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		builder.WriteString(strconv.Quote(name))
		builder.WriteByte(':')
		encoded, err := json.Marshal(attributes[name])
		if err != nil {
			builder.WriteString(strconv.Quote(fmt.Sprint(attributes[name])))
		} else {
			builder.Write(encoded)
		}
		builder.WriteByte(';')
	}
	return builder.String()
}

// carrierID is PI's span identity: "<scope>:<local id>" (memory.ts:93-95).
func carrierID(carrier Carrier) string {
	return carrier[carrierScopeKey] + ":" + carrier[carrierSpanKey]
}

func copyCarrier(carrier Carrier) Carrier {
	if carrier == nil {
		return nil
	}
	copied := make(Carrier, len(carrier))
	for name, value := range carrier {
		copied[name] = value
	}
	return copied
}
