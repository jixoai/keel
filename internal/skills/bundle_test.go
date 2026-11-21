package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillTree(t *testing.T, root, skillMD string, resources map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, "demo-skill")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SkillFileName), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	for name, content := range resources {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir resource: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write resource: %v", err)
		}
	}
	return dir
}

const demoSkillMD = `---
name: demo-skill
description: A demo skill with one reference.
---

# Demo

Follow references/guide.md.
`

func TestLoadBundleHashesWholeTreeDeterministically(t *testing.T) {
	root := t.TempDir()
	first := writeSkillTree(t, filepath.Join(root, "a"), demoSkillMD, map[string]string{
		"references/guide.md": "step one\n",
		"assets/logo.txt":     "keel\n",
	})
	second := writeSkillTree(t, filepath.Join(root, "b"), demoSkillMD, map[string]string{
		"assets/logo.txt":     "keel\n",
		"references/guide.md": "step one\n",
	})

	bundleA, err := LoadBundle(first)
	if err != nil {
		t.Fatalf("LoadBundle(first): %v", err)
	}
	bundleB, err := LoadBundle(second)
	if err != nil {
		t.Fatalf("LoadBundle(second): %v", err)
	}
	if bundleA.Hash == "" {
		t.Fatal("bundle hash is empty")
	}
	if bundleA.Hash != bundleB.Hash {
		t.Fatalf("same content has different hashes: %s vs %s", bundleA.Hash, bundleB.Hash)
	}
	if bundleA.Skill.Name != "demo-skill" {
		t.Fatalf("skill name = %q", bundleA.Skill.Name)
	}
	if len(bundleA.Files) != 3 {
		t.Fatalf("files = %d, want 3", len(bundleA.Files))
	}
	if bundleA.Files[0].Path != SkillFileName {
		t.Fatalf("first manifest path = %q, want SKILL.md", bundleA.Files[0].Path)
	}
	if got := bundleA.Files[0].Hash; got != ContentHash([]byte(demoSkillMD)) {
		t.Fatalf("SKILL.md hash = %s", got)
	}
	content, ok := bundleA.ReadFile("references/guide.md")
	if !ok || string(content) != "step one\n" {
		t.Fatalf("ReadFile = %q, %v", content, ok)
	}
	if _, ok := bundleA.ReadFile("../outside"); ok {
		t.Fatal("ReadFile accepted a traversal path")
	}
}

func TestBundleHashChangesWithResourceContent(t *testing.T) {
	root := t.TempDir()
	base := writeSkillTree(t, filepath.Join(root, "a"), demoSkillMD, map[string]string{"references/guide.md": "step one\n"})
	changed := writeSkillTree(t, filepath.Join(root, "b"), demoSkillMD, map[string]string{"references/guide.md": "step two\n"})
	baseBundle, err := LoadBundle(base)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	changedBundle, err := LoadBundle(changed)
	if err != nil {
		t.Fatalf("changed: %v", err)
	}
	if baseBundle.Hash == changedBundle.Hash {
		t.Fatal("changing a resource did not change the bundle hash")
	}
	if baseBundle.Skill.Hash != changedBundle.Skill.Hash {
		t.Fatal("SKILL.md did not change, so the document hash must stay equal")
	}
}

func TestLoadBundleRejectsMissingSKILLMDAndSymlinks(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(empty); !errors.Is(err, ErrBundleInvalid) {
		t.Fatalf("missing SKILL.md error = %v", err)
	}

	dir := writeSkillTree(t, filepath.Join(root, "d"), demoSkillMD, nil)
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "references", "escape.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadBundle(dir); !errors.Is(err, ErrBundleInvalid) {
		t.Fatalf("symlink error = %v, want ErrBundleInvalid", err)
	}
}

func TestBundleFromFilesRejectsUnsafePaths(t *testing.T) {
	doc, err := ComposeSkillDocument("made-up", "description", "body", nil)
	if err != nil {
		t.Fatalf("ComposeSkillDocument: %v", err)
	}
	for _, bad := range []string{"../escape.md", "/etc/passwd", `references\win.md`, "a//b.md", "./SKILL.md", ".hidden/x.md", "node_modules/x.md"} {
		_, err := BundleFromFiles([]FileInput{
			{Path: SkillFileName, Content: doc},
			{Path: bad, Content: []byte("x")},
		})
		if !errors.Is(err, ErrBundleInvalid) {
			t.Fatalf("path %q error = %v, want ErrBundleInvalid", bad, err)
		}
	}
	bundle, err := BundleFromFilesNamed([]FileInput{{Path: SkillFileName, Content: doc}}, "made-up")
	if err != nil {
		t.Fatalf("valid bundle: %v", err)
	}
	if bundle.Skill.Name != "made-up" {
		t.Fatalf("name = %q", bundle.Skill.Name)
	}
}

func TestComposeSkillDocumentRoundTrips(t *testing.T) {
	doc, err := ComposeSkillDocument("deploy-flow", "Ship a release", "# Steps\n\n1. Build.\n", map[string]string{"owner": "platform"})
	if err != nil {
		t.Fatalf("ComposeSkillDocument: %v", err)
	}
	skill, warnings, err := ParseSKILLMD(doc, filepath.Join("deploy-flow", SkillFileName))
	if err != nil {
		t.Fatalf("ParseSKILLMD: %v", err)
	}
	if skill == nil {
		t.Fatalf("skill is nil; warnings %v", warnings)
	}
	if skill.Name != "deploy-flow" || skill.Description != "Ship a release" {
		t.Fatalf("parsed = %+v", skill)
	}
	if skill.Metadata["owner"] != "platform" {
		t.Fatalf("metadata = %v", skill.Metadata)
	}
	if !strings.Contains(skill.Body, "1. Build.") {
		t.Fatalf("body = %q", skill.Body)
	}
}

func TestWriteBundleIsContentAddressedAndImmutable(t *testing.T) {
	root := t.TempDir()
	doc, err := ComposeSkillDocument("demo", "description", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := BundleFromFilesNamed([]FileInput{{Path: SkillFileName, Content: doc}}, "demo")
	if err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(root, "versions", bundle.Hash)
	if err := WriteBundle(versionDir, bundle); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	// Writing the same content again is idempotent (deduplication).
	if err := WriteBundle(versionDir, bundle); err != nil {
		t.Fatalf("idempotent WriteBundle: %v", err)
	}
	if err := Verify(versionDir, bundle.Hash); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Changing the bytes under a version directory is detected, never accepted.
	if err := os.WriteFile(filepath.Join(versionDir, "extra.md"), []byte("tamper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(versionDir, bundle.Hash); !errors.Is(err, ErrBundleMismatch) {
		t.Fatalf("Verify after tamper = %v, want ErrBundleMismatch", err)
	}

	// WriteBundle refuses to replace a directory holding different content.
	otherDoc, err := ComposeSkillDocument("demo", "other description", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := BundleFromFilesNamed([]FileInput{{Path: SkillFileName, Content: otherDoc}}, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBundle(versionDir, other); !errors.Is(err, ErrBundleMismatch) {
		t.Fatalf("WriteBundle over other content = %v, want ErrBundleMismatch", err)
	}
}

func TestReplaceBundleMovesTheActiveCopy(t *testing.T) {
	root := t.TempDir()
	doc, _ := ComposeSkillDocument("demo", "description", "body", nil)
	first, err := BundleFromFilesNamed([]FileInput{{Path: SkillFileName, Content: doc}}, "demo")
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, "active", "demo")
	if err := ReplaceBundle(active, first); err != nil {
		t.Fatalf("ReplaceBundle first: %v", err)
	}
	otherDoc, _ := ComposeSkillDocument("demo", "second description", "body", nil)
	second, err := BundleFromFilesNamed([]FileInput{{Path: SkillFileName, Content: otherDoc}}, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBundle(active, second); err != nil {
		t.Fatalf("ReplaceBundle second: %v", err)
	}
	loaded, err := LoadBundle(active)
	if err != nil {
		t.Fatalf("LoadBundle active: %v", err)
	}
	if loaded.Hash != second.Hash {
		t.Fatalf("active hash = %s, want %s", loaded.Hash, second.Hash)
	}
}
