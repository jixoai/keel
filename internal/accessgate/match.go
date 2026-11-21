package accessgate

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jixoai/keel/internal/access"
)

// MatchResult is one matched route and the path parameters extracted from the
// request path. The gate uses Params because net/http has not populated
// r.PathValue at middleware time.
type MatchResult struct {
	Rule   Rule
	Params map[string]string
}

// Match finds the most specific rule for one request. The product subtree mount
// (ModeDelegated) is never matched: a concrete inner route must decide.
func Match(method, path string) (MatchResult, bool) {
	requestSegments, ok := splitPath(path)
	if !ok {
		return MatchResult{}, false
	}
	var (
		best       Rule
		bestParams map[string]string
		bestVal    int
		found      bool
	)
	for _, rule := range rules {
		if rule.Mode == ModeDelegated {
			continue
		}
		if rule.Method != "" && !strings.EqualFold(rule.Method, method) {
			continue
		}
		params, value, ok := matchPattern(rule.Pattern, requestSegments)
		if !ok {
			continue
		}
		// A method-specific rule beats a method-less one; then the pattern with
		// more literal segments wins; then the longer pattern. A tie keeps the
		// first declaration (the matrix is reviewed in order).
		if rule.Method != "" {
			value += 1000
		}
		if !found || value > bestVal {
			found, best, bestParams, bestVal = true, rule, params, value
		}
	}
	if !found {
		return MatchResult{}, false
	}
	return MatchResult{Rule: best, Params: bestParams}, true
}

// splitPath turns "/v1/a/b" into ["v1","a","b"]. A trailing slash is
// significant only for the root mount and is rejected here (no concrete route
// ends in "/").
func splitPath(path string) ([]string, bool) {
	if path == "" || path == "/" {
		return nil, false
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" || strings.HasSuffix(trimmed, "/") {
		return nil, false
	}
	return strings.Split(trimmed, "/"), true
}

// matchPattern matches one rule pattern against request segments: "{name}"
// segments match one non-empty segment; every other segment is literal.
func matchPattern(pattern string, request []string) (map[string]string, int, bool) {
	expected, ok := splitPath(pattern)
	if !ok || len(expected) != len(request) {
		return nil, 0, false
	}
	params := map[string]string{}
	literals := 0
	for i, part := range expected {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") && len(part) > 2 {
			name := part[1 : len(part)-1]
			if request[i] == "" {
				return nil, 0, false
			}
			params[name] = request[i]
			continue
		}
		if part != request[i] {
			return nil, 0, false
		}
		literals++
	}
	return params, literals*100 + len(expected), true
}

// Validate checks the matrix's internal consistency. It runs in tests and at
// package init: a malformed rule must fail before a request can be misrouted.
func Validate() error {
	if len(rules) == 0 {
		return fmt.Errorf("accessgate: route permission matrix is empty")
	}
	seen := make(map[string]int, len(rules))
	for index, rule := range rules {
		where := fmt.Sprintf("rules[%d] (%s)", index, rule)
		if !ValidMode(rule.Mode) {
			return fmt.Errorf("%s: unknown mode %q", where, rule.Mode)
		}
		if rule.Pattern == "" || !strings.HasPrefix(rule.Pattern, "/") {
			return fmt.Errorf("%s: pattern must be an absolute path", where)
		}
		if rule.Method != "" && !validMethod(rule.Method) {
			return fmt.Errorf("%s: unknown method %q", where, rule.Method)
		}
		duplicateKey := rule.Method + "\x00" + rule.Pattern
		if previous, duplicate := seen[duplicateKey]; duplicate {
			return fmt.Errorf("%s: duplicate declaration (already at rules[%d])", where, previous)
		}
		seen[duplicateKey] = index
		if err := validateRuleMode(rule); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

func validateRuleMode(rule Rule) error {
	switch rule.Mode {
	case ModePublic:
		if rule.Key != "" {
			return fmt.Errorf("public route must not declare a key")
		}
		return nil
	case ModeOperator, ModeDelegated:
		if rule.Key != "" {
			return fmt.Errorf("%s route must not declare a permission key", rule.Mode)
		}
		return nil
	case ModeKey:
		spec, ok := access.Lookup(rule.Key)
		if !ok {
			return fmt.Errorf("key %q is not in the registry", rule.Key)
		}
		if spec.RequiresRelation() {
			return fmt.Errorf("resource-scoped key %q cannot use tenant mode; declare a resource or held-key mode", rule.Key)
		}
		return nil
	case ModeHeldKey, ModeTenantKey:
		spec, ok := access.Lookup(rule.Key)
		if !ok {
			return fmt.Errorf("key %q is not in the registry", rule.Key)
		}
		if !spec.RequiresRelation() {
			return fmt.Errorf("tenant-level key %q must use key mode", rule.Key)
		}
		if rule.Mode == ModeTenantKey {
			if rule.ResourceType != "" || rule.ResourceParam != "" {
				return fmt.Errorf("tenant-key mode must not declare a resource")
			}
			return nil
		}
		if rule.ResourceType != "" || rule.ResourceParam != "" {
			return fmt.Errorf("held-key mode has no resource to attach")
		}
		return nil
	case ModeResourceKey:
		spec, ok := access.Lookup(rule.Key)
		if !ok {
			return fmt.Errorf("key %q is not in the registry", rule.Key)
		}
		if !spec.RequiresRelation() {
			return fmt.Errorf("tenant-level key %q cannot use resource mode", rule.Key)
		}
		if strings.TrimSpace(rule.ResourceType) == "" {
			return fmt.Errorf("resource mode must declare a resource type")
		}
		if !validResourceParam(rule.ResourceParam) {
			return fmt.Errorf("resource mode must declare path:<name> or query:<name>, got %q", rule.ResourceParam)
		}
		return nil
	default:
		return fmt.Errorf("unknown mode %q", rule.Mode)
	}
}

func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func validResourceParam(param string) bool {
	prefix, name, ok := strings.Cut(param, ":")
	if !ok || strings.TrimSpace(name) == "" {
		return false
	}
	switch prefix {
	case "path", "query":
		return true
	}
	return false
}

// CheckRoutes verifies every registered /v1 route owns a declaration. It is the
// boot/test gate: a new route without a rule fails startup, never falls through
// to an unauthenticated handler.
func CheckRoutes(patterns []string) error {
	byRoute := make(map[string]Rule, len(rules))
	for _, rule := range rules {
		byRoute[rule.Method+"\x00"+rule.Pattern] = rule
	}
	missing := make([]string, 0)
	seen := make(map[string]bool, len(patterns))
	for _, pattern := range patterns {
		method, path := splitPattern(pattern)
		if !strings.HasPrefix(path, "/v1") {
			continue
		}
		if path == "/v1/" {
			// Product subtree mount: inner routes are enumerated separately.
			continue
		}
		key := method + "\x00" + path
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := byRoute[key]; !ok {
			missing = append(missing, method+" "+path)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("accessgate: %d /v1 route(s) have no permission declaration: %s",
		len(missing), strings.Join(missing, ", "))
}

// UnusedRules reports matrix declarations no registered route backs. It is a
// report/test helper: a stale rule is drift, not a boot failure (the rule may
// describe a route mounted later by a host).
func UnusedRules(patterns []string) []string {
	registered := make(map[string]bool, len(patterns))
	for _, pattern := range patterns {
		method, path := splitPattern(pattern)
		registered[method+"\x00"+path] = true
	}
	out := make([]string, 0)
	for _, rule := range rules {
		if rule.Mode == ModeDelegated {
			continue
		}
		if !registered[rule.Method+"\x00"+rule.Pattern] {
			out = append(out, rule.String())
		}
	}
	sort.Strings(out)
	return out
}

// splitPattern separates "METHOD /path" or "/path".
func splitPattern(pattern string) (method, path string) {
	pattern = strings.TrimSpace(pattern)
	if strings.HasPrefix(pattern, "/") {
		return "", pattern
	}
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return "", pattern
	}
	return strings.ToUpper(method), path
}

// RuleCount returns the number of declarations (including the delegated mount).
func RuleCount() int { return len(rules) }

func init() {
	if err := Validate(); err != nil {
		panic("accessgate: route matrix self-check failed: " + err.Error())
	}
}
