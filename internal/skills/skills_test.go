package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from PI packages/coding-agent/test/skills.test.ts at commit 55a5c69.
// Cases that PI drives through loadSkillsFromDir are driven through Discover
// here (single-root discovery has the same semantics), and the
// formatSkillsForPrompt cases through FormatForPrompt/Index.

func testdata(t *testing.T, elem ...string) string {
	t.Helper()
	parts := append([]string{"testdata"}, elem...)
	path, err := filepath.Abs(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("abs %v: %v", parts, err)
	}
	return path
}

func fixtureSkill(t *testing.T, name string) (*Skill, []Warning) {
	t.Helper()
	skills, warnings := Discover([]string{testdata(t, "skills", name)})
	if len(skills) == 1 {
		return &skills[0], warnings
	}
	return nil, warnings
}

func hasWarning(warnings []Warning, substr string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning.Message, substr) {
			return true
		}
	}
	return false
}

func requireNoWarnings(t *testing.T, warnings []Warning) {
	t.Helper()
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

func requireSingleSkill(t *testing.T, skill *Skill, warnings []Warning) *Skill {
	t.Helper()
	if skill == nil {
		t.Fatalf("expected one skill, got none (warnings: %v)", warnings)
	}
	return skill
}

// PI: "should load a valid skill"
func TestLoadSkillsFromDir_ValidSkill(t *testing.T) {
	skill, warnings := fixtureSkill(t, "valid-skill")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.Name != "valid-skill" {
		t.Errorf("name = %q, want valid-skill", skill.Name)
	}
	if skill.Description != "A valid skill for testing purposes." {
		t.Errorf("description = %q", skill.Description)
	}
	requireNoWarnings(t, warnings)
}

// PI: "should allow names that don't match parent directory"
func TestLoadSkillsFromDir_NameNeedNotMatchDir(t *testing.T) {
	skill, warnings := fixtureSkill(t, "name-mismatch")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.Name != "different-name" {
		t.Errorf("name = %q, want different-name", skill.Name)
	}
	if hasWarning(warnings, "does not match parent directory") {
		t.Errorf("unexpected name/directory mismatch warning: %v", warnings)
	}
}

// PI: "should warn when name contains invalid characters"
func TestLoadSkillsFromDir_InvalidNameChars(t *testing.T) {
	skill, warnings := fixtureSkill(t, "invalid-name-chars")
	requireSingleSkill(t, skill, warnings)

	if !hasWarning(warnings, "invalid characters") {
		t.Errorf("expected invalid-characters warning, got %v", warnings)
	}
}

// PI: "should warn when name exceeds 64 characters"
func TestLoadSkillsFromDir_LongName(t *testing.T) {
	skill, warnings := fixtureSkill(t, "long-name")
	requireSingleSkill(t, skill, warnings)

	if !hasWarning(warnings, "exceeds 64 characters") {
		t.Errorf("expected name-length warning, got %v", warnings)
	}
}

// PI: "should warn and skip skill when description is missing"
func TestLoadSkillsFromDir_MissingDescription(t *testing.T) {
	skill, warnings := fixtureSkill(t, "missing-description")

	if skill != nil {
		t.Fatalf("expected no skill, got %+v", skill)
	}
	if !hasWarning(warnings, "description is required") {
		t.Errorf("expected description warning, got %v", warnings)
	}
}

// PI: "should ignore unknown frontmatter fields"
func TestLoadSkillsFromDir_UnknownFields(t *testing.T) {
	skill, warnings := fixtureSkill(t, "unknown-field")
	requireSingleSkill(t, skill, warnings)
	requireNoWarnings(t, warnings)
}

// PI: "should load nested skills recursively"
func TestLoadSkillsFromDir_Nested(t *testing.T) {
	skill, warnings := fixtureSkill(t, "nested")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.Name != "child-skill" {
		t.Errorf("name = %q, want child-skill", skill.Name)
	}
	requireNoWarnings(t, warnings)
}

// PI: "should prefer a directory's root SKILL.md over nested SKILL.md files"
func TestLoadSkillsFromDir_RootSkillPreferred(t *testing.T) {
	skill, warnings := fixtureSkill(t, "root-skill-preferred")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.Name != "root-skill-preferred" {
		t.Errorf("name = %q, want root-skill-preferred", skill.Name)
	}
	if skill.Description != "Root skill should win." {
		t.Errorf("description = %q", skill.Description)
	}
	requireNoWarnings(t, warnings)
}

// PI: "should skip files without frontmatter"
func TestLoadSkillsFromDir_NoFrontmatter(t *testing.T) {
	skill, warnings := fixtureSkill(t, "no-frontmatter")

	if skill != nil {
		t.Fatalf("expected no skill, got %+v", skill)
	}
	if !hasWarning(warnings, "description is required") {
		t.Errorf("expected description warning, got %v", warnings)
	}
}

// PI: "should warn and skip skill when YAML frontmatter is invalid"
func TestLoadSkillsFromDir_InvalidYAML(t *testing.T) {
	skill, warnings := fixtureSkill(t, "invalid-yaml")

	if skill != nil {
		t.Fatalf("expected no skill, got %+v", skill)
	}
	if !hasWarning(warnings, "line") {
		t.Errorf("expected YAML line error warning, got %v", warnings)
	}
}

// PI: "should preserve multiline descriptions from YAML"
func TestLoadSkillsFromDir_MultilineDescription(t *testing.T) {
	skill, warnings := fixtureSkill(t, "multiline-description")
	skill = requireSingleSkill(t, skill, warnings)

	if !strings.Contains(skill.Description, "\n") {
		t.Errorf("description lost newlines: %q", skill.Description)
	}
	if !strings.Contains(skill.Description, "This is a multiline description.") {
		t.Errorf("description = %q", skill.Description)
	}
	requireNoWarnings(t, warnings)
}

// PI: "should warn when name contains consecutive hyphens"
func TestLoadSkillsFromDir_ConsecutiveHyphens(t *testing.T) {
	skill, warnings := fixtureSkill(t, "consecutive-hyphens")
	requireSingleSkill(t, skill, warnings)

	if !hasWarning(warnings, "consecutive hyphens") {
		t.Errorf("expected consecutive-hyphens warning, got %v", warnings)
	}
}

// PI: "should load all skills from fixture directory"
func TestLoadSkillsFromDir_AllFixtures(t *testing.T) {
	skills, _ := Discover([]string{testdata(t, "skills")})
	if len(skills) < 6 {
		t.Fatalf("loaded %d skills, want at least 6", len(skills))
	}
}

// PI: "should return empty for non-existent directory"
func TestLoadSkillsFromDir_NonExistentDir(t *testing.T) {
	skills, warnings := Discover([]string{"/non/existent/path"})
	if len(skills) != 0 {
		t.Fatalf("expected no skills, got %v", skills)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

// PI: "should use parent directory name when name not in frontmatter"
func TestLoadSkillsFromDir_ParentDirFallback(t *testing.T) {
	skill, warnings := fixtureSkill(t, "root-skill-preferred")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.Name != "root-skill-preferred" {
		t.Errorf("name = %q, want directory-derived root-skill-preferred", skill.Name)
	}
}

// PI: "should parse disable-model-invocation frontmatter field"
func TestLoadSkillsFromDir_DisableModelInvocation(t *testing.T) {
	skill, warnings := fixtureSkill(t, "disable-model-invocation")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.Name != "disable-model-invocation" {
		t.Errorf("name = %q", skill.Name)
	}
	if !skill.DisableModelInvocation {
		t.Error("DisableModelInvocation = false, want true")
	}
	if hasWarning(warnings, "unknown frontmatter field") {
		t.Errorf("unexpected unknown-field warning: %v", warnings)
	}
}

// PI: "should default disableModelInvocation to false when not specified"
func TestLoadSkillsFromDir_DefaultDisableModelInvocation(t *testing.T) {
	skill, warnings := fixtureSkill(t, "valid-skill")
	skill = requireSingleSkill(t, skill, warnings)

	if skill.DisableModelInvocation {
		t.Error("DisableModelInvocation = true, want false")
	}
}

// Spec fields PI surfaces only in its docs: license, compatibility, metadata,
// allowed-tools. Covered with an inline document (PI has no fixture).
func TestParseSKILLMD_AllFrontmatterFields(t *testing.T) {
	raw := []byte(`---
name: pdf-processing
description: Extract PDF text, fill forms. Use when handling PDFs.
license: Apache-2.0
compatibility: Requires git, docker, jq, and access to the internet
metadata:
  author: example-org
  version: "1.0"
allowed-tools: Bash(git:*) Bash(jq:*) Read
disable-model-invocation: true
---

# PDF processing

Body instructions.
`)

	skill, warnings, err := ParseSKILLMD(raw, "/skills/pdf-processing/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if skill.Name != "pdf-processing" {
		t.Errorf("Name = %q", skill.Name)
	}
	if skill.License != "Apache-2.0" {
		t.Errorf("License = %q", skill.License)
	}
	if skill.Compatibility != "Requires git, docker, jq, and access to the internet" {
		t.Errorf("Compatibility = %q", skill.Compatibility)
	}
	if got := skill.Metadata["author"]; got != "example-org" {
		t.Errorf("Metadata[author] = %q", got)
	}
	if got := skill.Metadata["version"]; got != "1.0" {
		t.Errorf("Metadata[version] = %q", got)
	}
	wantTools := []string{"Bash(git:*)", "Bash(jq:*)", "Read"}
	if len(skill.AllowedTools) != len(wantTools) {
		t.Fatalf("AllowedTools = %v, want %v", skill.AllowedTools, wantTools)
	}
	for i, want := range wantTools {
		if skill.AllowedTools[i] != want {
			t.Errorf("AllowedTools[%d] = %q, want %q", i, skill.AllowedTools[i], want)
		}
	}
	if !skill.DisableModelInvocation {
		t.Error("DisableModelInvocation = false, want true")
	}
	if !strings.Contains(skill.Body, "# PDF processing") {
		t.Errorf("Body = %q", skill.Body)
	}
}

func TestParseSKILLMD_AllowedToolsList(t *testing.T) {
	raw := []byte("---\nname: list-tools\ndescription: Tools as a list.\nallowed-tools:\n  - Read\n  - Write\n---\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/list-tools/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := []string{"Read", "Write"}
	if len(skill.AllowedTools) != len(want) {
		t.Fatalf("AllowedTools = %v, want %v", skill.AllowedTools, want)
	}
	for i := range want {
		if skill.AllowedTools[i] != want[i] {
			t.Errorf("AllowedTools[%d] = %q, want %q", i, skill.AllowedTools[i], want[i])
		}
	}
}

func TestParseSKILLMD_InvalidFieldTypesWarn(t *testing.T) {
	raw := []byte("---\nname: bad-types\ndescription: Bad types.\nlicense: [a, b]\ncompatibility: 42\nmetadata: nope\nallowed-tools: 3\ndisable-model-invocation: \"true\"\n---\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/bad-types/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if skill == nil {
		t.Fatal("expected skill to load with warnings")
	}
	for _, want := range []string{
		"license must be a string",
		"compatibility must be a string",
		"metadata must be a mapping",
		"allowed-tools must be a string or a list of strings",
		"disable-model-invocation must be a boolean",
	} {
		if !hasWarning(warnings, want) {
			t.Errorf("missing warning %q in %v", want, warnings)
		}
	}
	if skill.DisableModelInvocation {
		t.Error("string \"true\" must not enable DisableModelInvocation (PI checks YAML boolean true)")
	}
}

func TestParseSKILLMD_DescriptionTooLong(t *testing.T) {
	atLimit := strings.Repeat("x", MaxDescriptionLength)
	raw := []byte("---\nname: long-description\ndescription: " + atLimit + "\n---\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/long-description/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if skill == nil || len(warnings) != 0 {
		t.Fatalf("description of exactly %d chars must load cleanly: skill=%v warnings=%v", MaxDescriptionLength, skill, warnings)
	}

	overLimit := strings.Repeat("x", MaxDescriptionLength+1)
	raw = []byte("---\nname: long-description\ndescription: " + overLimit + "\n---\n")
	skill, warnings, err = ParseSKILLMD(raw, "/skills/long-description/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if skill == nil {
		t.Fatal("a too-long description must warn, not reject the skill (PI keeps it)")
	}
	if !hasWarning(warnings, "description exceeds 1024 characters") {
		t.Errorf("expected description-length warning, got %v", warnings)
	}
}

func TestParseSKILLMD_HyphenNames(t *testing.T) {
	for _, name := range []string{"-leading", "trailing-"} {
		raw := []byte("---\nname: " + name + "\ndescription: Hyphen name.\n---\n")
		skill, warnings, err := ParseSKILLMD(raw, "/skills/"+name+"/SKILL.md")
		if err != nil {
			t.Fatalf("ParseSKILLMD(%q): %v", name, err)
		}
		if skill == nil {
			t.Fatalf("name %q should be kept with a warning", name)
		}
		if !hasWarning(warnings, "must not start or end with a hyphen") {
			t.Errorf("name %q: expected hyphen warning, got %v", name, warnings)
		}
	}
}

func TestParseSKILLMD_InvalidYAMLReturnsError(t *testing.T) {
	raw := []byte("---\nname: broken\ndescription: [unclosed bracket\n---\n")
	skill, _, err := ParseSKILLMD(raw, "/skills/broken/SKILL.md")
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if skill != nil {
		t.Fatalf("expected nil skill, got %+v", skill)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error = %q, want line information", err)
	}
}

func TestParseSKILLMD_DuplicateKeyReturnsError(t *testing.T) {
	raw := []byte("---\nname: first\ndescription: One.\nname: second\n---\n")
	skill, _, err := ParseSKILLMD(raw, "/skills/dup/SKILL.md")
	if err == nil {
		t.Fatal("expected error for duplicate frontmatter keys")
	}
	if skill != nil {
		t.Fatalf("expected nil skill, got %+v", skill)
	}
	if !strings.Contains(err.Error(), "already defined") {
		t.Errorf("error = %q, want duplicate-key detail", err)
	}
}

func TestParseSKILLMD_EmptyFrontmatter(t *testing.T) {
	// PI's frontmatter splitter yields "" for `---\n---`; it must not panic
	// and behaves like a document without a description.
	for _, raw := range []string{"---\n---", "---\n---\n\n# Body\n"} {
		skill, warnings, err := ParseSKILLMD([]byte(raw), "/skills/empty/SKILL.md")
		if err != nil {
			t.Fatalf("ParseSKILLMD(%q): %v", raw, err)
		}
		if skill != nil {
			t.Fatalf("expected nil skill for %q, got %+v", raw, skill)
		}
		if !hasWarning(warnings, "description is required") {
			t.Errorf("expected description warning for %q, got %v", raw, warnings)
		}
	}
}

func TestParseSKILLMD_NullFrontmatterDocument(t *testing.T) {
	raw := []byte("---\nnull\n---\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/null/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if skill != nil {
		t.Fatalf("expected nil skill, got %+v", skill)
	}
	if !hasWarning(warnings, "description is required") {
		t.Errorf("expected description warning, got %v", warnings)
	}
}

func TestParseSKILLMD_TimestampScalarIsString(t *testing.T) {
	// yaml.v3 tags plain dates as !!timestamp; PI's YAML 1.2 core-schema
	// parser returns a string, so the field must still be readable.
	raw := []byte("---\nname: 2020-01-01\ndescription: 2020-01-01\n---\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/date/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if skill.Name != "2020-01-01" || skill.Description != "2020-01-01" {
		t.Fatalf("skill = %+v", skill)
	}
}

func TestParseSKILLMD_MissingDescriptionNotLoaded(t *testing.T) {
	raw := []byte("---\nname: no-description\n---\n\n# Body\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/no-description/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if skill != nil {
		t.Fatalf("expected nil skill, got %+v", skill)
	}
	if !hasWarning(warnings, "description is required") {
		t.Errorf("expected description warning, got %v", warnings)
	}
}

func TestParseSKILLMD_Hash(t *testing.T) {
	raw := []byte("---\nname: hash-skill\ndescription: Stable hash.\n---\n\nBody one.\n")
	first, _, err := ParseSKILLMD(raw, "/skills/hash-skill/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	second, _, err := ParseSKILLMD(raw, "/skills/hash-skill/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if first.Hash != second.Hash {
		t.Errorf("hash not stable: %q vs %q", first.Hash, second.Hash)
	}
	if len(first.Hash) != 64 {
		t.Errorf("hash = %q, want 64 hex chars", first.Hash)
	}
	if first.Hash != ContentHash(raw) {
		t.Errorf("Hash = %q, want ContentHash(raw) = %q", first.Hash, ContentHash(raw))
	}

	changed := []byte("---\nname: hash-skill\ndescription: Stable hash.\n---\n\nBody two.\n")
	third, _, err := ParseSKILLMD(changed, "/skills/hash-skill/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if first.Hash == third.Hash {
		t.Error("body change did not change the hash")
	}
}

func TestParseSKILLMD_BOMAndCRLF(t *testing.T) {
	raw := []byte("\uFEFF---\r\nname: crlf-skill\r\ndescription: CRLF skill.\r\n---\r\n\r\nBody\r\n")
	skill, warnings, err := ParseSKILLMD(raw, "/skills/crlf-skill/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if skill.Name != "crlf-skill" {
		t.Errorf("Name = %q", skill.Name)
	}
	if skill.Body != "Body" {
		t.Errorf("Body = %q, want Body", skill.Body)
	}
	if skill.Hash != ContentHash(raw) {
		t.Errorf("Hash = %q, want raw-byte hash", skill.Hash)
	}
}

// PI: collision handling keeps the first skill and warns.
func TestDiscover_Collision(t *testing.T) {
	roots := []string{
		testdata(t, "skills-collision", "first"),
		testdata(t, "skills-collision", "second"),
	}
	skills, warnings := Discover(roots)
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "calendar" {
		t.Errorf("name = %q, want calendar", skills[0].Name)
	}
	if skills[0].Description != "First calendar skill." {
		t.Errorf("description = %q, want the first skill to win", skills[0].Description)
	}
	if !hasWarning(warnings, `name "calendar" collision`) {
		t.Errorf("expected collision warning, got %v", warnings)
	}
}

func TestDiscover_CollisionReverseOrder(t *testing.T) {
	roots := []string{
		testdata(t, "skills-collision", "second"),
		testdata(t, "skills-collision", "first"),
	}
	skills, _ := Discover(roots)
	if len(skills) != 1 || skills[0].Description != "Second calendar skill." {
		t.Fatalf("expected second to win when discovered first, got %+v", skills)
	}
}

func TestDiscover_SameFileTwiceIsNotACollision(t *testing.T) {
	dir := testdata(t, "skills", "valid-skill")
	skills, warnings := Discover([]string{dir, dir})
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if hasWarning(warnings, "collision") {
		t.Errorf("same file reported as collision: %v", warnings)
	}
}

func TestDiscover_OverlappingRoots(t *testing.T) {
	skillsRoot := testdata(t, "skills")
	skills, warnings := Discover([]string{skillsRoot, testdata(t, "skills", "valid-skill")})
	if len(skills) < 6 {
		t.Fatalf("expected fixture skills, got %d", len(skills))
	}
	if hasWarning(warnings, "collision") {
		t.Errorf("overlapping roots reported a collision: %v", warnings)
	}
}

func TestDiscover_SkipsHiddenDirsAndNodeModules(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, ".hidden", "hidden-skill"), "hidden-skill", "Hidden.")
	writeSkill(t, filepath.Join(root, "node_modules", "dep", "dep-skill"), "dep-skill", "Dependency.")
	writeSkill(t, filepath.Join(root, "visible", "visible-skill"), "visible-skill", "Visible.")

	skills, warnings := Discover([]string{root})
	if len(skills) != 1 || skills[0].Name != "visible-skill" {
		t.Fatalf("skills = %+v, want only visible-skill", skills)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

func TestDiscover_TildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSkill(t, filepath.Join(home, "skills", "tilde-skill"), "tilde-skill", "From home.")

	skills, warnings := Discover([]string{"~/skills"})
	if len(skills) != 1 || skills[0].Name != "tilde-skill" {
		t.Fatalf("skills = %+v, warnings = %v", skills, warnings)
	}
}

func TestDiscover_StandaloneMarkdownRootFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "solo.md")
	if err := os.WriteFile(path, []byte("---\nname: solo\ndescription: Standalone.\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills, warnings := Discover([]string{path})
	if len(skills) != 1 || skills[0].Name != "solo" {
		t.Fatalf("skills = %+v, warnings = %v", skills, warnings)
	}
}

func TestDefaultRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(repo, "packages", "app")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	roots := DefaultRoots(cwd)
	want := []string{
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
		filepath.Join(repo, "packages", ".agents", "skills"),
		filepath.Join(repo, ".agents", "skills"),
	}
	if len(roots) != len(want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Errorf("roots[%d] = %q, want %q", i, roots[i], want[i])
		}
	}
}

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n\nBody for " + name + ".\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testSkill(name, description, path string, disableModelInvocation bool) Skill {
	return Skill{
		Name:                   name,
		Description:            description,
		Path:                   path,
		DisableModelInvocation: disableModelInvocation,
	}
}

// PI: "should return empty string for no skills"
func TestFormatForPrompt_Empty(t *testing.T) {
	if got := FormatForPrompt(nil, "read"); got != "" {
		t.Errorf("FormatForPrompt(nil) = %q, want empty", got)
	}
	if got := Index(nil); got != "" {
		t.Errorf("Index(nil) = %q, want empty", got)
	}
}

// PI: "should format skills as XML"
func TestFormatForPrompt_XML(t *testing.T) {
	skills := []Skill{testSkill("test-skill", "A test skill.", "/path/to/skill/SKILL.md", false)}
	got := FormatForPrompt(skills, "read")

	for _, want := range []string{
		"<available_skills>",
		"</available_skills>",
		"<skill>",
		"<name>test-skill</name>",
		"<description>A test skill.</description>",
		"<location>/path/to/skill/SKILL.md</location>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// PI: "should include intro text before XML"
func TestFormatForPrompt_Intro(t *testing.T) {
	skills := []Skill{testSkill("test-skill", "A test skill.", "/path/to/skill/SKILL.md", false)}
	got := FormatForPrompt(skills, "read")

	xmlStart := strings.Index(got, "<available_skills>")
	if xmlStart < 0 {
		t.Fatalf("no <available_skills> in output:\n%s", got)
	}
	intro := got[:xmlStart]
	if !strings.Contains(intro, "The following skills provide specialized instructions") {
		t.Errorf("intro missing heading:\n%s", intro)
	}
	if !strings.Contains(intro, "Use the read tool to load a skill's file") {
		t.Errorf("intro missing read-tool line:\n%s", intro)
	}
}

func TestFormatForPrompt_FileReadToolVariants(t *testing.T) {
	skills := []Skill{testSkill("test-skill", "A test skill.", "/path/SKILL.md", false)}

	if got := FormatForPrompt(skills, "read"); !strings.Contains(got, "Use the read tool to load a skill's file") {
		t.Errorf("read variant missing its line: %s", got)
	}
	if got := FormatForPrompt(skills, "bash"); !strings.Contains(got, "Use bash to load a skill's file") {
		t.Errorf("bash variant missing its line: %s", got)
	}
	indirect := FormatForPrompt(skills, "indirect")
	if !strings.Contains(indirect, "Load a skill's file when the task matches its description.") {
		t.Errorf("indirect variant missing its line: %s", indirect)
	}
	if strings.Contains(indirect, "Use the read tool") || strings.Contains(indirect, "Use bash") {
		t.Errorf("indirect variant leaked tool-specific text: %s", indirect)
	}
	// PI's ternary treats any unknown value as the indirect variant.
	if got := FormatForPrompt(skills, "unknown"); got != indirect {
		t.Errorf("unknown variant differs from indirect:\n%q\n%q", got, indirect)
	}
}

// PI: "should escape XML special characters"
func TestFormatForPrompt_EscapesXML(t *testing.T) {
	skills := []Skill{testSkill("test-skill", `A skill with <special> & "characters".`, "/path/to/skill/SKILL.md", false)}
	got := FormatForPrompt(skills, "read")

	for _, want := range []string{"&lt;special&gt;", "&amp;", `&quot;characters&quot;`} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing escaped %q:\n%s", want, got)
		}
	}
}

// PI: "should format multiple skills"
func TestFormatForPrompt_MultipleSkills(t *testing.T) {
	skills := []Skill{
		testSkill("skill-one", "First skill.", "/path/one/SKILL.md", false),
		testSkill("skill-two", "Second skill.", "/path/two/SKILL.md", false),
	}
	got := FormatForPrompt(skills, "read")

	if !strings.Contains(got, "<name>skill-one</name>") || !strings.Contains(got, "<name>skill-two</name>") {
		t.Errorf("missing skill names:\n%s", got)
	}
	if count := strings.Count(got, "<skill>"); count != 2 {
		t.Errorf("<skill> count = %d, want 2", count)
	}
}

// PI: "should exclude skills with disableModelInvocation from prompt"
func TestFormatForPrompt_ExcludesDisabled(t *testing.T) {
	skills := []Skill{
		testSkill("visible-skill", "A visible skill.", "/path/visible/SKILL.md", false),
		testSkill("hidden-skill", "A hidden skill.", "/path/hidden/SKILL.md", true),
	}
	got := FormatForPrompt(skills, "read")

	if !strings.Contains(got, "<name>visible-skill</name>") {
		t.Errorf("visible skill missing:\n%s", got)
	}
	if strings.Contains(got, "<name>hidden-skill</name>") {
		t.Errorf("disabled skill leaked into prompt:\n%s", got)
	}
	if count := strings.Count(got, "<skill>"); count != 1 {
		t.Errorf("<skill> count = %d, want 1", count)
	}
}

// PI: "should return empty string when all skills have disableModelInvocation"
func TestFormatForPrompt_AllDisabled(t *testing.T) {
	skills := []Skill{testSkill("hidden-skill", "A hidden skill.", "/path/hidden/SKILL.md", true)}
	if got := FormatForPrompt(skills, "read"); got != "" {
		t.Errorf("FormatForPrompt = %q, want empty", got)
	}
	if got := Index(skills); got != "" {
		t.Errorf("Index = %q, want empty", got)
	}
}

// Byte-exact check against PI's formatSkillsForPrompt output. The expected
// strings below were produced by executing PI's exact formatter algorithm
// from skills.ts:358-388.
func TestFormatForPrompt_ByteExactPIOutput(t *testing.T) {
	skills := []Skill{
		testSkill("test-skill", `A skill with <special> & "characters".`, "/path/to/skill/SKILL.md", false),
		testSkill("hidden", "Hidden.", "/path/hidden/SKILL.md", true),
	}

	const wantRead = `

The following skills provide specialized instructions for specific tasks.
Use the read tool to load a skill's file when the task matches its description.
Resolve relative paths against the skill's directory (parent of SKILL.md). Use absolute paths in tool calls.

<available_skills>
  <skill>
    <name>test-skill</name>
    <description>A skill with &lt;special&gt; &amp; &quot;characters&quot;.</description>
    <location>/path/to/skill/SKILL.md</location>
  </skill>
</available_skills>`

	const wantBash = `

The following skills provide specialized instructions for specific tasks.
Use bash to load a skill's file when the task matches its description.
Resolve relative paths against the skill's directory (parent of SKILL.md). Use absolute paths in tool calls.

<available_skills>
  <skill>
    <name>test-skill</name>
    <description>A skill with &lt;special&gt; &amp; &quot;characters&quot;.</description>
    <location>/path/to/skill/SKILL.md</location>
  </skill>
</available_skills>`

	const wantIndirect = `

The following skills provide specialized instructions for specific tasks.
Load a skill's file when the task matches its description.
Resolve relative paths against the skill's directory (parent of SKILL.md). Use absolute paths in tool calls.

<available_skills>
  <skill>
    <name>test-skill</name>
    <description>A skill with &lt;special&gt; &amp; &quot;characters&quot;.</description>
    <location>/path/to/skill/SKILL.md</location>
  </skill>
</available_skills>`

	if got := FormatForPrompt(skills, "read"); got != wantRead {
		t.Errorf("read output differs from PI:\ngot  %q\nwant %q", got, wantRead)
	}
	if got := FormatForPrompt(skills, "bash"); got != wantBash {
		t.Errorf("bash output differs from PI:\ngot  %q\nwant %q", got, wantBash)
	}
	if got := FormatForPrompt(skills, "indirect"); got != wantIndirect {
		t.Errorf("indirect output differs from PI:\ngot  %q\nwant %q", got, wantIndirect)
	}
}

// Host contract: the index carries only name/description/path; the body stays
// out of the system prompt (progressive disclosure).
func TestIndex_ExcludesBody(t *testing.T) {
	skill := testSkill("body-skill", "Has a body.", "/path/body/SKILL.md", false)
	skill.Body = "SECRET BODY TEXT"
	got := Index([]Skill{skill})

	if strings.Contains(got, "SECRET BODY TEXT") {
		t.Errorf("Index leaked the skill body:\n%s", got)
	}
	if got != FormatForPrompt([]Skill{skill}, "read") {
		t.Error("Index is not FormatForPrompt with the read tool")
	}
}

// PI sdk-skills.test.ts: a skill created in a temp skills directory is
// discovered by default. The session wiring around it is out of scope here.
func TestDiscover_TempSkillsDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "skills", "test-skill"), "test-skill", "A test skill for SDK tests.")

	skills, warnings := Discover([]string{filepath.Join(root, "skills")})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	found := false
	for _, skill := range skills {
		if skill.Name == "test-skill" {
			found = true
		}
	}
	if !found {
		t.Fatalf("test-skill not discovered: %+v", skills)
	}
}
