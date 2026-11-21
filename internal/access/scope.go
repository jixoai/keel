package access

import (
	"errors"
	"fmt"
	"sort"
)

// Scope is how far a permission grant reaches. Scope is a separate axis from
// the permission key: the key names an action, the scope names the set of
// resources the action applies to (design §3.2, L5/L7). A scope is never baked
// into a key name.
//
// The four scopes form a total order:
//
//	own < team < tenant < all
//
// "Wider" means the grant outranks a narrower one when the same key is granted
// more than once; it is not a bypass of the resource relation (see
// RelationEvaluation).
type Scope string

// Permission scopes, from narrowest to widest. The string values are the
// database vocabulary; do not rename them.
const (
	// ScopeOwn covers resources the principal holds directly (its own resources
	// or resources explicitly shared with its account).
	ScopeOwn Scope = "own"
	// ScopeTeam covers own plus resources shared through a group or a team bot.
	ScopeTeam Scope = "team"
	// ScopeTenant covers every relation the principal holds in its tenant. It is
	// also the natural scope of tenant-level administrative keys.
	ScopeTenant Scope = "tenant"
	// ScopeAll covers every relation, including resources in another tenant.
	// Only a deployment-level super admin can normally hold it (L1/L6).
	ScopeAll Scope = "all"
)

// scopeRank orders scopes from narrowest to widest. A missing entry means the
// scope is unknown.
var scopeRank = map[Scope]int{
	ScopeOwn:    1,
	ScopeTeam:   2,
	ScopeTenant: 3,
	ScopeAll:    4,
}

// ParseScope parses one scope word. Unknown or misspelled scopes are rejected;
// there is no silent fallback to a narrower or wider scope.
func ParseScope(raw string) (Scope, error) {
	scope := Scope(raw)
	if !ValidScope(scope) {
		return "", fmt.Errorf("access: unknown scope %q (want own, team, tenant or all)", raw)
	}
	return scope, nil
}

// ValidScope reports whether scope is in the closed scope set.
func ValidScope(scope Scope) bool {
	_, ok := scopeRank[scope]
	return ok
}

// Valid reports whether the scope is in the closed scope set.
func (s Scope) Valid() bool { return ValidScope(s) }

// Rank returns the scope's position in own < team < tenant < all and whether
// the scope is valid. Invalid scopes have no rank.
func (s Scope) Rank() (int, bool) {
	rank, ok := scopeRank[s]
	return rank, ok
}

// Covers reports whether s is at least as wide as other. Two invalid scopes
// never cover anything.
func (s Scope) Covers(other Scope) bool {
	left, leftOK := s.Rank()
	right, rightOK := other.Rank()
	return leftOK && rightOK && left >= right
}

// CompareScopes returns -1, 0 or +1 as left is narrower than, equal to, or
// wider than right. Invalid scopes sort after every valid scope and compare
// equal to each other.
func CompareScopes(left, right Scope) int {
	leftRank, leftOK := left.Rank()
	rightRank, rightOK := right.Rank()
	switch {
	case !leftOK && !rightOK:
		return 0
	case !leftOK:
		return 1
	case !rightOK:
		return -1
	case leftRank < rightRank:
		return -1
	case leftRank > rightRank:
		return 1
	default:
		return 0
	}
}

// MaxScope returns the widest scope in scopes. An empty list or an invalid
// entry is an error: silently dropping a bad scope would make an effective
// permission set depend on which source happened to be read first.
func MaxScope(scopes ...Scope) (Scope, error) {
	if len(scopes) == 0 {
		return "", errors.New("access: no scope to compare")
	}
	best := Scope("")
	bestRank := 0
	for _, scope := range scopes {
		rank, ok := scope.Rank()
		if !ok {
			return "", fmt.Errorf("access: unknown scope %q", scope)
		}
		if rank > bestRank {
			best = scope
			bestRank = rank
		}
	}
	return best, nil
}

// SortScopes returns a copy ordered own < team < tenant < all. Invalid scopes
// are placed after every valid scope.
func SortScopes(scopes []Scope) []Scope {
	out := append([]Scope(nil), scopes...)
	sort.SliceStable(out, func(i, j int) bool {
		return CompareScopes(out[i], out[j]) < 0
	})
	return out
}
