// Package skills implements the Agent Skills host layer for Keel.
//
// Ported from PI packages/coding-agent/src/core/skills.ts and
// packages/coding-agent/src/utils/frontmatter.ts at commit 55a5c69 (MIT
// License, Copyright (c) 2025 Mario Zechner). Keel keeps PI's discovery,
// validation, collision and progressive-disclosure semantics; the
// implementation is a Go rewrite with no Node dependencies.
//
// Spec: https://agentskills.io/specification
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Limits from the Agent Skills specification.
const (
	// MaxNameLength is the maximum skill name length in characters.
	MaxNameLength = 64
	// MaxDescriptionLength is the maximum description length in characters.
	MaxDescriptionLength = 1024
	// MaxCompatibilityLength is the maximum compatibility length in characters.
	MaxCompatibilityLength = 500
	// SkillFileName is the portable skill definition file name.
	SkillFileName = "SKILL.md"
)

var validNameChars = regexp.MustCompile(`^[a-z0-9-]+$`)

// Skill is one parsed Agent Skills definition.
//
// Path is the path to the SKILL.md file (or, for a standalone Markdown skill,
// the .md file). Body is the Markdown after the frontmatter. Hash is the
// SHA-256 of the raw file bytes (frontmatter plus body) — the identity used
// for content deduplication and immutable versions.
type Skill struct {
	Name                   string
	Description            string
	Path                   string
	License                string
	Compatibility          string
	Metadata               map[string]string
	AllowedTools           []string
	DisableModelInvocation bool
	Body                   string
	Hash                   string
}

// Dir returns the skill directory (the parent of SKILL.md). Relative paths in
// the body resolve against this directory.
func (s Skill) Dir() string {
	return filepath.Dir(s.Path)
}

// Warning is a non-fatal problem found while parsing or discovering skills.
// Parsing never aborts on invalid fields: every problem is reported here.
type Warning struct {
	// Path is the SKILL.md (or standalone Markdown) path the warning is about.
	Path string
	// Message is a human-readable description of the problem.
	Message string
}

// String renders the warning as "path: message".
func (w Warning) String() string {
	if w.Path == "" {
		return w.Message
	}
	return w.Path + ": " + w.Message
}

// ParseSKILLMD parses one SKILL.md document.
//
// Missing or empty description means the skill is not loadable: it returns
// (nil, warnings, nil) with a "description is required" warning, mirroring
// PI (skills.ts:304-332). Invalid name/description values produce warnings
// but still return a skill, as PI does. A malformed YAML frontmatter returns
// an error and no skill.
//
// path is stored on the result and is also used to derive the default name
// from the parent directory when the frontmatter omits name (skills.ts:319-321).
func ParseSKILLMD(raw []byte, path string) (*Skill, []Warning, error) {
	return parseSkillFile(raw, path, true)
}

// parseSkillFile is the shared implementation for SKILL.md documents and
// standalone Markdown skills. declared reports whether the file is a
// SKILL.md; non-declared files without a description are skipped silently,
// matching PI (skills.ts:304-308).
func parseSkillFile(raw []byte, path string, declared bool) (*Skill, []Warning, error) {
	content := normalizeContent(string(raw))
	yamlText, body, hasFrontmatter := splitFrontmatter(content)

	fields, err := decodeFrontmatter(yamlText, hasFrontmatter)
	if err != nil {
		return nil, nil, err
	}

	var warnings []Warning
	warn := func(format string, args ...any) {
		warnings = append(warnings, Warning{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	description, descriptionOK := stringField(fields["description"])
	if !descriptionOK || strings.TrimSpace(description) == "" {
		if declared {
			warn("description is required")
		}
		return nil, warnings, nil
	}
	if n := utf8.RuneCountInString(description); n > MaxDescriptionLength {
		warn("description exceeds %d characters (%d)", MaxDescriptionLength, n)
	}

	name, nameOK := stringField(fields["name"])
	if !nameOK || name == "" {
		// PI falls back to the parent directory name (skills.ts:319-321).
		name = filepath.Base(filepath.Dir(path))
	}
	validateName(name, warn)

	skill := &Skill{
		Name:        name,
		Description: description,
		Path:        path,
		Body:        body,
		Hash:        ContentHash(raw),
	}

	if node, ok := fields["license"]; ok && !isNull(node) {
		if value, ok := stringField(node); ok {
			skill.License = value
		} else {
			warn("license must be a string")
		}
	}

	if node, ok := fields["compatibility"]; ok && !isNull(node) {
		if value, ok := stringField(node); ok {
			skill.Compatibility = value
			if n := utf8.RuneCountInString(value); n > MaxCompatibilityLength {
				warn("compatibility exceeds %d characters (%d)", MaxCompatibilityLength, n)
			}
		} else {
			warn("compatibility must be a string")
		}
	}

	if node, ok := fields["metadata"]; ok && !isNull(node) {
		parseMetadata(node, skill, warn)
	}

	if node, ok := fields["allowed-tools"]; ok && !isNull(node) {
		parseAllowedTools(node, skill, warn)
	}

	if node, ok := fields["disable-model-invocation"]; ok && !isNull(node) {
		parseDisableModelInvocation(node, skill, warn)
	}

	return skill, warnings, nil
}

// validateName mirrors PI's validateName (skills.ts:92-112) and keeps the
// exact warning texts.
func validateName(name string, warn func(format string, args ...any)) {
	if n := utf8.RuneCountInString(name); n > MaxNameLength {
		warn("name exceeds %d characters (%d)", MaxNameLength, n)
	}
	if !validNameChars.MatchString(name) {
		warn("name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		warn("name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		warn("name must not contain consecutive hyphens")
	}
}

// decodeFrontmatter turns the YAML text into a field map. A YAML syntax error
// is returned as an error; PI reports it as a warning and skips the skill
// (skills.ts:293-302). Discover converts the error back into a warning.
func decodeFrontmatter(yamlText string, hasFrontmatter bool) (map[string]*yaml.Node, error) {
	fields := make(map[string]*yaml.Node)
	if !hasFrontmatter || strings.TrimSpace(yamlText) == "" {
		return fields, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}
	if len(doc.Content) == 0 {
		return fields, nil
	}

	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		// PI's parseFrontmatter does `parsed ?? {}`, so a null document is
		// equivalent to no frontmatter.
		return fields, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("parse frontmatter: frontmatter must be a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if _, exists := fields[key.Value]; exists {
			// PI's npm yaml parser rejects duplicate mapping keys by default;
			// yaml.v3 keeps them when decoding into a Node, so enforce it here.
			return nil, fmt.Errorf("parse frontmatter: mapping key %q already defined", key.Value)
		}
		fields[key.Value] = value
	}
	return fields, nil
}

// stringField returns the scalar's string value. Only YAML strings qualify;
// numbers and booleans do not (PI checks typeof === "string"). Timestamps are
// accepted because yaml.v3 tags plain dates as !!timestamp while PI's YAML 1.2
// core-schema parser returns them as strings; both implementations must see
// the same text.
func stringField(node *yaml.Node) (string, bool) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return "", false
	}
	if node.Tag != "!!str" && node.Tag != "!!timestamp" {
		return "", false
	}
	return node.Value, true
}

func isNull(node *yaml.Node) bool {
	return node == nil || (node.Kind == yaml.ScalarNode && node.Tag == "!!null")
}

func parseMetadata(node *yaml.Node, skill *Skill, warn func(format string, args ...any)) {
	if node.Kind != yaml.MappingNode {
		warn("metadata must be a mapping of string keys to string values")
		return
	}
	skill.Metadata = make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			warn("metadata keys must be strings")
			continue
		}
		if value.Kind != yaml.ScalarNode {
			warn("metadata value for %q must be a string", key.Value)
			continue
		}
		// Scalar values are stringified: the spec wants string values, but
		// unquoted numbers/booleans are common and PI ignores the field
		// entirely, so be tolerant rather than dropping the entry.
		skill.Metadata[key.Value] = value.Value
	}
}

func parseAllowedTools(node *yaml.Node, skill *Skill, warn func(format string, args ...any)) {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			warn("allowed-tools must be a string or a list of strings")
			return
		}
		// The spec defines allowed-tools as a space-separated string.
		skill.AllowedTools = strings.Fields(node.Value)
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
				warn("allowed-tools entries must be strings")
				continue
			}
			skill.AllowedTools = append(skill.AllowedTools, item.Value)
		}
	default:
		warn("allowed-tools must be a string or a list of strings")
	}
}

func parseDisableModelInvocation(node *yaml.Node, skill *Skill, warn func(format string, args ...any)) {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!bool" {
		value, err := strconv.ParseBool(node.Value)
		if err == nil {
			skill.DisableModelInvocation = value
			return
		}
	}
	// PI only treats a YAML boolean true as set (skills.ts:341). A non-boolean
	// value is reported instead of silently ignored.
	warn("disable-model-invocation must be a boolean")
}

// normalizeContent strips a UTF-8 BOM and normalizes CRLF/CR to LF, matching
// PI's parseFrontmatter (utils/frontmatter.ts:5-15).
func normalizeContent(content string) string {
	content = strings.TrimPrefix(content, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

// splitFrontmatter extracts the YAML frontmatter and the trimmed Markdown
// body. Without a leading "---" followed by a closing "\n---" line, the whole
// document is the body (PI utils/frontmatter.ts:7-21).
func splitFrontmatter(content string) (yamlText, body string, ok bool) {
	if !strings.HasPrefix(content, "---") {
		return "", content, false
	}
	rel := strings.Index(content[3:], "\n---")
	if rel < 0 {
		return "", content, false
	}
	end := 3 + rel // index of the newline that starts the closing line
	yamlEnd := end
	if yamlEnd < 4 {
		// Empty frontmatter ("---\n---"): PI's slice(4, 3) yields "".
		yamlEnd = 4
	}
	return content[4:yamlEnd], strings.TrimSpace(content[end+4:]), true
}

// ContentHash returns the lowercase hex SHA-256 of raw bytes. It is the
// skill's content identity (frontmatter plus body, byte-exact).
func ContentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Discover scans roots recursively for skills. A directory containing
// SKILL.md is a skill root and is not descended into further; other
// directories are walked. Root-level standalone .md files are accepted, as
// in PI (skills.ts:262-270). Hidden directories and node_modules are skipped
// (skills.ts:224-231). "~" at the start of a root is expanded to the user
// home directory.
//
// Duplicate names keep the first discovered skill and emit a
// `name "x" collision` warning (PI skills.ts:437-450). The same file reached
// through overlapping roots or symlinks is loaded once.
func Discover(roots []string) ([]Skill, []Warning) {
	d := &discoverer{
		names:     make(map[string]struct{}),
		seenFiles: make(map[string]struct{}),
		seenDirs:  make(map[string]struct{}),
	}
	for _, raw := range roots {
		root := expandTilde(strings.TrimSpace(raw))
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			// Explicit roots that do not exist are skipped silently, as
			// loadSkillsFromDir does for missing default directories
			// (skills.ts:183-185).
			continue
		}
		if info.IsDir() {
			d.scanDir(root, true)
			continue
		}
		if info.Mode().IsRegular() && strings.HasSuffix(root, ".md") {
			d.loadFile(root, filepath.Base(root) == SkillFileName)
		}
	}
	return d.skills, d.warnings
}

// DefaultRoots returns the standard Agent Skills roots for cwd: the user
// directory ~/.agents/skills first, then .agents/skills from cwd up to the
// git repository root (inclusive), mirroring PI's ancestor discovery
// (package-manager.ts:2653-2670, 500-522). Duplicates are removed.
func DefaultRoots(cwd string) []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".agents", "skills"))
	}

	dir, err := filepath.Abs(cwd)
	if err != nil {
		dir = cwd
	}
	gitRoot := findGitRoot(dir)
	for {
		roots = append(roots, filepath.Join(dir, ".agents", "skills"))
		if dir == gitRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dedupePaths(roots)
}

func findGitRoot(start string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := paths[:0]
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func expandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

type discoverer struct {
	skills    []Skill
	warnings  []Warning
	names     map[string]struct{}
	seenFiles map[string]struct{}
	seenDirs  map[string]struct{}
}

func (d *discoverer) scanDir(dir string, includeRootFiles bool) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	if _, ok := d.seenDirs[realDir]; ok {
		return
	}
	d.seenDirs[realDir] = struct{}{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	// If the directory contains SKILL.md, it is a skill root and PI does not
	// recurse further (skills.ts:160-166, 194-221).
	for _, entry := range entries {
		if entry.Name() != SkillFileName {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, ok := entryFileInfo(path, entry)
		if !ok || !info.Mode().IsRegular() {
			continue
		}
		d.loadFile(path, true)
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		path := filepath.Join(dir, name)
		info, ok := entryFileInfo(path, entry)
		if !ok {
			continue
		}
		if info.IsDir() {
			d.scanDir(path, false)
			continue
		}
		if includeRootFiles && info.Mode().IsRegular() && strings.HasSuffix(name, ".md") {
			d.loadFile(path, false)
		}
	}
}

func (d *discoverer) loadFile(path string, declared bool) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath = path
	}
	if _, ok := d.seenFiles[realPath]; ok {
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		d.warnings = append(d.warnings, Warning{Path: path, Message: err.Error()})
		return
	}

	skill, warnings, err := parseSkillFile(raw, path, declared)
	d.warnings = append(d.warnings, warnings...)
	if err != nil {
		// PI reports malformed SKILL.md as a diagnostic and skips it; a
		// standalone .md is skipped without a diagnostic (skills.ts:293-302).
		if declared {
			d.warnings = append(d.warnings, Warning{Path: path, Message: err.Error()})
		}
		return
	}
	if skill == nil {
		return
	}

	d.seenFiles[realPath] = struct{}{}
	if _, exists := d.names[skill.Name]; exists {
		// Keep the first skill, like PI (skills.ts:437-450).
		d.warnings = append(d.warnings, Warning{Path: path, Message: fmt.Sprintf("name %q collision", skill.Name)})
		return
	}
	d.names[skill.Name] = struct{}{}
	d.skills = append(d.skills, *skill)
}

// entryFileInfo resolves symlinks so callers see the target's mode.
func entryFileInfo(path string, entry os.DirEntry) (os.FileInfo, bool) {
	if entry.Type()&os.ModeSymlink != 0 {
		info, err := os.Stat(path)
		if err != nil {
			return nil, false
		}
		return info, true
	}
	info, err := entry.Info()
	if err != nil {
		return nil, false
	}
	return info, true
}

// FormatForPrompt renders the system-prompt fragment for progressive
// disclosure. It contains only name, description and path — never the body.
// Skills with DisableModelInvocation are omitted (PI formatSkillsForPrompt,
// skills.ts:358-388). The XML shape and escaping match PI exactly.
//
// fileReadTool names the tool that loads skill files: "read" and "bash"
// select their PI intro line; any other value (PI's "indirect") uses the
// tool-neutral line.
func FormatForPrompt(skills []Skill, fileReadTool string) string {
	visible := make([]Skill, 0, len(skills))
	for _, skill := range skills {
		if !skill.DisableModelInvocation {
			visible = append(visible, skill)
		}
	}
	if len(visible) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.\n")
	switch fileReadTool {
	case "read":
		b.WriteString("Use the read tool to load a skill's file when the task matches its description.\n")
	case "bash":
		b.WriteString("Use bash to load a skill's file when the task matches its description.\n")
	default:
		b.WriteString("Load a skill's file when the task matches its description.\n")
	}
	b.WriteString("Resolve relative paths against the skill's directory (parent of SKILL.md). Use absolute paths in tool calls.\n\n")
	b.WriteString("<available_skills>\n")
	for _, skill := range visible {
		b.WriteString("  <skill>\n")
		fmt.Fprintf(&b, "    <name>%s</name>\n", escapeXML(skill.Name))
		fmt.Fprintf(&b, "    <description>%s</description>\n", escapeXML(skill.Description))
		fmt.Fprintf(&b, "    <location>%s</location>\n", escapeXML(skill.Path))
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// Index is FormatForPrompt for a host that exposes a read tool. It is the
// progressive-disclosure fragment required by the Skills host contract.
func Index(skills []Skill) string {
	return FormatForPrompt(skills, "read")
}

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func escapeXML(value string) string {
	return xmlEscaper.Replace(value)
}
