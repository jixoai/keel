package assembly

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ConfigFormatVersion is the config.toml schema version this package parses.
const ConfigFormatVersion = 1

const configHashDomain = "keel-assembly-config-v1"

// Config is one parsed config.toml document: the fourth assembly stage.
//
// The document carries only overrides. Every row must name an id declared by an
// earlier stage; the base bundle is the only document that adds rows.
type Config struct {
	// Version is the config.toml schema version (must be ConfigFormatVersion).
	Version int
	// Rows are the row overrides in declaration order.
	Rows []Row
	// Source is a human label for issue paths (defaults to "config.toml" when
	// parsed).
	Source string
}

// Document projects the config onto its assembly stage.
func (c Config) Document() Document {
	return Document{Stage: StageConfig, Name: "config.toml", Rows: cloneRows(c.Rows)}
}

// Validate checks the document shape without resolving it against a base. It is
// what rotation construction uses; Build additionally checks that every row id
// exists in an earlier stage and that overlay order is ascending.
func (c Config) Validate() error {
	if issues := validateConfigShape(c); len(issues) > 0 {
		return &Error{Issues: issues}
	}
	return nil
}

// Hash returns the canonical content digest of the configuration. It is
// independent of whitespace, comments, key order within a row and row order:
// reordering rows does not change the effective configuration, so it must not
// manufacture a new configuration identity. (Rotation still advances its
// revision for every ledger event; see Rotation.)
func (c Config) Hash() string {
	rows := append([]Row(nil), c.Rows...)
	sort.Slice(rows, func(i, j int) bool {
		return canonicalConfigRow(rows[i]) < canonicalConfigRow(rows[j])
	})
	hash := sha256.New()
	hash.Write([]byte(configHashDomain))
	hash.Write([]byte{0})
	fmt.Fprintf(hash, "version=%d", c.Version)
	hash.Write([]byte{0})
	for _, row := range rows {
		hash.Write([]byte(canonicalConfigRow(row)))
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func canonicalConfigRow(row Row) string {
	var b strings.Builder
	b.WriteString(string(row.ID))
	b.WriteString("\x00")
	b.WriteString(string(row.Layer))
	b.WriteString("\x00")
	b.WriteString(row.Impl)
	b.WriteString("\x00")
	if row.Absent {
		b.WriteString("absent")
	} else {
		b.WriteString("present")
	}
	b.WriteString("\x00")
	for _, requirement := range row.Requires {
		b.WriteString(string(requirement))
		b.WriteString("\x00")
	}
	keys := make([]string, 0, len(row.Params))
	for key := range row.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(row.Params[key])
		b.WriteString("\x00")
	}
	return b.String()
}

// ConfigVersion is one immutable configuration version: a content digest, the
// ledger event sequence that made it active, and a monotonic revision.
type ConfigVersion struct {
	// Revision advances once per rotation, even when the document content is
	// unchanged: the rotation event itself is the version fact.
	Revision int64
	// Hash is the canonical content digest (Config.Hash).
	Hash string
	// AnchorEventSeq is the ledger event sequence at which this version became
	// active. A run pins this number together with Hash.
	AnchorEventSeq int64
}

func (v ConfigVersion) String() string {
	return fmt.Sprintf("revision=%d anchor=%d hash=%s", v.Revision, v.AnchorEventSeq, v.Hash)
}

// Pinned is one configuration version plus its parsed document. A run holds a
// Pinned value; rotations never mutate it, so in-flight work keeps the version
// it started with.
type Pinned struct {
	// Config is the immutable configuration document.
	Config Config
	// Version is the identity a run records (configHash + anchorEventSeq).
	Version ConfigVersion
}

// Document projects the pinned configuration onto the config stage.
func (p Pinned) Document() Document { return p.Config.Document() }

// Hash returns the pinned content digest.
func (p Pinned) Hash() string { return p.Version.Hash }

// Rotation is the config.toml rotation state machine. It is a plain value with
// explicit methods: no goroutines, locks, clocks or IO. The hosting process
// owns when a rotation happens (a ledger event) and supplies its anchor.
//
// The contract: a rotation writes a new file atomically with rename, assigns the
// ledger anchor before the rename, and becomes effective for the next run; a run
// that already started keeps its Pinned value.
type Rotation struct {
	current     Pinned
	previous    Pinned
	hasPrevious bool
}

// NewRotation starts a rotation history at revision 1.
func NewRotation(config Config, anchorEventSeq int64) (*Rotation, error) {
	if issues := validateRotation(config, anchorEventSeq); len(issues) > 0 {
		return nil, &Error{Issues: issues}
	}
	normalized := cloneConfig(config)
	return &Rotation{current: Pinned{
		Config: normalized,
		Version: ConfigVersion{
			Revision:       1,
			Hash:           normalized.Hash(),
			AnchorEventSeq: anchorEventSeq,
		},
	}}, nil
}

// Pin returns the version a new run must use. The value is a deep copy: later
// rotations cannot change an in-flight run.
func (r *Rotation) Pin() Pinned {
	if r == nil {
		return Pinned{}
	}
	return clonePinned(r.current)
}

// Current returns the active version identity.
func (r *Rotation) Current() ConfigVersion {
	if r == nil {
		return ConfigVersion{}
	}
	return r.current.Version
}

// Previous returns the version that was active before the last rotation.
func (r *Rotation) Previous() (Pinned, bool) {
	if r == nil || !r.hasPrevious {
		return Pinned{}, false
	}
	return clonePinned(r.previous), true
}

// Rotate makes config the next version. The anchor must be a ledger event
// sequence strictly greater than the current one: a rotation without a new
// ledger anchor has no place to point a run at.
func (r *Rotation) Rotate(config Config, anchorEventSeq int64) (ConfigVersion, error) {
	if r == nil {
		return ConfigVersion{}, &Error{Issues: []Issue{{
			Code:    IssueConfigAnchor,
			Message: "cannot rotate a nil configuration history",
		}}}
	}
	if issues := validateRotation(config, anchorEventSeq); len(issues) > 0 {
		return ConfigVersion{}, &Error{Issues: issues}
	}
	if anchorEventSeq <= r.current.Version.AnchorEventSeq {
		return ConfigVersion{}, &Error{Issues: []Issue{{
			Code:    IssueConfigAnchor,
			Stage:   StageConfig,
			Field:   "anchorEventSeq",
			Message: fmt.Sprintf("rotation anchor %d must be greater than the current anchor %d", anchorEventSeq, r.current.Version.AnchorEventSeq),
		}}}
	}
	normalized := cloneConfig(config)
	next := Pinned{
		Config: normalized,
		Version: ConfigVersion{
			Revision:       r.current.Version.Revision + 1,
			Hash:           normalized.Hash(),
			AnchorEventSeq: anchorEventSeq,
		},
	}
	r.previous, r.hasPrevious = r.current, true
	r.current = next
	return next.Version, nil
}

func validateRotation(config Config, anchorEventSeq int64) []Issue {
	issues := validateConfigShape(config)
	if anchorEventSeq < 0 {
		issues = append(issues, Issue{
			Code:    IssueConfigAnchor,
			Stage:   StageConfig,
			Field:   "anchorEventSeq",
			Message: "a configuration anchor cannot be negative",
		})
	}
	return issues
}

func validateConfigShape(config Config) []Issue {
	var issues []Issue
	if config.Version != ConfigFormatVersion {
		issues = append(issues, Issue{
			Code:    IssueConfigVersion,
			Stage:   StageConfig,
			Field:   "config.version",
			Message: fmt.Sprintf("config schema version must be %d, got %d", ConfigFormatVersion, config.Version),
		})
	}
	seen := map[RowID]bool{}
	for i, row := range config.Rows {
		where := fmt.Sprintf("config.rows[%d]", i)
		if row.ID == "" {
			issues = append(issues, Issue{
				Code:    IssueConfigEmptyRow,
				Stage:   StageConfig,
				Field:   where + ".id",
				Message: "a config row must name a row id",
			})
			continue
		}
		if !row.ID.Valid() {
			issues = append(issues, Issue{
				Code:    IssueRowID,
				Stage:   StageConfig,
				Row:     row.ID,
				Field:   where + ".id",
				Message: fmt.Sprintf("invalid row id %q (want <plugin-id>#<row>)", row.ID),
			})
			continue
		}
		if seen[row.ID] {
			issues = append(issues, Issue{
				Code:    IssueConfigDuplicate,
				Stage:   StageConfig,
				Row:     row.ID,
				Field:   where + ".id",
				Message: fmt.Sprintf("row id %q appears twice in config", row.ID),
			})
			continue
		}
		seen[row.ID] = true
		if row.Layer != "" || len(row.Requires) > 0 {
			issues = append(issues, Issue{
				Code:    IssueConfigKey,
				Stage:   StageConfig,
				Row:     row.ID,
				Field:   where,
				Message: "config rows may only override id, impl, absent and params",
			})
		}
		if row.Absent {
			if row.Impl != "" {
				issues = append(issues, Issue{
					Code:    IssueConfigAbsentImpl,
					Stage:   StageConfig,
					Row:     row.ID,
					Field:   where + ".impl",
					Message: "an absent config row cannot carry an implementation",
				})
			}
			if len(row.Params) > 0 {
				issues = append(issues, Issue{
					Code:    IssueConfigParamsAbs,
					Stage:   StageConfig,
					Row:     row.ID,
					Field:   where + ".params",
					Message: "an absent config row cannot carry params",
				})
			}
		} else if strings.TrimSpace(row.Impl) == "" {
			issues = append(issues, Issue{
				Code:    IssueRowImpl,
				Stage:   StageConfig,
				Row:     row.ID,
				Field:   where + ".impl",
				Message: "a present config row must name an implementation",
			})
		}
		issues = append(issues, validateParams(row.Params, where)...)
	}
	return issues
}

func cloneConfig(config Config) Config {
	config.Rows = cloneRows(config.Rows)
	return config
}

func clonePinned(pinned Pinned) Pinned {
	pinned.Config = cloneConfig(pinned.Config)
	return pinned
}

// --- strict config.toml parsing -------------------------------------------

type configRowState struct {
	line int
	row  Row
	keys map[string]bool
}

// ParseConfig parses the strict config.toml subset documented in
// docs/ASSEMBLY.md:
//
//	version = 1
//
//	[[rows]]
//	id = "keel.shell#bash-local"
//	impl = "ssh"
//	params.host = "devbox"
//
// Unknown tables, unknown keys, wrong value types, duplicate keys/rows and a
// row that names no id are field-level errors with stable codes. Arrays, inline
// tables and multi-line strings are deliberately outside the subset; a config
// that needs them must not silently half-apply.
func ParseConfig(data []byte) (Config, error) {
	config := Config{Source: "config.toml"}
	var issues []Issue
	var current *configRowState
	section := "root"
	seenVersion := false
	versionRejected := false
	seenIDs := map[RowID]bool{}

	issue := func(code string, line int, format string, args ...any) {
		issues = append(issues, Issue{
			Code:    code,
			Stage:   StageConfig,
			Field:   fmt.Sprintf("config.toml:%d", line),
			Message: fmt.Sprintf(format, args...),
		})
	}

	finishRow := func() {
		if current == nil {
			return
		}
		row := current.row
		finish := func(issue Issue) {
			if issue.Row == "" {
				issue.Row = row.ID
			}
			if issue.Stage == "" {
				issue.Stage = StageConfig
			}
			issue.Field = fmt.Sprintf("config.toml:%d%s", current.line, issue.Field)
			issues = append(issues, issue)
		}
		if len(current.keys) == 0 {
			finish(Issue{Code: IssueConfigEmptyRow, Message: "empty [[rows]] table"})
			current = nil
			return
		}
		if row.ID == "" {
			finish(Issue{Code: IssueConfigEmptyRow, Field: ".id", Message: "a config row must name a row id"})
			current = nil
			return
		}
		if !row.ID.Valid() {
			finish(Issue{Code: IssueRowID, Field: ".id", Message: fmt.Sprintf("invalid row id %q (want <plugin-id>#<row>)", row.ID)})
			current = nil
			return
		}
		if seenIDs[row.ID] {
			finish(Issue{Code: IssueConfigDuplicate, Field: ".id", Message: fmt.Sprintf("row id %q appears twice in config", row.ID)})
			current = nil
			return
		}
		seenIDs[row.ID] = true
		if row.Absent {
			if row.Impl != "" {
				finish(Issue{Code: IssueConfigAbsentImpl, Field: ".impl", Message: "an absent config row cannot carry an implementation"})
			}
			if len(row.Params) > 0 {
				finish(Issue{Code: IssueConfigParamsAbs, Field: ".params", Message: "an absent config row cannot carry params"})
			}
		} else if strings.TrimSpace(row.Impl) == "" {
			finish(Issue{Code: IssueRowImpl, Field: ".impl", Message: "a present config row must name an implementation"})
		}
		for _, paramIssue := range validateParams(row.Params, fmt.Sprintf("config.toml:%d", current.line)) {
			issues = append(issues, paramIssue)
		}
		config.Rows = append(config.Rows, row)
		current = nil
	}

	for index, raw := range strings.Split(string(data), "\n") {
		lineNo := index + 1
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") {
			finishRow()
			if line != "[[rows]]" {
				issue(IssueConfigTable, lineNo, "unsupported array-of-tables header %q", line)
				section = "table"
				continue
			}
			current = &configRowState{line: lineNo, keys: map[string]bool{}}
			section = "rows"
			continue
		}
		if strings.HasPrefix(line, "[") {
			finishRow()
			issue(IssueConfigTable, lineNo, "unsupported table header %q (only [[rows]] is defined)", line)
			section = "table"
			continue
		}
		key, rawValue, ok := splitTOMLKeyValue(line)
		if !ok {
			issue(IssueConfigValue, lineNo, "expected key = value, got %q", line)
			continue
		}
		switch section {
		case "root":
			if key != "version" {
				issue(IssueConfigKey, lineNo, "unknown top-level key %q", key)
				continue
			}
			if seenVersion {
				issue(IssueConfigDuplicate, lineNo, "version is declared twice")
				continue
			}
			seenVersion = true
			version, err := strconv.ParseInt(strings.TrimSpace(rawValue), 10, 64)
			if err != nil || version != ConfigFormatVersion {
				versionRejected = true
				issue(IssueConfigVersion, lineNo, "version must be the integer %d", ConfigFormatVersion)
				continue
			}
			config.Version = int(version)
		case "table":
			// The table header was already rejected; do not pile up a second
			// issue for every key under it.
		case "rows":
			if current == nil {
				issue(IssueConfigKey, lineNo, "key %q appears outside a [[rows]] table", key)
				continue
			}
			parseConfigRowKey(issue, current, lineNo, key, rawValue)
		}
	}
	finishRow()

	if !seenVersion && !versionRejected {
		issues = append(issues, Issue{
			Code:    IssueConfigVersion,
			Stage:   StageConfig,
			Field:   "config.toml",
			Message: fmt.Sprintf("version = %d is required", ConfigFormatVersion),
		})
	}
	if len(issues) > 0 {
		return Config{}, &Error{Issues: issues}
	}
	return config, nil
}

func parseConfigRowKey(issue func(string, int, string, ...any), current *configRowState, lineNo int, key, rawValue string) {
	mark := func(name string) bool {
		if current.keys[name] {
			issue(IssueConfigDuplicate, lineNo, "%s is declared twice in this row", name)
			return false
		}
		current.keys[name] = true
		return true
	}
	switch {
	case key == "id":
		if !mark(key) {
			return
		}
		value, ok := parseTOMLString(rawValue)
		if !ok {
			issue(IssueConfigValue, lineNo, "id must be a string")
			return
		}
		current.row.ID = RowID(value)
	case key == "impl":
		if !mark(key) {
			return
		}
		value, ok := parseTOMLString(rawValue)
		if !ok {
			issue(IssueConfigValue, lineNo, "impl must be a string")
			return
		}
		current.row.Impl = value
	case key == "absent":
		if !mark(key) {
			return
		}
		switch strings.TrimSpace(rawValue) {
		case "true":
			current.row.Absent = true
		case "false":
			current.row.Absent = false
		default:
			issue(IssueConfigValue, lineNo, "absent must be true or false")
		}
	case strings.HasPrefix(key, "params."):
		if !mark(key) {
			return
		}
		param := strings.TrimPrefix(key, "params.")
		if !paramKeyPattern.MatchString(param) {
			issue(IssueConfigKey, lineNo, "invalid param key %q", param)
			return
		}
		value, ok := parseTOMLString(rawValue)
		if !ok {
			issue(IssueConfigValue, lineNo, "param %q must be a string", param)
			return
		}
		if current.row.Params == nil {
			current.row.Params = map[string]string{}
		}
		current.row.Params[param] = value
	default:
		issue(IssueConfigKey, lineNo, "unknown row key %q", key)
	}
}

func stripTOMLComment(line string) string {
	var quote rune
	escaped := false
	for i, char := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if char == '\\' && quote == '"' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			quote = char
		case '#':
			return line[:i]
		}
	}
	return line
}

func splitTOMLKeyValue(line string) (string, string, bool) {
	index := strings.IndexByte(line, '=')
	if index <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:index])
	if key == "" {
		return "", "", false
	}
	return key, strings.TrimSpace(line[index+1:]), true
}

func parseTOMLString(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return "", false
	}
	switch raw[0] {
	case '"':
		if raw[len(raw)-1] != '"' {
			return "", false
		}
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", false
		}
		return value, true
	case '\'':
		if raw[len(raw)-1] != '\'' {
			return "", false
		}
		value := raw[1 : len(raw)-1]
		if strings.Contains(value, "'") {
			return "", false
		}
		return value, true
	}
	return "", false
}
