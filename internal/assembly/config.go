package assembly

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jixoai/keel/internal/capturepolicy"
)

// ConfigFormatVersion is the config.toml schema version this package parses.
const ConfigFormatVersion = 1

const configHashDomain = "keel-assembly-config-v1"

// SettingFilesystemCaptureMode is the one setting the config.toml file carries
// today. It maps onto the tenant document's filesystem.capture.mode and is the
// only setting the run path resolves per attempt; other tenant settings remain
// on the admin JSON document until their file representation is defined.
const SettingFilesystemCaptureMode = "filesystem.capture.mode"

// Compaction settings the config.toml file may carry. They map onto the
// tenant document's compaction section; the file is the outermost deployment
// layer, so an operator can turn automatic checkpointing on (and tune its
// thresholds) without rewriting the admin JSON document.
const (
	SettingCompactionEnabled          = "compaction.enabled"
	SettingCompactionThreshold        = "compaction.threshold"
	SettingCompactionKeepRecentTokens = "compaction.keepRecentTokens"
	SettingCompactionThresholdTokens  = "compaction.thresholdTokens"
	SettingCompactionMaxCheckpoints   = "compaction.maxCheckpoints"
	SettingCompactionTTLHours         = "compaction.ttlHours"
	SettingCompactionManualCompact    = "compaction.manualCompact"
)

// ConfigSettings is the typed settings table of a config.toml document. Keys
// use the tenant document's dotted names so the file and the admin JSON
// document share one vocabulary.
type ConfigSettings struct {
	Values map[string]string
}

// Get returns one setting value.
func (s ConfigSettings) Get(key string) (string, bool) {
	if s.Values == nil {
		return "", false
	}
	value, ok := s.Values[key]
	return value, ok
}

// Empty reports whether the document declares no settings.
func (s ConfigSettings) Empty() bool { return len(s.Values) == 0 }

// Config is one parsed config.toml document: the fourth assembly stage.
//
// The document carries only overrides. Every row must name an id declared by an
// earlier stage; the base bundle is the only document that adds rows.
type Config struct {
	// Version is the config.toml schema version (must be ConfigFormatVersion).
	Version int
	// Rows are the row overrides in declaration order.
	Rows []Row
	// Settings are the scalar settings the file carries (today only
	// filesystem.capture.mode). They are not assembly rows: they overlay the
	// tenant document, not the service graph.
	Settings ConfigSettings
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
	keys := make([]string, 0, len(c.Settings.Values))
	for key := range c.Settings.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		hash.Write([]byte(key))
		hash.Write([]byte{0})
		hash.Write([]byte(c.Settings.Values[key]))
		hash.Write([]byte{0})
	}
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
	issues = append(issues, validateConfigSettings(config.Settings)...)
	return issues
}

func validateConfigSettings(settings ConfigSettings) []Issue {
	if settings.Empty() {
		return nil
	}
	keys := make([]string, 0, len(settings.Values))
	for key := range settings.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var issues []Issue
	for _, key := range keys {
		where := "config.toml." + key
		value := settings.Values[key]
		switch key {
		case SettingCompactionEnabled, SettingCompactionManualCompact:
			if value != "true" && value != "false" {
				issues = append(issues, Issue{Code: IssueConfigValue, Stage: StageConfig, Field: where,
					Message: fmt.Sprintf("%s must be true or false", key)})
			}
		case SettingCompactionThreshold:
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || parsed <= 0 || parsed > 1 {
				issues = append(issues, Issue{Code: IssueConfigValue, Stage: StageConfig, Field: where,
					Message: fmt.Sprintf("%s must be in (0, 1]", key)})
			}
		case SettingCompactionKeepRecentTokens, SettingCompactionThresholdTokens,
			SettingCompactionMaxCheckpoints, SettingCompactionTTLHours:
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				issues = append(issues, Issue{Code: IssueConfigValue, Stage: StageConfig, Field: where,
					Message: fmt.Sprintf("%s must be a non-negative integer", key)})
			}
		case SettingFilesystemCaptureMode:
			if value == "" || strings.TrimSpace(value) != value {
				issues = append(issues, Issue{
					Code:    IssueConfigValue,
					Stage:   StageConfig,
					Field:   where,
					Message: fmt.Sprintf("%s must be one of per-tool-call|checkpoint|manual", key),
				})
				continue
			}
			if _, err := capturepolicy.ParseCaptureMode(value); err != nil {
				issues = append(issues, Issue{
					Code:    IssueConfigValue,
					Stage:   StageConfig,
					Field:   where,
					Message: fmt.Sprintf("%s %q (want per-tool-call|checkpoint|manual)", key, value),
				})
			}
		default:
			issues = append(issues, Issue{
				Code:    IssueConfigKey,
				Stage:   StageConfig,
				Field:   where,
				Message: fmt.Sprintf("unknown config setting %q", key),
			})
		}
	}
	return issues
}

func cloneConfig(config Config) Config {
	config.Rows = cloneRows(config.Rows)
	if config.Settings.Values != nil {
		values := make(map[string]string, len(config.Settings.Values))
		for key, value := range config.Settings.Values {
			values[key] = value
		}
		config.Settings.Values = values
	}
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
//	[filesystem.capture]
//	mode = "checkpoint"
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
	seenSettings := map[string]bool{}
	seenCaptureTable := false
	seenCompactionTable := false

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
			if line == "[filesystem.capture]" {
				if seenCaptureTable {
					issue(IssueConfigDuplicate, lineNo, "[filesystem.capture] appears twice")
					section = "table"
					continue
				}
				seenCaptureTable = true
				section = "capture"
				continue
			}
			if line == "[compaction]" {
				if seenCompactionTable {
					issue(IssueConfigDuplicate, lineNo, "[compaction] appears twice")
					section = "table"
					continue
				}
				seenCompactionTable = true
				section = "compaction"
				continue
			}
			issue(IssueConfigTable, lineNo, "unsupported table header %q (only [[rows]], [filesystem.capture] and [compaction] are defined)", line)
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
		case "capture":
			if key != "mode" {
				issue(IssueConfigKey, lineNo, "unknown [filesystem.capture] key %q", key)
				continue
			}
			if seenSettings[SettingFilesystemCaptureMode] {
				issue(IssueConfigDuplicate, lineNo, "filesystem.capture.mode is declared twice")
				continue
			}
			value, ok := parseTOMLString(rawValue)
			if !ok {
				issue(IssueConfigValue, lineNo, "filesystem.capture.mode must be a string")
				continue
			}
			if value == "" || strings.TrimSpace(value) != value {
				issue(IssueConfigValue, lineNo, "filesystem.capture.mode must be per-tool-call|checkpoint|manual without surrounding whitespace")
				continue
			}
			if _, err := capturepolicy.ParseCaptureMode(value); err != nil {
				issue(IssueConfigValue, lineNo, "filesystem.capture.mode %q (want per-tool-call|checkpoint|manual)", value)
				continue
			}
			seenSettings[SettingFilesystemCaptureMode] = true
			if config.Settings.Values == nil {
				config.Settings.Values = map[string]string{}
			}
			config.Settings.Values[SettingFilesystemCaptureMode] = value
		case "compaction":
			setting, ok := compactionSettingKey(key)
			if !ok {
				issue(IssueConfigKey, lineNo, "unknown [compaction] key %q", key)
				continue
			}
			value, message := parseCompactionSetting(key, rawValue)
			if message != "" {
				issue(IssueConfigValue, lineNo, "%s", message)
				continue
			}
			if seenSettings[setting] {
				issue(IssueConfigDuplicate, lineNo, "%s is declared twice", setting)
				continue
			}
			seenSettings[setting] = true
			if config.Settings.Values == nil {
				config.Settings.Values = map[string]string{}
			}
			config.Settings.Values[setting] = value
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

// compactionSettingKey maps a [compaction] key to its tenant-document name.
func compactionSettingKey(key string) (string, bool) {
	switch key {
	case "enabled":
		return SettingCompactionEnabled, true
	case "threshold":
		return SettingCompactionThreshold, true
	case "keepRecentTokens":
		return SettingCompactionKeepRecentTokens, true
	case "thresholdTokens":
		return SettingCompactionThresholdTokens, true
	case "maxCheckpoints":
		return SettingCompactionMaxCheckpoints, true
	case "ttlHours":
		return SettingCompactionTTLHours, true
	case "manualCompact":
		return SettingCompactionManualCompact, true
	default:
		return "", false
	}
}

// parseCompactionSetting validates one [compaction] value and returns its
// canonical text. Booleans are true|false; threshold is a fraction in (0,1];
// the rest are non-negative integers.
func parseCompactionSetting(key, raw string) (string, string) {
	value := strings.TrimSpace(raw)
	switch key {
	case "enabled", "manualCompact":
		if value != "true" && value != "false" {
			return "", fmt.Sprintf("compaction.%s must be true or false", key)
		}
		return value, ""
	case "threshold":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed <= 0 || parsed > 1 {
			return "", "compaction.threshold must be in (0, 1]"
		}
		return strconv.FormatFloat(parsed, 'g', -1, 64), ""
	default:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return "", fmt.Sprintf("compaction.%s must be a non-negative integer", key)
		}
		return strconv.FormatInt(parsed, 10), ""
	}
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
