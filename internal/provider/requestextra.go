package provider

// Request-body overlays. StreamConfig.Extra carries the caller's per-request
// options (for a resolved model: its request patch — max output tokens,
// reasoning effort, thinking budget, ...). Each driver applies the overlay to
// the body it built, so the extension rides the same request as the envelope
// without the driver interpreting it.

// applyRequestExtra overlays caller-supplied request options onto a provider
// request body. Managed keys are never overridable: the driver owns the
// protocol envelope, the caller only owns its extensions.
//
// The overlay is sparse and one level deep: a nil value is skipped, a nested
// object patches the existing object's keys without erasing its siblings (an
// overlay {"thinking":{"budget_tokens":1024}} must not drop the rest of the
// thinking object), and any other value replaces the body's.
func applyRequestExtra(body map[string]any, extra map[string]any, managed ...string) {
	if body == nil || len(extra) == 0 {
		return
	}
	protected := make(map[string]struct{}, len(managed))
	for _, key := range managed {
		protected[key] = struct{}{}
	}
	for key, value := range extra {
		if value == nil {
			continue
		}
		if _, ok := protected[key]; ok {
			continue
		}
		if patch, ok := value.(map[string]any); ok {
			existing, _ := body[key].(map[string]any)
			merged := make(map[string]any, len(existing)+len(patch))
			for k, v := range existing {
				merged[k] = v
			}
			for k, v := range patch {
				if v != nil {
					merged[k] = v
				}
			}
			body[key] = merged
			continue
		}
		body[key] = value
	}
}
