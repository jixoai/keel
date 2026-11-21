package skills

// Skill bundles (change keel-harness-v2, W7 skills): a skill is not only its
// SKILL.md. The specification allows resources next to it (references, assets,
// scripts), and a host that versions skills must version the whole tree. This
// file defines that tree as a deterministic, content-addressed object:
//
//   - the bytes of every regular file (SKILL.md included) are hashed;
//   - the bundle identity is the SHA-256 of a canonical manifest
//     (`keel-skill-bundle-v1`, files sorted by path, sizes and modes included);
//   - equal content therefore has one hash no matter where it was read from,
//     which is what content deduplication and immutable versions need.
//
// A bundle is read from a directory (`LoadBundle`), built from in-memory files
// (`BundleFromFiles`), or written to a directory (`WriteBundle`). None of these
// functions executes anything: a resource that happens to be a shell script is
// stored as bytes. Execution is the bash tool's job and stays under its policy
// (see internal/skilllib/backend.go and the W7 evidence note).
//
// Bounds are part of the contract: a skill is context material a model reads,
// not an arbitrary archive, so the loader refuses a tree larger than
// MaxBundleFiles/MaxBundleBytes instead of materialising it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bundle limits. They are exported so the registry's validation and the tool
// surface publish the same numbers.
const (
	// MaxBundleFiles bounds the number of files in one skill bundle.
	MaxBundleFiles = 256
	// MaxBundleBytes bounds one bundle's total bytes (the context a skill may
	// add); resources larger than this belong in workspace files, not a skill.
	MaxBundleBytes = 8 << 20
	// MaxBundleFileBytes bounds one resource file.
	MaxBundleFileBytes = 2 << 20
	// BundleManifestVersion is the canonical manifest's schema version.
	BundleManifestVersion = 1
)

// Bundle errors are typed so callers can map them onto their own surfaces
// (the tool surface's invalid_arguments vs the settings API's 400).
var (
	// ErrBundleInvalid reports a structurally invalid bundle (missing
	// SKILL.md, an unsafe path, a limit exceeded, an invalid file mode).
	ErrBundleInvalid = errors.New("skills: invalid bundle")
	// ErrBundleMismatch reports a bundle whose bytes do not match the hash it
	// is stored under: an immutable version was modified after the fact.
	ErrBundleMismatch = errors.New("skills: bundle content does not match its hash")
)

// BundleFile is one file of a bundle in the canonical manifest.
type BundleFile struct {
	// Path is the slash-separated path relative to the skill directory.
	Path string `json:"path"`
	// Size is the file's byte length.
	Size int64 `json:"size"`
	// Hash is the lowercase hex SHA-256 of the file's bytes.
	Hash string `json:"hash"`
	// Mode is the Unix permission bits (0644 or 0755).
	Mode uint32 `json:"mode"`
}

// Bundle is one parsed skill tree. Files are the canonical manifest, Skill is
// the parsed SKILL.md, Hash is the bundle identity and contents holds the
// bytes (bounded by MaxBundleBytes).
type Bundle struct {
	Skill    *Skill
	Files    []BundleFile
	Hash     string
	contents map[string][]byte
}

// Manifest is the canonical manifest document the bundle hash is derived from.
type Manifest struct {
	Version int          `json:"version"`
	Files   []BundleFile `json:"files"`
}

// NewManifest builds the canonical manifest for a file list. The list is sorted
// in place by path so the encoding is independent of discovery order.
func NewManifest(files []BundleFile) Manifest {
	copied := append([]BundleFile(nil), files...)
	sort.Slice(copied, func(i, j int) bool { return copied[i].Path < copied[j].Path })
	return Manifest{Version: BundleManifestVersion, Files: copied}
}

// HashManifest returns the bundle identity of a canonical manifest.
func HashManifest(manifest Manifest) (string, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("skills: encode bundle manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// LoadBundle reads a skill directory from disk.
//
// The directory must contain SKILL.md at its root. Hidden entries and
// node_modules are skipped (they are not skill content), symlinks are refused
// rather than followed (a version's bytes must come from the version), and the
// size/count bounds are enforced before any file is returned.
func LoadBundle(dir string) (*Bundle, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %q: %v", ErrBundleInvalid, dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBundleInvalid, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %q is not a directory", ErrBundleInvalid, dir)
	}

	contents := make(map[string][]byte)
	files := make([]BundleFile, 0, 16)
	var total int64
	err = filepath.WalkDir(abs, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == abs {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink %q is not allowed in a skill bundle", ErrBundleInvalid, name)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(abs, current)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if err := validateBundlePath(rel); err != nil {
			return err
		}
		fileInfo, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if fileInfo.Size() > MaxBundleFileBytes {
			return fmt.Errorf("%w: %q is %d bytes (limit %d)", ErrBundleInvalid, rel, fileInfo.Size(), MaxBundleFileBytes)
		}
		raw, readErr := os.ReadFile(current)
		if readErr != nil {
			return readErr
		}
		total += int64(len(raw))
		if total > MaxBundleBytes {
			return fmt.Errorf("%w: bundle exceeds %d bytes", ErrBundleInvalid, MaxBundleBytes)
		}
		if len(files) >= MaxBundleFiles {
			return fmt.Errorf("%w: bundle exceeds %d files", ErrBundleInvalid, MaxBundleFiles)
		}
		mode := normalizeMode(uint32(fileInfo.Mode().Perm()))
		files = append(files, BundleFile{Path: rel, Size: int64(len(raw)), Hash: ContentHash(raw), Mode: mode})
		contents[rel] = raw
		return nil
	})
	if err != nil {
		return nil, err
	}

	if _, ok := contents[SkillFileName]; !ok {
		return nil, fmt.Errorf("%w: %s is required at the bundle root", ErrBundleInvalid, SkillFileName)
	}
	bundle, err := newBundle(files, contents, filepath.Base(abs))
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

// BundleFromFiles builds a bundle from in-memory files (an install request or
// a tool call). Paths are validated exactly like LoadBundle's, so an upload
// cannot escape the skill directory.
func BundleFromFiles(inputs []FileInput) (*Bundle, error) {
	return BundleFromFilesNamed(inputs, "")
}

// BundleFromFilesNamed is BundleFromFiles with the skill name as the fallback
// when the SKILL.md frontmatter omits `name` (the specification's rule: the
// directory name is the default). The registry passes the requested name, so
// an uploaded bundle without a name field still lands under the name the
// caller asked for instead of the synthetic "bundle".
func BundleFromFilesNamed(inputs []FileInput, name string) (*Bundle, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: at least %s is required", ErrBundleInvalid, SkillFileName)
	}
	contents := make(map[string][]byte, len(inputs))
	files := make([]BundleFile, 0, len(inputs))
	var total int64
	for _, input := range inputs {
		rel := strings.TrimPrefix(filepath.ToSlash(input.Path), "./")
		if err := validateBundlePath(rel); err != nil {
			return nil, err
		}
		if len(input.Content) > MaxBundleFileBytes {
			return nil, fmt.Errorf("%w: %q is %d bytes (limit %d)", ErrBundleInvalid, rel, len(input.Content), MaxBundleFileBytes)
		}
		if _, exists := contents[rel]; exists {
			return nil, fmt.Errorf("%w: duplicate path %q", ErrBundleInvalid, rel)
		}
		total += int64(len(input.Content))
		if total > MaxBundleBytes {
			return nil, fmt.Errorf("%w: bundle exceeds %d bytes", ErrBundleInvalid, MaxBundleBytes)
		}
		if len(files) >= MaxBundleFiles {
			return nil, fmt.Errorf("%w: bundle exceeds %d files", ErrBundleInvalid, MaxBundleFiles)
		}
		mode := normalizeMode(input.Mode)
		files = append(files, BundleFile{Path: rel, Size: int64(len(input.Content)), Hash: ContentHash(input.Content), Mode: mode})
		contents[rel] = append([]byte(nil), input.Content...)
	}
	if _, ok := contents[SkillFileName]; !ok {
		return nil, fmt.Errorf("%w: %s is required at the bundle root", ErrBundleInvalid, SkillFileName)
	}
	hint := strings.TrimSpace(name)
	if hint == "" {
		hint = "bundle"
	}
	return newBundle(files, contents, hint)
}

// FileInput is one in-memory bundle file.
type FileInput struct {
	Path    string
	Content []byte
	Mode    uint32
}

// newBundle is the shared constructor: it parses SKILL.md, validates the name
// and computes the identity. nameHint is the directory name the specification
// uses when the frontmatter omits `name`.
func newBundle(files []BundleFile, contents map[string][]byte, nameHint string) (*Bundle, error) {
	manifest := NewManifest(files)
	hash, err := HashManifest(manifest)
	if err != nil {
		return nil, err
	}
	raw := contents[SkillFileName]
	skill, warnings, parseErr := ParseSKILLMD(raw, path.Join(nameHint, SkillFileName))
	if parseErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrBundleInvalid, parseErr)
	}
	if skill == nil {
		reason := "description is required"
		if len(warnings) > 0 {
			reason = warnings[0].Message
		}
		return nil, fmt.Errorf("%w: %s", ErrBundleInvalid, reason)
	}
	// The synthetic path above is only for parsing; callers that materialise the
	// bundle replace it (Bundle.WithPath).
	return &Bundle{Skill: skill, Files: manifest.Files, Hash: hash, contents: contents}, nil
}

// WithPath returns the bundle with its skill path rewritten to <dir>/SKILL.md.
// It is used after materialisation so relative resource references resolve
// against the real directory. The identity is unchanged.
func (b *Bundle) WithPath(dir string) *Bundle {
	if b == nil || b.Skill == nil {
		return b
	}
	copy := *b
	skillCopy := *b.Skill
	skillCopy.Path = filepath.Join(dir, SkillFileName)
	copy.Skill = &skillCopy
	return &copy
}

// ReadFile returns one file's bytes. The path must be in the manifest, so a
// caller cannot read outside the bundle even if it controls the name.
func (b *Bundle) ReadFile(name string) ([]byte, bool) {
	if b == nil {
		return nil, false
	}
	clean := strings.TrimPrefix(filepath.ToSlash(name), "./")
	if validateBundlePath(clean) != nil {
		return nil, false
	}
	content, ok := b.contents[clean]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), content...), true
}

// Contents returns a copy of every file (used by persistence and the API).
func (b *Bundle) Contents() map[string][]byte {
	out := make(map[string][]byte, len(b.contents))
	for name, content := range b.contents {
		out[name] = append([]byte(nil), content...)
	}
	return out
}

// Template returns the parsed SKILL.md text as a file list (a convenience for
// patch operations).
func (b *Bundle) Template() []FileInput {
	inputs := make([]FileInput, 0, len(b.Files))
	for _, file := range b.Files {
		inputs = append(inputs, FileInput{Path: file.Path, Content: b.contents[file.Path], Mode: file.Mode})
	}
	return inputs
}

// Verify checks that a directory's bytes still hash to the expected bundle
// identity. It is the immutable-version check: a version directory whose
// content changed is reported, never silently trusted.
func Verify(dir, expected string) error {
	bundle, err := LoadBundle(dir)
	if err != nil {
		return err
	}
	if bundle.Hash != expected {
		return fmt.Errorf("%w: %s has bundle hash %s, want %s", ErrBundleMismatch, dir, bundle.Hash, expected)
	}
	return nil
}

// WriteBundle writes a bundle into dir. The write is atomic in the sense that
// matters here: the tree is built under a temporary sibling and renamed into
// place, so a reader never sees a half-written skill. An existing directory is
// accepted only when its content hashes to the same identity (idempotent
// content deduplication); a mismatch is refused, because versions are
// immutable.
func WriteBundle(dir string, bundle *Bundle) error {
	if bundle == nil || bundle.Skill == nil {
		return fmt.Errorf("%w: bundle is empty", ErrBundleInvalid)
	}
	if existing, err := LoadBundle(dir); err == nil {
		if existing.Hash != bundle.Hash {
			return fmt.Errorf("%w: %s already holds bundle %s", ErrBundleMismatch, dir, existing.Hash)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// A directory that exists but is not a valid bundle is never replaced by
		// WriteBundle: version directories are write-once.
		if _, statErr := os.Stat(dir); statErr == nil {
			return fmt.Errorf("%w: %s exists and is not bundle %s: %v", ErrBundleMismatch, dir, bundle.Hash, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(dir), ".bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	for _, file := range bundle.Files {
		content, ok := bundle.contents[file.Path]
		if !ok {
			return fmt.Errorf("%w: missing content for %q", ErrBundleInvalid, file.Path)
		}
		target := filepath.Join(temp, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, os.FileMode(normalizeMode(file.Mode))); err != nil {
			return err
		}
	}
	if err := os.Rename(temp, dir); err != nil {
		return err
	}
	return nil
}

// ReplaceBundle atomically replaces dir with bundle's content. Unlike
// WriteBundle it accepts an existing directory: it is the active-pointer
// materialisation, where the bytes of the *version* stay immutable in their
// own directory and only this copy moves.
func ReplaceBundle(dir string, bundle *Bundle) error {
	if bundle == nil || bundle.Skill == nil {
		return fmt.Errorf("%w: bundle is empty", ErrBundleInvalid)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".active-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	for _, file := range bundle.Files {
		content, ok := bundle.contents[file.Path]
		if !ok {
			return fmt.Errorf("%w: missing content for %q", ErrBundleInvalid, file.Path)
		}
		target := filepath.Join(temp, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, os.FileMode(normalizeMode(file.Mode))); err != nil {
			return err
		}
	}
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = dir + ".old"
		_ = os.RemoveAll(old)
		if err := os.Rename(dir, old); err != nil {
			return err
		}
	}
	if err := os.Rename(temp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		return err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// ComposeSkillDocument renders a SKILL.md from structured fields. It is what
// `skills.manage create` and `/learn` use: the caller supplies the substance
// (description and body), the host supplies valid frontmatter so the document
// always parses under the same rules as a hand-written skill.
func ComposeSkillDocument(name, description, body string, metadata map[string]string) ([]byte, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrBundleInvalid)
	}
	if description == "" {
		return nil, fmt.Errorf("%w: description is required", ErrBundleInvalid)
	}
	frontmatter := map[string]any{"name": name, "description": description}
	if len(metadata) > 0 {
		copied := make(map[string]string, len(metadata))
		for key, value := range metadata {
			copied[key] = value
		}
		frontmatter["metadata"] = copied
	}
	yamlText, err := yaml.Marshal(frontmatter)
	if err != nil {
		return nil, fmt.Errorf("skills: encode frontmatter: %w", err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlText)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")
	return []byte(b.String()), nil
}

// validateBundlePath rejects absolute paths, parent traversal, empty segments
// and Windows separators. The manifest is a portable format, so it stores
// slash paths only.
func validateBundlePath(rel string) error {
	if rel == "" {
		return fmt.Errorf("%w: empty file path", ErrBundleInvalid)
	}
	if strings.Contains(rel, `\`) {
		return fmt.Errorf("%w: path %q must use forward slashes", ErrBundleInvalid, rel)
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("%w: path %q must be relative", ErrBundleInvalid, rel)
	}
	clean := path.Clean(rel)
	if clean == "." || clean != rel {
		return fmt.Errorf("%w: path %q is not canonical", ErrBundleInvalid, rel)
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: path %q contains %q", ErrBundleInvalid, rel, segment)
		}
		// LoadBundle skips hidden entries and node_modules; an uploaded path
		// carrying them would hash differently after materialisation. Refuse it
		// here so a bundle's identity is the same on both sides.
		if strings.HasPrefix(segment, ".") || segment == "node_modules" {
			return fmt.Errorf("%w: path %q is not skill content (hidden or node_modules)", ErrBundleInvalid, rel)
		}
	}
	return nil
}

// normalizeMode keeps a file readable, and executable when it was. The
// registry never preserves setuid/sticky bits from an upload.
func normalizeMode(mode uint32) uint32 {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}
