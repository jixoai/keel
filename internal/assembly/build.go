package assembly

import (
	"fmt"
	"sort"
	"strings"
)

// Blocked reason codes reported for rows that cannot activate.
const (
	// BlockedReasonAbsent: a required row is absent in the effective set.
	BlockedReasonAbsent = "absent"
	// BlockedReasonDependency: a required row is itself blocked.
	BlockedReasonDependency = "dependency"
	// BlockedReasonCycle: defensive code for an activation cycle (Build rejects
	// cycles, so a well-formed assembly never reports this).
	BlockedReasonCycle = "cycle"
)

// Step is one activated row. Steps are in activation order, which is derived
// from declared availability, not from hand-written order.
type Step struct {
	// Index is the row's position in the effective (declaration) order.
	Index int
	// ID is the row id (empty for an anonymous base row).
	ID RowID
	// Row is a copy of the effective row.
	Row EffectiveRow
}

// BlockedRow is a row that cannot activate because the rows it requires are
// unavailable. A blocked row is not an assembly error: disabling a row is a
// normal overlay, and dependents report it instead of silently running
// half-wired.
type BlockedRow struct {
	// Index is the row's position in the effective order.
	Index int
	// ID is the row id (empty for an anonymous base row).
	ID RowID
	// Missing lists the required row ids that are absent or blocked. Order
	// follows the row's Requires declaration; duplicates are removed.
	Missing []RowID
	// Reason is one of the BlockedReason* codes.
	Reason string
}

// Assembly is the immutable result of applying a stack. It keeps the effective
// row order (no reordering) and a separate activation order (availability
// driven).
type Assembly struct {
	effective  []EffectiveRow
	byID       map[RowID]int
	activation []Step
	blocked    []BlockedRow
}

// Len returns the number of effective rows.
func (a *Assembly) Len() int {
	if a == nil {
		return 0
	}
	return len(a.effective)
}

// Effective returns a deep copy of every effective row in declaration order.
// Absent rows are included; use Active for the rows that activate.
func (a *Assembly) Effective() []EffectiveRow {
	if a == nil {
		return nil
	}
	out := make([]EffectiveRow, 0, len(a.effective))
	for _, row := range a.effective {
		out = append(out, row.clone())
	}
	return out
}

// Active returns a deep copy of the rows that activate, in effective order.
func (a *Assembly) Active() []EffectiveRow {
	if a == nil {
		return nil
	}
	active := make([]bool, len(a.effective))
	for _, step := range a.activation {
		active[step.Index] = true
	}
	out := make([]EffectiveRow, 0, len(a.activation))
	for i, row := range a.effective {
		if active[i] {
			out = append(out, row.clone())
		}
	}
	return out
}

// Row returns a copy of one effective row by id. Absent rows are returned with
// Absent set; they are not silently dropped.
func (a *Assembly) Row(id RowID) (EffectiveRow, bool) {
	if a == nil || id == "" {
		return EffectiveRow{}, false
	}
	index, ok := a.byID[id]
	if !ok {
		return EffectiveRow{}, false
	}
	return a.effective[index].clone(), true
}

// Activation returns the activation order as a copy.
func (a *Assembly) Activation() []Step {
	if a == nil {
		return nil
	}
	out := make([]Step, 0, len(a.activation))
	for _, step := range a.activation {
		step.Row = step.Row.clone()
		out = append(out, step)
	}
	return out
}

// Blocked returns the rows that could not activate, in effective order.
func (a *Assembly) Blocked() []BlockedRow {
	if a == nil {
		return nil
	}
	out := make([]BlockedRow, 0, len(a.blocked))
	for _, row := range a.blocked {
		row.Missing = append([]RowID(nil), row.Missing...)
		out = append(out, row)
	}
	return out
}

// Build validates the whole stack and resolves it. A stack must contain exactly
// one base document; mode, preset and config documents are optional and, when
// present, must appear in canonical stack order.
func Build(docs []Document) (*Assembly, error) {
	if issues := validateStack(docs); len(issues) > 0 {
		return nil, &Error{Issues: issues}
	}
	var base Document
	for _, doc := range docs {
		if doc.Stage == StageBase {
			base = doc
			break
		}
	}
	effective, byID, issues := buildBase(base)
	if len(issues) > 0 {
		return nil, &Error{Issues: issues}
	}
	for _, doc := range docs {
		if doc.Stage == StageBase {
			continue
		}
		if issues := applyOverlay(doc, effective, byID); len(issues) > 0 {
			return nil, &Error{Issues: issues}
		}
	}
	if issues := validateEffectiveRequirements(effective, byID); len(issues) > 0 {
		return nil, &Error{Issues: issues}
	}
	activation, blocked := computeActivation(effective, byID)
	return &Assembly{
		effective:  effective,
		byID:       byID,
		activation: activation,
		blocked:    blocked,
	}, nil
}

// Prepare is the startup entry point: it builds the stack and then rejects any
// unsupported seam combination before returning an assembly. A capability gap
// therefore fails before startup instead of surfacing on the first tool call.
func Prepare(docs []Document, registry *Registry, requirements []Requirement) (*Assembly, error) {
	assembly, err := Build(docs)
	if err != nil {
		return nil, err
	}
	if err := assembly.CheckSeams(registry, requirements); err != nil {
		return nil, err
	}
	return assembly, nil
}

// CheckSeams rejects requirements the registry cannot satisfy.
func (a *Assembly) CheckSeams(registry *Registry, requirements []Requirement) error {
	if len(requirements) == 0 {
		return nil
	}
	if registry == nil {
		return &InvalidCapabilityError{Problems: []CapabilityProblem{{
			Code:    ProblemMissingProvider,
			Message: "seam requirements were declared without a provider registry",
		}}}
	}
	return registry.Check(requirements...)
}

func validateStack(docs []Document) []Issue {
	if len(docs) == 0 {
		return []Issue{{Code: IssueMissingBase, Message: "the assembly stack has no base document"}}
	}
	var issues []Issue
	seen := map[Stage]bool{}
	last := -1
	for i, doc := range docs {
		stage, ok := ParseStage(string(doc.Stage))
		if !ok {
			issues = append(issues, Issue{
				Code:    IssueStageUnknown,
				Field:   fmt.Sprintf("documents[%d]", i),
				Message: fmt.Sprintf("unknown stage %q", doc.Stage),
			})
			continue
		}
		if seen[stage] {
			issues = append(issues, Issue{
				Code:    IssueStageDuplicate,
				Stage:   stage,
				Field:   fmt.Sprintf("documents[%d]", i),
				Message: fmt.Sprintf("stage %q appears more than once", stage),
			})
		}
		seen[stage] = true
		if stage.Index() < last {
			issues = append(issues, Issue{
				Code:    IssueStageOrder,
				Stage:   stage,
				Field:   fmt.Sprintf("documents[%d]", i),
				Message: fmt.Sprintf("stage %q appears after a later stage", stage),
			})
		}
		if stage.Index() > last {
			last = stage.Index()
		}
		if stage == StageMode {
			if _, ok := ParseMode(doc.Name); !ok {
				issues = append(issues, Issue{
					Code:    IssueMode,
					Stage:   StageMode,
					Field:   field(doc, i),
					Message: fmt.Sprintf("mode document must name one of serve/headless/tui/sdk, got %q", doc.Name),
				})
			}
		}
	}
	if !seen[StageBase] {
		issues = append(issues, Issue{Code: IssueMissingBase, Message: "the assembly stack has no base document"})
	}
	return issues
}

func buildBase(doc Document) ([]EffectiveRow, map[RowID]int, []Issue) {
	var issues []Issue
	rows := make([]EffectiveRow, 0, len(doc.Rows))
	byID := make(map[RowID]int, len(doc.Rows))
	seen := map[RowID]bool{}
	for i, input := range doc.Rows {
		where := field(doc, i)
		row := input.Clone()
		if row.ID != "" {
			if !row.ID.Valid() {
				issues = append(issues, Issue{
					Code:    IssueRowID,
					Stage:   StageBase,
					Row:     row.ID,
					Field:   where + ".id",
					Message: fmt.Sprintf("invalid row id %q (want <plugin-id>#<row>)", row.ID),
				})
				continue
			}
			if seen[row.ID] {
				issues = append(issues, Issue{
					Code:    IssueRowDuplicate,
					Stage:   StageBase,
					Row:     row.ID,
					Field:   where + ".id",
					Message: fmt.Sprintf("row id %q is declared twice", row.ID),
				})
				continue
			}
			seen[row.ID] = true
		}
		if row.Absent {
			issues = append(issues, Issue{
				Code:    IssueRowBaseAbsent,
				Stage:   StageBase,
				Row:     row.ID,
				Field:   where + ".absent",
				Message: "the base stage declares rows; it cannot mark one absent",
			})
			continue
		}
		if _, ok := parseLayer(string(row.Layer)); !ok {
			issues = append(issues, Issue{
				Code:    IssueRowLayer,
				Stage:   StageBase,
				Row:     row.ID,
				Field:   where + ".layer",
				Message: fmt.Sprintf("invalid layer %q", row.Layer),
			})
			continue
		}
		if strings.TrimSpace(row.Impl) == "" {
			issues = append(issues, Issue{
				Code:    IssueRowImpl,
				Stage:   StageBase,
				Row:     row.ID,
				Field:   where + ".impl",
				Message: "a present row must name an implementation",
			})
			continue
		}
		if paramIssues := validateParams(row.Params, where); len(paramIssues) > 0 {
			issues = append(issues, paramIssues...)
			continue
		}
		requires, requireIssues := validateRequires(row.ID, row.Requires, where)
		if len(requireIssues) > 0 {
			issues = append(issues, requireIssues...)
			continue
		}
		row.Requires = requires
		position := len(rows)
		if row.ID != "" {
			byID[row.ID] = position
		}
		rows = append(rows, EffectiveRow{Row: row, Origin: StageBase, Source: StageBase})
	}
	return rows, byID, issues
}

func applyOverlay(doc Document, effective []EffectiveRow, byID map[RowID]int) []Issue {
	var issues []Issue
	seen := map[RowID]bool{}
	lastPosition := -1
	for i, input := range doc.Rows {
		where := field(doc, i)
		row := input.Clone()
		if row.ID == "" {
			issues = append(issues, Issue{
				Code:    IssueRowID,
				Stage:   doc.Stage,
				Field:   where + ".id",
				Message: "overlay rows must have a stable row id; a row without one does not participate in overlay",
			})
			continue
		}
		if !row.ID.Valid() {
			issues = append(issues, Issue{
				Code:    IssueRowID,
				Stage:   doc.Stage,
				Row:     row.ID,
				Field:   where + ".id",
				Message: fmt.Sprintf("invalid row id %q (want <plugin-id>#<row>)", row.ID),
			})
			continue
		}
		if seen[row.ID] {
			issues = append(issues, Issue{
				Code:    IssueRowDuplicate,
				Stage:   doc.Stage,
				Row:     row.ID,
				Field:   where + ".id",
				Message: fmt.Sprintf("row id %q appears twice in this document", row.ID),
			})
			continue
		}
		seen[row.ID] = true

		position, ok := byID[row.ID]
		if !ok {
			issues = append(issues, Issue{
				Code:    IssueRowUnknown,
				Stage:   doc.Stage,
				Row:     row.ID,
				Field:   where + ".id",
				Message: fmt.Sprintf("row id %q is not declared by an earlier stage", row.ID),
			})
			continue
		}
		if position <= lastPosition {
			issues = append(issues, Issue{
				Code:    IssueRowOrder,
				Stage:   doc.Stage,
				Row:     row.ID,
				Field:   where + ".id",
				Message: fmt.Sprintf("row %q is out of effective order; an overlay must not reorder rows", row.ID),
			})
		}
		lastPosition = position
		inherited := effective[position]

		if row.Layer != "" {
			if _, ok := parseLayer(string(row.Layer)); !ok {
				issues = append(issues, Issue{
					Code:    IssueRowLayer,
					Stage:   doc.Stage,
					Row:     row.ID,
					Field:   where + ".layer",
					Message: fmt.Sprintf("invalid layer %q", row.Layer),
				})
				continue
			}
			if row.Layer != inherited.Layer {
				issues = append(issues, Issue{
					Code:    IssueRowLayer,
					Stage:   doc.Stage,
					Row:     row.ID,
					Field:   where + ".layer",
					Message: fmt.Sprintf("an override cannot change layer %q to %q", inherited.Layer, row.Layer),
				})
				continue
			}
		}

		if row.Absent {
			if row.Impl != "" || len(row.Params) > 0 || len(row.Requires) > 0 {
				issues = append(issues, Issue{
					Code:    IssueRowAbsentVals,
					Stage:   doc.Stage,
					Row:     row.ID,
					Field:   where,
					Message: "an absent row cannot also carry an implementation, params or requires",
				})
				continue
			}
			effective[position] = EffectiveRow{
				Row:        Row{ID: row.ID, Layer: inherited.Layer, Absent: true},
				Origin:     inherited.Origin,
				Source:     doc.Stage,
				Overridden: true,
			}
			continue
		}

		if strings.TrimSpace(row.Impl) == "" {
			issues = append(issues, Issue{
				Code:    IssueRowImpl,
				Stage:   doc.Stage,
				Row:     row.ID,
				Field:   where + ".impl",
				Message: "a present row must name an implementation",
			})
			continue
		}
		if len(row.Requires) > 0 {
			issues = append(issues, Issue{
				Code:    IssueRowRequires,
				Stage:   doc.Stage,
				Row:     row.ID,
				Field:   where + ".requires",
				Message: "an override replaces implementation and params; requires is declared in the base stage",
			})
			continue
		}
		if paramIssues := validateParams(row.Params, where); len(paramIssues) > 0 {
			issues = append(issues, paramIssues...)
			continue
		}
		effective[position] = EffectiveRow{
			Row: Row{
				ID:     row.ID,
				Layer:  inherited.Layer,
				Impl:   row.Impl,
				Params: cloneParams(row.Params),
			},
			Origin:     inherited.Origin,
			Source:     doc.Stage,
			Overridden: true,
		}
	}
	return issues
}

func validateParams(params map[string]string, where string) []Issue {
	if len(params) == 0 {
		return nil
	}
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var issues []Issue
	for _, key := range keys {
		if !paramKeyPattern.MatchString(key) {
			issues = append(issues, Issue{
				Code:    IssueRowParams,
				Row:     "",
				Field:   fmt.Sprintf("%s.params.%s", where, key),
				Message: fmt.Sprintf("invalid param key %q", key),
			})
		}
	}
	return issues
}

func validateRequires(self RowID, requires []RowID, where string) ([]RowID, []Issue) {
	seen := map[RowID]bool{}
	out := make([]RowID, 0, len(requires))
	var issues []Issue
	for i, requirement := range requires {
		at := fmt.Sprintf("%s.requires[%d]", where, i)
		if !requirement.Valid() {
			issues = append(issues, Issue{
				Code:    IssueRowRequires,
				Row:     self,
				Field:   at,
				Message: fmt.Sprintf("invalid required row id %q", requirement),
			})
			continue
		}
		if self != "" && requirement == self {
			issues = append(issues, Issue{
				Code:    IssueRowRequires,
				Row:     self,
				Field:   at,
				Message: "a row cannot require itself",
			})
			continue
		}
		if seen[requirement] {
			issues = append(issues, Issue{
				Code:    IssueRowRequires,
				Row:     self,
				Field:   at,
				Message: fmt.Sprintf("required row id %q is listed twice", requirement),
			})
			continue
		}
		seen[requirement] = true
		out = append(out, requirement)
	}
	return out, issues
}

func validateEffectiveRequirements(effective []EffectiveRow, byID map[RowID]int) []Issue {
	var issues []Issue
	for i, row := range effective {
		if row.Absent {
			continue
		}
		for j, requirement := range row.Requires {
			if _, ok := byID[requirement]; !ok {
				issues = append(issues, Issue{
					Code:    IssueRowRequires,
					Stage:   row.Source,
					Row:     row.ID,
					Field:   fmt.Sprintf("effective[%d].requires[%d]", i, j),
					Message: fmt.Sprintf("required row %q is not declared", requirement),
				})
			}
		}
	}
	issues = append(issues, detectCycles(effective, byID)...)
	return issues
}

func detectCycles(effective []EffectiveRow, byID map[RowID]int) []Issue {
	const (
		white = iota
		gray
		black
	)
	state := make([]int, len(effective))
	var stack []int
	var issues []Issue
	reported := map[string]bool{}

	var visit func(int)
	visit = func(index int) {
		state[index] = gray
		stack = append(stack, index)
		for _, requirement := range effective[index].Requires {
			next, ok := byID[requirement]
			if !ok || effective[next].Absent {
				continue
			}
			switch state[next] {
			case white:
				visit(next)
			case gray:
				start := 0
				for start < len(stack) && stack[start] != next {
					start++
				}
				cycle := append([]int(nil), stack[start:]...)
				key := cycleKey(effective, cycle)
				if reported[key] {
					continue
				}
				reported[key] = true
				ids := make([]string, 0, len(cycle))
				for _, member := range cycle {
					ids = append(ids, string(effective[member].ID))
				}
				issues = append(issues, Issue{
					Code:    IssueRowCycle,
					Stage:   effective[cycle[0]].Source,
					Row:     effective[cycle[0]].ID,
					Field:   fmt.Sprintf("effective[%d].requires", cycle[0]),
					Message: "requirement cycle: " + strings.Join(ids, " -> "),
				})
			}
		}
		stack = stack[:len(stack)-1]
		state[index] = black
	}

	for i := range effective {
		if effective[i].Absent || state[i] != white {
			continue
		}
		visit(i)
	}
	return issues
}

func cycleKey(effective []EffectiveRow, cycle []int) string {
	ids := make([]string, 0, len(cycle))
	for _, member := range cycle {
		ids = append(ids, string(effective[member].ID))
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

func computeActivation(effective []EffectiveRow, byID map[RowID]int) ([]Step, []BlockedRow) {
	blockedReason := make([]string, len(effective))
	blockedMissing := make([][]RowID, len(effective))

	for changed := true; changed; {
		changed = false
		for i, row := range effective {
			if row.Absent || blockedReason[i] != "" {
				continue
			}
			var missing []RowID
			reason := ""
			for _, requirement := range row.Requires {
				next, ok := byID[requirement]
				if !ok {
					missing = append(missing, requirement)
					reason = BlockedReasonAbsent
					continue
				}
				if effective[next].Absent {
					missing = append(missing, requirement)
					reason = BlockedReasonAbsent
					continue
				}
				if blockedReason[next] != "" {
					missing = append(missing, requirement)
					if reason == "" {
						reason = BlockedReasonDependency
					}
				}
			}
			if len(missing) > 0 {
				blockedReason[i] = reason
				blockedMissing[i] = dedupeRowIDs(missing)
				changed = true
			}
		}
	}

	active := make([]bool, len(effective))
	remaining := 0
	for i, row := range effective {
		if row.Absent || blockedReason[i] != "" {
			continue
		}
		active[i] = true
		remaining++
	}

	indegree := make([]int, len(effective))
	reverse := make([][]int, len(effective))
	for i, row := range effective {
		if !active[i] {
			continue
		}
		for _, requirement := range row.Requires {
			next, ok := byID[requirement]
			if !ok || !active[next] {
				continue
			}
			indegree[i]++
			reverse[next] = append(reverse[next], i)
		}
	}

	done := make([]bool, len(effective))
	steps := make([]Step, 0, remaining)
	for len(steps) < remaining {
		picked := -1
		for i := range effective {
			if active[i] && !done[i] && indegree[i] == 0 {
				picked = i
				break
			}
		}
		if picked < 0 {
			break
		}
		done[picked] = true
		steps = append(steps, Step{
			Index: picked,
			ID:    effective[picked].ID,
			Row:   effective[picked].clone(),
		})
		for _, dependent := range reverse[picked] {
			indegree[dependent]--
		}
	}

	var blocked []BlockedRow
	for i, row := range effective {
		if blockedReason[i] == "" {
			continue
		}
		blocked = append(blocked, BlockedRow{
			Index:   i,
			ID:      row.ID,
			Missing: append([]RowID(nil), blockedMissing[i]...),
			Reason:  blockedReason[i],
		})
	}
	// A cycle should have been rejected by Build. Keep the activation result
	// honest if a future caller builds an Assembly by other means: remaining
	// rows are unavailable, not silently activated.
	for i, row := range effective {
		if !active[i] || done[i] {
			continue
		}
		blocked = append(blocked, BlockedRow{
			Index:  i,
			ID:     row.ID,
			Reason: BlockedReasonCycle,
		})
	}
	return steps, blocked
}

func dedupeRowIDs(ids []RowID) []RowID {
	seen := map[RowID]bool{}
	out := make([]RowID, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
