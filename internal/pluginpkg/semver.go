package pluginpkg

import (
	"fmt"
	"strconv"
	"strings"
)

// SemVer is a parsed semantic version (MAJOR.MINOR.PATCH with optional
// prerelease and build metadata). The manifest's `version` field and the
// `engines.keel` gate both use it; nothing here is lenient about shape, because
// a version that cannot be compared cannot be gated.
type SemVer struct {
	Major int
	Minor int
	Patch int
	// Pre is the prerelease part without the leading dash ("rc.1").
	Pre string
	// Build is the build metadata without the leading plus ("20260101").
	Build string
	raw   string
}

// String returns the canonical rendering, preserving the parsed prerelease and
// build parts.
func (v SemVer) String() string {
	if v.raw != "" {
		return v.raw
	}
	out := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		out += "-" + v.Pre
	}
	if v.Build != "" {
		out += "+" + v.Build
	}
	return out
}

// IsZero reports whether the version was never parsed (the zero value).
func (v SemVer) IsZero() bool {
	return v.raw == "" && v.Major == 0 && v.Minor == 0 && v.Patch == 0 && v.Pre == ""
}

// ParseSemVer parses a strict three-part semantic version. A leading "v" is
// rejected: the manifest grammar is the spec's, not npm's tolerance.
func ParseSemVer(raw string) (SemVer, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return SemVer{}, fmt.Errorf("version is empty")
	}
	if text != raw {
		return SemVer{}, fmt.Errorf("version %q has surrounding whitespace", raw)
	}
	core := text
	build := ""
	if index := strings.IndexByte(core, '+'); index >= 0 {
		build = core[index+1:]
		core = core[:index]
	}
	pre := ""
	if index := strings.IndexByte(core, '-'); index >= 0 {
		pre = core[index+1:]
		core = core[:index]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return SemVer{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", raw)
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		if part == "" {
			return SemVer{}, fmt.Errorf("version %q has an empty numeric part", raw)
		}
		if len(part) > 1 && part[0] == '0' {
			return SemVer{}, fmt.Errorf("version %q has a leading zero in %q", raw, part)
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return SemVer{}, fmt.Errorf("version %q has a non-numeric part %q", raw, part)
		}
		numbers[i] = value
	}
	if pre != "" && !validIdentifiers(pre) {
		return SemVer{}, fmt.Errorf("version %q has an invalid prerelease part", raw)
	}
	if build != "" && !validIdentifiers(build) {
		return SemVer{}, fmt.Errorf("version %q has invalid build metadata", raw)
	}
	return SemVer{Major: numbers[0], Minor: numbers[1], Patch: numbers[2], Pre: pre, Build: build, raw: text}, nil
}

func validIdentifiers(text string) bool {
	for _, part := range strings.Split(text, ".") {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// Compare orders two versions: -1 when a < b, 0 when equal (build metadata is
// ignored, as semver specifies), +1 when a > b.
func (v SemVer) Compare(other SemVer) int {
	if v.Major != other.Major {
		return compareInt(v.Major, other.Major)
	}
	if v.Minor != other.Minor {
		return compareInt(v.Minor, other.Minor)
	}
	if v.Patch != other.Patch {
		return compareInt(v.Patch, other.Patch)
	}
	return comparePrerelease(v.Pre, other.Pre)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePrerelease implements the semver precedence rule: a version with a
// prerelease is lower than the same version without one, and prerelease
// identifiers compare numerically when both are numeric, else lexically.
func comparePrerelease(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	left := strings.Split(a, ".")
	right := strings.Split(b, ".")
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] == right[i] {
			continue
		}
		leftNum, leftErr := strconv.Atoi(left[i])
		rightNum, rightErr := strconv.Atoi(right[i])
		switch {
		case leftErr == nil && rightErr == nil:
			return compareInt(leftNum, rightNum)
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		case left[i] < right[i]:
			return -1
		default:
			return 1
		}
	}
	return compareInt(len(left), len(right))
}

// VersionRange is a parsed `engines.keel` constraint: one or more comparators
// combined with AND. `>=0.2.0 <0.3.0` is the spec's example form; commas are
// accepted as separators too, and `^` / `~` are supported as sugar.
type VersionRange struct {
	Raw          string
	comparators  []comparator
	parsedAnyVer bool
}

type comparator struct {
	op      string
	version SemVer
}

// ParseVersionRange parses a range expression. An empty expression (or "*")
// means "any version".
func ParseVersionRange(raw string) (VersionRange, error) {
	text := strings.TrimSpace(raw)
	if text == "" || text == "*" {
		return VersionRange{Raw: raw, parsedAnyVer: true}, nil
	}
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' })
	rng := VersionRange{Raw: raw}
	for _, field := range fields {
		if field == "" {
			continue
		}
		op := ""
		rest := field
		for _, candidate := range []string{">=", "<=", "==", ">", "<", "=", "^", "~"} {
			if strings.HasPrefix(field, candidate) {
				op = candidate
				rest = strings.TrimSpace(strings.TrimPrefix(field, candidate))
				break
			}
		}
		version, err := ParseSemVer(rest)
		if err != nil {
			return VersionRange{}, fmt.Errorf("engines.keel: %q is not a valid constraint (%v)", field, err)
		}
		if op == "==" {
			op = "="
		}
		rng.comparators = append(rng.comparators, comparator{op: op, version: version})
	}
	if len(rng.comparators) == 0 {
		return VersionRange{}, fmt.Errorf("engines.keel: %q carries no comparator", raw)
	}
	return rng, nil
}

// Contains reports whether the version satisfies every comparator. A
// prerelease version only satisfies a range whose comparator names the same
// MAJOR.MINOR.PATCH with a prerelease, mirroring npm: `>=0.2.0` never matches
// `0.3.0-rc.1`, but `>=0.3.0-0` does.
func (r VersionRange) Contains(v SemVer) bool {
	if r.parsedAnyVer {
		return true
	}
	if v.Pre != "" && !r.namesPrereleaseOf(v) {
		return false
	}
	for _, cmp := range r.comparators {
		if !cmp.matches(v) {
			return false
		}
	}
	return true
}

// namesPrereleaseOf reports whether any comparator mentions the same
// MAJOR.MINOR.PATCH as v with a prerelease (the npm "opt-in" form).
func (r VersionRange) namesPrereleaseOf(v SemVer) bool {
	for _, cmp := range r.comparators {
		other := cmp.version
		if other.Major == v.Major && other.Minor == v.Minor && other.Patch == v.Patch && other.Pre != "" {
			return true
		}
	}
	return false
}

func (c comparator) matches(v SemVer) bool {
	switch c.op {
	case ">=":
		return v.Compare(c.version) >= 0
	case ">":
		return v.Compare(c.version) > 0
	case "<=":
		return v.Compare(c.version) <= 0
	case "<":
		return v.Compare(c.version) < 0
	case "=":
		return v.Compare(c.version) == 0
	case "^":
		// Compatible-with: same leftmost non-zero component.
		switch {
		case c.version.Major > 0:
			return v.Major == c.version.Major && v.Compare(c.version) >= 0
		case c.version.Minor > 0:
			return v.Major == 0 && v.Minor == c.version.Minor && v.Compare(c.version) >= 0
		default:
			return v.Major == 0 && v.Minor == 0 && v.Patch == c.version.Patch
		}
	case "~":
		// Patch-level: same MAJOR.MINOR, at or above the version.
		return v.Major == c.version.Major && v.Minor == c.version.Minor && v.Compare(c.version) >= 0
	default:
		return false
	}
}
