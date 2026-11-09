package modelcfg

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// The built-in catalog is compiled into the binary. It is the static baseline
// every tenant overlay starts from; it never contains a credential value.
//
//go:embed catalog/builtin.json
var builtinCatalogJSON []byte

// Catalog is a validated configuration document. Its contents are immutable;
// NewResolver clones before applying an overlay.
type Catalog struct {
	doc map[string]any
}

// document is the decoded, typed form of a catalog or tenant document.
type document struct {
	providers          []ProviderConfig
	modelRules         []rule
	modelAPIRules      []rule
	siteRules          []rule
	providerModelRules []rule
	selection          Selection
}

// rule is one ordered model-config rule. Which match fields are meaningful
// depends on the list it came from.
type rule struct {
	match      string
	apiType    string
	baseURL    string
	providerID string
	modelID    string
	config     map[string]any
}

// ruleDoc is the wire shape of a rule before validation.
type ruleDoc struct {
	Match      string         `json:"match,omitempty"`
	APIType    string         `json:"apiType,omitempty"`
	BaseURL    string         `json:"baseUrl,omitempty"`
	ProviderID string         `json:"providerId,omitempty"`
	ModelID    string         `json:"modelId,omitempty"`
	Config     map[string]any `json:"config"`
}

var (
	builtinOnce sync.Once
	builtinCat  *Catalog
	builtinErr  error
)

// EmbeddedBuiltin returns the built-in catalog compiled into the binary. The
// embedded document is validated once; an invalid built-in catalog is a
// programming error and panics rather than silently degrading every
// deployment.
func EmbeddedBuiltin() *Catalog {
	builtinOnce.Do(func() {
		builtinCat, builtinErr = LoadCatalog(builtinCatalogJSON)
	})
	if builtinErr != nil {
		panic("modelcfg: embedded builtin catalog: " + builtinErr.Error())
	}
	return builtinCat
}

// LoadCatalog parses and validates a catalog document. Validation is
// structural: provider identities, protocol names, URL shapes, rule shapes and
// field types. Incomplete providers are legal here and surface as issues at
// resolve time so the settings view can show them.
func LoadCatalog(raw []byte) (*Catalog, error) {
	doc, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if err := validateCatalogTopLevel(doc); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if err := requireSchemaVersion(doc); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	decoded, err := decodeDocument(doc)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if err := validateCatalogRuleReferences(decoded); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return &Catalog{doc: doc}, nil
}

// decodeJSONObject decodes a JSON object with numbers preserved as
// json.Number, rejecting trailing content.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("unexpected trailing JSON value")
		}
		return nil, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("document must be a JSON object")
	}
	return obj, nil
}

// decodeDocument decodes and structurally validates the parts of a document
// this package owns. Unknown top-level keys are ignored here; catalog loading
// rejects them separately.
func decodeDocument(doc map[string]any) (document, error) {
	var out document
	if doc == nil {
		return out, nil
	}

	if raw, ok := doc["providers"]; ok && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return out, errors.New("providers must be an array")
		}
		seen := make(map[string]struct{}, len(items))
		for i, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				return out, fmt.Errorf("providers[%d] must be an object", i)
			}
			provider, err := decodeProvider(obj)
			if err != nil {
				return out, fmt.Errorf("providers[%d]: %w", i, err)
			}
			if err := validateProviderConfig(provider); err != nil {
				return out, fmt.Errorf("providers[%d]: %w", i, err)
			}
			if _, duplicate := seen[provider.ID]; duplicate {
				return out, fmt.Errorf("providers[%d].id %q is duplicated", i, provider.ID)
			}
			seen[provider.ID] = struct{}{}
			out.providers = append(out.providers, provider)
		}
	}

	var err error
	if out.modelRules, err = decodeRules(doc, "modelRules", ruleKindModel); err != nil {
		return out, err
	}
	if out.modelAPIRules, err = decodeRules(doc, "modelApiRules", ruleKindModelAPI); err != nil {
		return out, err
	}
	if out.siteRules, err = decodeRules(doc, "siteRules", ruleKindSite); err != nil {
		return out, err
	}
	if out.providerModelRules, err = decodeRules(doc, "providerModelRules", ruleKindProviderModel); err != nil {
		return out, err
	}

	if raw, ok := doc["selection"]; ok && raw != nil {
		selection, err := decodeSelection(raw)
		if err != nil {
			return out, err
		}
		out.selection = selection
	}
	return out, nil
}

func decodeProvider(obj map[string]any) (ProviderConfig, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return ProviderConfig{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var provider ProviderConfig
	if err := dec.Decode(&provider); err != nil {
		return ProviderConfig{}, err
	}
	return provider, nil
}

type ruleKind int

const (
	ruleKindModel ruleKind = iota
	ruleKindModelAPI
	ruleKindSite
	ruleKindProviderModel
)

func decodeRules(doc map[string]any, key string, kind ruleKind) ([]rule, error) {
	raw, ok := doc[key]
	if !ok || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	out := make([]rule, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be an object", key, i)
		}
		decoded, err := decodeRule(obj)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		if err := validateRule(decoded, kind); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
		}
		if err := validateModelConfigMap(decoded.Config); err != nil {
			return nil, fmt.Errorf("%s[%d].config: %w", key, i, err)
		}
		out = append(out, rule{
			match:      strings.TrimSpace(decoded.Match),
			apiType:    strings.TrimSpace(decoded.APIType),
			baseURL:    strings.TrimSpace(decoded.BaseURL),
			providerID: strings.TrimSpace(decoded.ProviderID),
			modelID:    strings.TrimSpace(decoded.ModelID),
			config:     decoded.Config,
		})
	}
	return out, nil
}

func decodeRule(obj map[string]any) (ruleDoc, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return ruleDoc{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var decoded ruleDoc
	if err := dec.Decode(&decoded); err != nil {
		return ruleDoc{}, err
	}
	return decoded, nil
}

// ParseSelection decodes a selection fact: the "selection" field of a
// user.message payload, or a tenant document's selection. An empty or null
// payload yields the zero selection, which callers read as "no choice made".
// The fact and the document share one vocabulary, so the two cannot drift
// apart.
func ParseSelection(raw []byte) (Selection, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return Selection{}, nil
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return Selection{}, fmt.Errorf("selection: %w", err)
	}
	return decodeSelection(value)
}

func decodeSelection(raw any) (Selection, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return Selection{}, errors.New("selection must be an object")
	}
	wire, err := json.Marshal(obj)
	if err != nil {
		return Selection{}, err
	}
	var decoded struct {
		ProviderID     string `json:"providerId"`
		ModelID        string `json:"modelId"`
		ReasoningLevel string `json:"reasoningLevel"`
		Options        *struct {
			ReasoningLevel string `json:"reasoningLevel"`
		} `json:"options"`
	}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		return Selection{}, fmt.Errorf("selection: %w", err)
	}
	selection := Selection{
		ProviderID:     strings.TrimSpace(decoded.ProviderID),
		ModelID:        strings.TrimSpace(decoded.ModelID),
		ReasoningLevel: strings.TrimSpace(decoded.ReasoningLevel),
	}
	if selection.ReasoningLevel == "" && decoded.Options != nil {
		selection.ReasoningLevel = strings.TrimSpace(decoded.Options.ReasoningLevel)
	}
	return selection, nil
}

func validateRule(decoded ruleDoc, kind ruleKind) error {
	switch kind {
	case ruleKindModel:
		if decoded.Match == "" {
			return errors.New("match is required")
		}
		if decoded.APIType != "" || decoded.BaseURL != "" || decoded.ProviderID != "" || decoded.ModelID != "" {
			return errors.New("model rule accepts only match and config")
		}
	case ruleKindModelAPI:
		if decoded.Match == "" {
			return errors.New("match is required")
		}
		if !validAPIType(decoded.APIType) {
			return fmt.Errorf("apiType %q is not supported", decoded.APIType)
		}
		if decoded.BaseURL != "" || decoded.ProviderID != "" || decoded.ModelID != "" {
			return errors.New("model API rule accepts only match, apiType and config")
		}
	case ruleKindSite:
		if decoded.Match == "" {
			return errors.New("match is required")
		}
		if decoded.BaseURL == "" {
			return errors.New("baseUrl is required")
		}
		if err := validateBaseURL(decoded.BaseURL); err != nil {
			return fmt.Errorf("baseUrl: %w", err)
		}
		if decoded.APIType != "" || decoded.ProviderID != "" || decoded.ModelID != "" {
			return errors.New("site rule accepts only match, baseUrl and config")
		}
	case ruleKindProviderModel:
		if decoded.ProviderID == "" {
			return errors.New("providerId is required")
		}
		if decoded.ModelID == "" {
			return errors.New("modelId is required")
		}
		if err := validateIdentifier(decoded.ProviderID, "providerId"); err != nil {
			return err
		}
		if err := validateIdentifier(decoded.ModelID, "modelId"); err != nil {
			return err
		}
		if decoded.Match != "" || decoded.APIType != "" || decoded.BaseURL != "" {
			return errors.New("provider model rule accepts only providerId, modelId and config")
		}
	}
	if decoded.Config == nil {
		return errors.New("config is required")
	}
	return nil
}

func validateProviderConfig(provider ProviderConfig) error {
	if err := validateIdentifier(provider.ID, "id"); err != nil {
		return err
	}
	if provider.Name != "" && strings.ContainsAny(provider.Name, "\r\n") {
		return errors.New("name must not contain line breaks")
	}
	if provider.API.Type != "" && !validAPIType(provider.API.Type) {
		return fmt.Errorf("api.type %q is not supported (want %s|%s|%s)",
			provider.API.Type, APITypeOpenAIChatCompletions, APITypeOpenAIResponses, APITypeAnthropicMessages)
	}
	if provider.API.BaseURL != "" {
		if err := validateBaseURL(provider.API.BaseURL); err != nil {
			return fmt.Errorf("api.baseUrl: %w", err)
		}
	}
	if err := validateHeaders(provider.API.Headers); err != nil {
		return err
	}
	if provider.Access.Env != "" && !envNamePattern.MatchString(provider.Access.Env) {
		return fmt.Errorf("access.env %q must be an environment variable name, never a credential", provider.Access.Env)
	}
	if err := validateSecretRef(provider.Access.Ref); err != nil {
		return fmt.Errorf("access.ref: %w", err)
	}
	if provider.Visibility != "" && provider.Visibility != VisibilityVisible && provider.Visibility != VisibilityHidden {
		return fmt.Errorf("visibility %q is not supported (want visible|hidden)", provider.Visibility)
	}
	seen := make(map[string]struct{}, len(provider.ModelIDs))
	for i, modelID := range provider.ModelIDs {
		if err := validateIdentifier(modelID, fmt.Sprintf("modelIds[%d]", i)); err != nil {
			return err
		}
		if _, duplicate := seen[modelID]; duplicate {
			return fmt.Errorf("modelIds[%d] %q is duplicated", i, modelID)
		}
		seen[modelID] = struct{}{}
	}
	return nil
}

func validateCatalogTopLevel(doc map[string]any) error {
	allowed := map[string]struct{}{
		"schemaVersion":      {},
		"providers":          {},
		"modelRules":         {},
		"modelApiRules":      {},
		"siteRules":          {},
		"providerModelRules": {},
		"selection":          {},
	}
	unknown := make([]string, 0)
	for key := range doc {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown field %q", unknown[0])
	}
	return nil
}

func validateCatalogRuleReferences(doc document) error {
	providers := make(map[string]struct{}, len(doc.providers))
	for _, provider := range doc.providers {
		providers[provider.ID] = struct{}{}
	}
	for i, rule := range doc.providerModelRules {
		if _, ok := providers[rule.providerID]; !ok {
			return fmt.Errorf("providerModelRules[%d].providerId %q does not match any provider", i, rule.providerID)
		}
	}
	return nil
}

func requireSchemaVersion(doc map[string]any) error {
	raw, ok := doc["schemaVersion"]
	if !ok {
		return errors.New("schemaVersion is required")
	}
	version, ok := asInt(raw)
	if !ok || version != 1 {
		return errors.New("schemaVersion must be 1")
	}
	return nil
}

func validAPIType(apiType string) bool {
	switch apiType {
	case APITypeOpenAIChatCompletions, APITypeOpenAIResponses, APITypeAnthropicMessages:
		return true
	default:
		return false
	}
}

var (
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretPrefixes = []string{"sk-", "pk-", "bearer ", "basic "}
)

func validateIdentifier(value, field string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s %q must not have surrounding whitespace", field, value)
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("%s %q must not contain whitespace or control characters", field, value)
		}
	}
	return nil
}

func validateBaseURL(raw string) error {
	if strings.TrimSpace(raw) != raw {
		return errors.New("must not have surrounding whitespace")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return errors.New("must not contain whitespace")
	}
	parsed, err := parseURL(raw)
	if err != nil {
		return err
	}
	if !strings.EqualFold(parsed.scheme, "http") && !strings.EqualFold(parsed.scheme, "https") {
		return errors.New("must use http or https")
	}
	if parsed.host == "" {
		return errors.New("must include a host")
	}
	if parsed.hasUser {
		return errors.New("must not embed credentials; use access")
	}
	return nil
}

func validateSecretRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.TrimSpace(ref) != ref || strings.ContainsAny(ref, " \t\r\n") {
		return errors.New("must not contain whitespace")
	}
	lower := strings.ToLower(ref)
	for _, prefix := range secretPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return errors.New("must be a reference, never a credential value")
		}
	}
	if len(ref) > 512 {
		return errors.New("is too long")
	}
	return nil
}

func validateHeaders(headers map[string]string) error {
	for name, value := range headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("api.headers contains an empty header name")
		}
		if strings.ContainsAny(name, " \t\r\n:") {
			return fmt.Errorf("api.headers name %q is invalid", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("api.headers[%q] must not contain line breaks", name)
		}
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "x-api-key", "api-key", "x-goog-api-key":
			if strings.TrimSpace(value) != "" {
				return fmt.Errorf("api.headers[%q] must not carry a credential; use access", name)
			}
		}
	}
	return nil
}

// validateModelConfigMap validates the types and value domains of a sparse
// model config. Presence is not required: absent and null fields are legal
// because a rule may only touch a subset of the model config.
func validateModelConfigMap(config map[string]any) error {
	if config == nil {
		return errors.New("config must be an object")
	}
	if err := rejectUnknownKeys(config, "enabled", "properties", "options"); err != nil {
		return err
	}
	if value, ok := config["enabled"]; ok && value != nil {
		if _, ok := value.(bool); !ok {
			return errors.New("enabled must be a boolean or null")
		}
	}
	if value, ok := config["properties"]; ok && value != nil {
		properties, ok := value.(map[string]any)
		if !ok {
			return errors.New("properties must be an object or null")
		}
		if err := validatePropertiesMap(properties); err != nil {
			return fmt.Errorf("properties: %w", err)
		}
	}
	if value, ok := config["options"]; ok && value != nil {
		options, ok := value.(map[string]any)
		if !ok {
			return errors.New("options must be an object or null")
		}
		if err := validateOptionsMap(options); err != nil {
			return fmt.Errorf("options: %w", err)
		}
	}
	return nil
}

func validatePropertiesMap(properties map[string]any) error {
	if err := rejectUnknownKeys(properties, "contextWindow", "inputFormat", "outputFormat",
		"supportsToolCall", "supportsJsonSchemaOutput", "supportsNativeWebSearch"); err != nil {
		return err
	}
	if value, ok := properties["contextWindow"]; ok && value != nil {
		window, ok := asInt(value)
		if !ok || window < 0 {
			return errors.New("contextWindow must be a non-negative integer")
		}
	}
	for _, key := range []string{"inputFormat", "outputFormat"} {
		value, ok := properties[key]
		if !ok || value == nil {
			continue
		}
		media, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object or null", key)
		}
		if key == "inputFormat" {
			if err := validateBoolFields(media, "text", "image", "video", "audio", "pdf"); err != nil {
				return fmt.Errorf("inputFormat: %w", err)
			}
		} else {
			if err := validateBoolFields(media, "text"); err != nil {
				return fmt.Errorf("outputFormat: %w", err)
			}
		}
	}
	for _, key := range []string{"supportsToolCall", "supportsJsonSchemaOutput", "supportsNativeWebSearch"} {
		value, ok := properties[key]
		if !ok || value == nil {
			continue
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean or null", key)
		}
	}
	return nil
}

func validateBoolFields(values map[string]any, allowed ...string) error {
	if err := rejectUnknownKeys(values, allowed...); err != nil {
		return err
	}
	for _, key := range allowed {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean or null", key)
		}
	}
	return nil
}

func validateOptionsMap(options map[string]any) error {
	if err := rejectUnknownKeys(options, "reasoningLevel", "maxOutputTokens"); err != nil {
		return err
	}
	if value, ok := options["reasoningLevel"]; ok && value != nil {
		level, ok := value.(map[string]any)
		if !ok {
			return errors.New("reasoningLevel must be an object or null")
		}
		if err := validateEnumSpec(level); err != nil {
			return fmt.Errorf("reasoningLevel: %w", err)
		}
	}
	if value, ok := options["maxOutputTokens"]; ok && value != nil {
		limit, ok := value.(map[string]any)
		if !ok {
			return errors.New("maxOutputTokens must be an object or null")
		}
		if err := validateLimitSpec(limit); err != nil {
			return fmt.Errorf("maxOutputTokens: %w", err)
		}
	}
	return nil
}

func validateEnumSpec(spec map[string]any) error {
	if err := rejectUnknownKeys(spec, "values", "map"); err != nil {
		return err
	}
	if value, ok := spec["values"]; ok && value != nil {
		items, ok := value.([]any)
		if !ok {
			return errors.New("values must be an array of strings or null")
		}
		seen := make(map[string]struct{}, len(items))
		for i, item := range items {
			level, ok := item.(string)
			if !ok || strings.TrimSpace(level) == "" {
				return fmt.Errorf("values[%d] must be a non-empty string", i)
			}
			if _, duplicate := seen[level]; duplicate {
				return fmt.Errorf("values[%d] %q is duplicated", i, level)
			}
			seen[level] = struct{}{}
		}
	}
	if value, ok := spec["map"]; ok && value != nil {
		patches, ok := value.(map[string]any)
		if !ok {
			return errors.New("map must be an object or null")
		}
		if err := validatePatchMap(patches, false); err != nil {
			return fmt.Errorf("map.%w", err)
		}
	}
	return nil
}

func validateLimitSpec(spec map[string]any) error {
	if err := rejectUnknownKeys(spec, "max", "map"); err != nil {
		return err
	}
	if value, ok := spec["max"]; ok && value != nil {
		max, ok := asInt(value)
		if !ok || max < 0 {
			return errors.New("max must be a non-negative integer")
		}
	}
	if value, ok := spec["map"]; ok && value != nil {
		patches, ok := value.(map[string]any)
		if !ok {
			return errors.New("map must be an object or null")
		}
		if err := validatePatchMap(patches, true); err != nil {
			return fmt.Errorf("map.%w", err)
		}
	}
	return nil
}

func validatePatchMap(patches map[string]any, allowEmptyKey bool) error {
	keys := make([]string, 0, len(patches))
	for key := range patches {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" && !allowEmptyKey {
			return errors.New("contains an empty key")
		}
		if key != "" && strings.TrimSpace(key) != key {
			return fmt.Errorf("key %q must not have surrounding whitespace", key)
		}
		value := patches[key]
		if value == nil {
			continue
		}
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%q must be a patch object or null", key)
		}
	}
	return nil
}

// rejectUnknownKeys reports the first unknown key (sorted for determinism).
func rejectUnknownKeys(values map[string]any, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		known[key] = struct{}{}
	}
	unknown := make([]string, 0)
	for key := range values {
		if _, ok := known[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown field %q", unknown[0])
	}
	return nil
}

// parsedURL is the small subset of net/url this package needs for validation.
type parsedURL struct {
	scheme  string
	host    string
	hasUser bool
}

func parseURL(raw string) (parsedURL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return parsedURL{}, err
	}
	return parsedURL{
		scheme:  parsed.Scheme,
		host:    parsed.Host,
		hasUser: parsed.User != nil,
	}, nil
}
