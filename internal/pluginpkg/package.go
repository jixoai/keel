package pluginpkg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Problem is why a package directory could not be used. A package with a
// Problem is isolated: discovery reports it (state invalid) and every other
// plugin keeps working.
type Problem struct {
	// Path is the offending file (usually plugin.yaml or a symlink).
	Path string `json:"path"`
	// Reason is a one-line summary.
	Reason string `json:"reason"`
	// Fields carries the field-level validation errors, when the manifest
	// parsed but failed validation.
	Fields []FieldError `json:"fields,omitempty"`
}

func (p *Problem) Error() string {
	if p == nil {
		return ""
	}
	if p.Path == "" {
		return p.Reason
	}
	return p.Path + ": " + p.Reason
}

// Package is one plugin package directory, loaded and validated.
type Package struct {
	// Dir is the package directory (absolute when the caller passed an absolute
	// path).
	Dir string `json:"dir"`
	// Manifest is the parsed manifest (nil when Problem is set).
	Manifest *Manifest `json:"manifest,omitempty"`
	// Hash is the SHA-256 of the package tree (empty when Problem is set).
	Hash string `json:"hash,omitempty"`
	// Problem explains why the package is unusable (nil when it is usable).
	Problem *Problem `json:"problem,omitempty"`
}

// Usable reports whether the package passed loading and validation.
func (p Package) Usable() bool { return p.Problem == nil && p.Manifest != nil }

// ID is the manifest id (empty for an unusable package).
func (p Package) ID() string {
	if p.Manifest == nil {
		return ""
	}
	return p.Manifest.ID
}

// Version is the manifest version (empty for an unusable package).
func (p Package) Version() string {
	if p.Manifest == nil {
		return ""
	}
	return p.Manifest.Version
}

// LoadDir reads and validates `<dir>/plugin.yaml` and hashes the package tree.
// A missing manifest is reported as a Problem with reason "plugin.yaml is
// missing" only when required; discovery skips such directories instead.
func LoadDir(dir string, env Environment) Package {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return Package{Dir: dir, Problem: &Problem{Path: dir, Reason: "resolve directory: " + err.Error()}}
	}
	manifestPath := filepath.Join(absolute, PluginFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		reason := "read " + PluginFileName + ": " + err.Error()
		if os.IsNotExist(err) {
			reason = PluginFileName + " is missing"
		}
		return Package{Dir: absolute, Problem: &Problem{Path: manifestPath, Reason: reason}}
	}
	manifest, err := ParseManifest(raw)
	if err != nil {
		return Package{Dir: absolute, Problem: &Problem{Path: manifestPath, Reason: err.Error(), Fields: FieldErrors(err)}}
	}
	manifest.Dir = absolute
	if err := manifest.Validate(env); err != nil {
		return Package{Dir: absolute, Manifest: manifest, Problem: &Problem{Path: manifestPath, Reason: err.Error(), Fields: FieldErrors(err)}}
	}
	if fieldErrs := validateTree(absolute, manifest); len(fieldErrs) > 0 {
		problem := &Problem{Path: manifestPath, Reason: "package content is invalid", Fields: fieldErrs}
		problem.Reason = problem.Fields[0].String()
		return Package{Dir: absolute, Manifest: manifest, Problem: problem}
	}
	if fieldErrs := validateExtensibilityFiles(absolute, manifest); len(fieldErrs) > 0 {
		problem := &Problem{Path: filepath.Join(absolute, ExtensibilityFileName), Reason: "extensibility document is invalid", Fields: fieldErrs}
		problem.Reason = problem.Fields[0].String()
		return Package{Dir: absolute, Manifest: manifest, Problem: problem}
	}
	hash, err := HashTree(absolute)
	if err != nil {
		return Package{Dir: absolute, Manifest: manifest, Problem: &Problem{Path: absolute, Reason: "hash package: " + err.Error()}}
	}
	return Package{Dir: absolute, Manifest: manifest, Hash: hash}
}

// validateTree checks the parts of the manifest that only the filesystem can
// answer: UI/contribution/extensibility entries exist inside the package, and
// the declared entry.command head resolves inside the package (a sidecar
// command is allowed to be a host binary name found on PATH, so only an
// explicit relative path is resolved).
func validateTree(dir string, manifest *Manifest) []FieldError {
	var errs fieldSet
	checkFile := func(field, value string) {
		entry := filepath.Join(dir, filepath.FromSlash(value))
		if !IsInside(dir, entry) {
			errs.add(field, "path %q escapes the plugin package", value)
			return
		}
		info, err := os.Stat(entry)
		if err != nil {
			errs.add(field, "path %q does not exist in the package", value)
			return
		}
		if info.IsDir() {
			errs.add(field, "path %q is a directory, not a file", value)
		}
	}
	if ui := manifest.Provides.UI; ui != nil && ui.Entry != "" {
		checkFile("provides.ui.entry", ui.Entry)
	}
	for index, contribution := range manifest.Contributes {
		if contribution.Entry != "" {
			checkFile(fmt.Sprintf("contributes[%d].entry", index), contribution.Entry)
		}
	}
	if ext := manifest.Extensibility; ext != nil {
		if ext.Doc != "" {
			checkFile("extensibility.doc", ext.Doc)
		}
		if ext.Schema != "" {
			checkFile("extensibility.schema", ext.Schema)
		}
	}
	return errs.list
}

// validateExtensibilityFiles parses and cross-checks the machine-readable
// document when the manifest declares one. A package whose JSON drifted from
// plugin.yaml is refused here, so creation mode never aggregates a document
// that describes a different surface than the installed manifest.
func validateExtensibilityFiles(dir string, manifest *Manifest) []FieldError {
	if manifest.Extensibility == nil || manifest.Extensibility.Schema == "" {
		return nil
	}
	path := filepath.Join(dir, filepath.FromSlash(manifest.Extensibility.Schema))
	raw, err := os.ReadFile(path)
	if err != nil {
		// validateTree already reported a missing file; a read failure here is
		// the same problem and must not be hidden.
		return []FieldError{{Field: "extensibility.schema", Message: err.Error()}}
	}
	doc, err := ParseExtensibility(raw)
	if err != nil {
		return prefixedFields("extensibility.", FieldErrors(err))
	}
	var failures []FieldError
	if err := doc.Validate(); err != nil {
		failures = append(failures, FieldErrors(err)...)
	}
	if err := doc.ValidateAgainstManifest(manifest); err != nil {
		failures = append(failures, FieldErrors(err)...)
	}
	return prefixedFields("extensibility.", failures)
}

// prefixedFields qualifies document field errors with the manifest key that
// points at the document, so a 400 can say
// "extensibility.schema: contributes[0].slot ...".
func prefixedFields(prefix string, fields []FieldError) []FieldError {
	out := make([]FieldError, 0, len(fields))
	for _, field := range fields {
		name := prefix + field.Field
		if field.Field == "" {
			name = strings.TrimSuffix(prefix, ".")
		}
		out = append(out, FieldError{Field: name, Message: field.Message})
	}
	return out
}

// IsInside reports whether target stays inside root (both may be relative; they
// are compared after cleaning, so ".." cannot slip through).
func IsInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// treeEntry is one hashed/ copied node, sorted by relative path.
type treeEntry struct {
	rel   string
	path  string
	isDir bool
	// executable marks a file with any executable bit set.
	executable bool
}

// walkTree lists the package tree deterministically. Symlinks are resolved:
// a link that escapes the package root is a Problem, a link inside the package
// is followed (the copy materializes it as a regular file, so the managed tree
// never contains a link).
func walkTree(dir string) ([]treeEntry, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("symlink %s is broken: %w", rel, err)
			}
			if !IsInside(root, resolved) {
				return fmt.Errorf("symlink %s escapes the plugin package (%s)", rel, resolved)
			}
			target, err := os.Stat(resolved)
			if err != nil {
				return err
			}
			if target.IsDir() {
				return fmt.Errorf("symlink %s points at a directory; directory links are not supported in a plugin package", rel)
			}
			entries = append(entries, treeEntry{rel: rel, path: resolved, executable: target.Mode()&0o111 != 0})
			return nil
		}
		if info.IsDir() {
			entries = append(entries, treeEntry{rel: rel, path: path, isDir: true})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		entries = append(entries, treeEntry{rel: rel, path: path, executable: info.Mode()&0o111 != 0})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, nil
}

// HashTree hashes a package directory: SHA-256 over the tree's relative paths,
// kinds and file contents in sorted order. Each record is
// `kind \0 rel \0 size \0 content`, which is prefix-free, so two different trees
// cannot produce the same byte stream. The manifest is part of the tree, so a
// manifest change changes the hash. The hash is the package's content identity
// in the database.
func HashTree(dir string) (string, error) {
	entries, err := walkTree(dir)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	for _, entry := range entries {
		kind := "f"
		if entry.isDir {
			kind = "d"
		}
		if entry.isDir {
			if _, err := io.WriteString(digest, kind+"\x00"+entry.rel+"\x00"); err != nil {
				return "", err
			}
			continue
		}
		info, err := os.Stat(entry.path)
		if err != nil {
			return "", err
		}
		size := info.Size()
		if _, err := io.WriteString(digest, kind+"\x00"+entry.rel+"\x00"+strconv.FormatInt(size, 10)+"\x00"); err != nil {
			return "", err
		}
		file, err := os.Open(entry.path)
		if err != nil {
			return "", err
		}
		copied, err := io.Copy(digest, file)
		file.Close()
		if err != nil {
			return "", err
		}
		if copied != size {
			return "", fmt.Errorf("%s changed while hashing (%d bytes read, %d expected)", entry.rel, copied, size)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// CopyTree materializes a package at dst with regular files only (no symlinks,
// no devices), so the managed copy can never point outside the plugins root.
// It enforces MaxPackageBytes/MaxPackageFileBytes.
func CopyTree(src, dst string) error {
	entries, err := walkTree(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var total int64
	for _, entry := range entries {
		target := filepath.Join(dst, filepath.FromSlash(entry.rel))
		if !IsInside(dst, target) {
			return fmt.Errorf("entry %s escapes the destination", entry.rel)
		}
		if entry.isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		source, err := os.Open(entry.path)
		if err != nil {
			return err
		}
		info, err := source.Stat()
		if err != nil {
			source.Close()
			return err
		}
		if info.Size() > MaxPackageFileBytes {
			source.Close()
			return fmt.Errorf("%s is %s (limit %d bytes)", entry.rel, describeSize(info.Size()), MaxPackageFileBytes)
		}
		total += info.Size()
		if total > MaxPackageBytes {
			source.Close()
			return fmt.Errorf("the package exceeds %d bytes", MaxPackageBytes)
		}
		mode := os.FileMode(0o644)
		if entry.executable {
			mode = 0o755
		}
		dest, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			source.Close()
			return err
		}
		if _, err := io.Copy(dest, source); err != nil {
			dest.Close()
			source.Close()
			return err
		}
		dest.Close()
		source.Close()
	}
	return nil
}

// RemoveTree removes a package directory, refusing to touch anything outside
// the given root.
func RemoveTree(root, dir string) error {
	if !IsInside(root, dir) || filepath.Clean(root) == filepath.Clean(dir) {
		return fmt.Errorf("refuse to remove %s: outside the plugins root", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}
