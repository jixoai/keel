package assembly

import (
	"regexp"
	"strings"
)

// RowID is the stable identity of one assembly row: `<plugin-id>#<row>`.
//
// Only a row with a valid id participates in overlay. The plugin half follows
// the frozen plugin id rule (docs/PLUGIN-EXTENSIBILITY.md §2:
// ^[a-z][a-z0-9._-]{1,63}$); the row half is a lowercase name segment. The id
// is not positional: reordering a base document changes activation tie-breaks,
// never which row an overlay replaces.
type RowID string

var (
	rowIDPluginPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
	rowIDNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	paramKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	seamImplPattern    = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// ParseRowID validates a row id string.
func ParseRowID(raw string) (RowID, bool) {
	if !validRowID(raw) {
		return "", false
	}
	return RowID(raw), true
}

// Valid reports whether the row id is well formed.
func (id RowID) Valid() bool { return validRowID(string(id)) }

// Plugin returns the plugin-id half of the row id ("" when malformed).
func (id RowID) Plugin() string {
	plugin, _, ok := strings.Cut(string(id), "#")
	if !ok {
		return ""
	}
	return plugin
}

// Name returns the row half of the row id ("" when malformed).
func (id RowID) Name() string {
	_, name, ok := strings.Cut(string(id), "#")
	if !ok {
		return ""
	}
	return name
}

func (id RowID) String() string { return string(id) }

func validRowID(raw string) bool {
	plugin, name, ok := strings.Cut(raw, "#")
	if !ok || strings.Contains(name, "#") {
		return false
	}
	return rowIDPluginPattern.MatchString(plugin) && rowIDNamePattern.MatchString(name)
}

// Row is one assembly row. A row names an implementation (Impl) and its
// parameters; Absent marks a row that an overlay removed.
//
// Params are an opaque scalar string map. An override replaces the whole map
// (last write wins at row granularity); it does not merge keys. That keeps the
// effective value independent of how many overlays touched the row.
type Row struct {
	// ID is the stable row identity. It may be empty only in the base stage:
	// a base row without an id is carried through but never participates in
	// overlay (docs/PLUGIN-EXTENSIBILITY.md §6).
	ID RowID
	// Layer is the architectural layer. Base rows must declare it; overlay
	// rows may omit it (inherit) or restate the inherited value.
	Layer Layer
	// Impl identifies the implementation bound to the row ("local", "remote",
	// "ssh", ...). It is required unless Absent is true.
	Impl string
	// Params are the implementation parameters.
	Params map[string]string
	// Requires lists the row ids that must be available before this row may
	// activate. Only base rows declare requirements: an overlay replaces
	// implementation and parameters, not the dependency shape.
	Requires []RowID
	// Absent removes the row from activation while keeping it visible in the
	// effective set (the "renders as absent" rule).
	Absent bool
}

// Document is one stage document: the base bundle, one mode bundle, one preset
// or the config.toml projection.
type Document struct {
	// Stage places the document in the stack.
	Stage Stage
	// Name is the mode name for a mode document; for other stages it is a
	// human label used in issue paths. It never participates in row identity.
	Name string
	// Rows are in declaration order. Overlay documents must list rows in the
	// effective order they override (see Build); base rows define that order.
	Rows []Row
}

// Clone returns a deep copy of the row.
func (r Row) Clone() Row {
	r.Params = cloneParams(r.Params)
	r.Requires = append([]RowID(nil), r.Requires...)
	return r
}

// EffectiveRow is one row after the whole stack has been applied, with the
// provenance needed to explain it.
type EffectiveRow struct {
	Row
	// Origin is the stage that first declared the row (always base today,
	// because only base may declare rows).
	Origin Stage
	// Source is the last stage that wrote the row.
	Source Stage
	// Overridden reports whether a stage after Origin replaced it.
	Overridden bool
}

func (r EffectiveRow) clone() EffectiveRow {
	r.Row = r.Row.Clone()
	return r
}

func cloneParams(params map[string]string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for key, value := range params {
		out[key] = value
	}
	return out
}

func cloneRows(rows []Row) []Row {
	out := make([]Row, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Clone())
	}
	return out
}
