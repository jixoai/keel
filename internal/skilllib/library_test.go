package skilllib_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/skills"
)

var testTenant = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func newLibrary(t *testing.T) *skilllib.Library {
	t.Helper()
	return skilllib.New(skilllib.Options{Root: t.TempDir()})
}

func writeSourceSkill(t *testing.T, dir, name, description, body string, resources map[string]string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	document, err := skills.ComposeSkillDocument(name, description, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, skills.SkillFileName), document, 0o644); err != nil {
		t.Fatal(err)
	}
	for path, content := range resources {
		target := filepath.Join(skillDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *skilllib.Error
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want skilllib.Error %q", err, code)
	}
	if typed.Code != code {
		t.Fatalf("error code = %q (%v), want %q", typed.Code, typed, code)
	}
}

func TestInstallContentAddressedAndImmutable(t *testing.T) {
	library := newLibrary(t)
	first, err := library.Install(testTenant, skilllib.InstallRequest{
		Name: "deploy-flow", Description: "Ship a release", Body: "1. Build.\n",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if first.Trust != skilllib.TrustLocal {
		t.Fatalf("trust = %q, want local", first.Trust)
	}
	if first.CurrentHash == "" || len(first.Versions) != 1 {
		t.Fatalf("record = %+v", first)
	}
	if _, err := os.Stat(filepath.Join(first.ActivePath, skills.SkillFileName)); err != nil {
		t.Fatalf("active SKILL.md missing: %v", err)
	}

	// Same content, same identity: the version is deduplicated.
	same, err := library.Install(testTenant, skilllib.InstallRequest{
		Name: "deploy-flow", Description: "Ship a release", Body: "1. Build.\n",
	})
	if err != nil {
		t.Fatalf("idempotent Install: %v", err)
	}
	if same.CurrentHash != first.CurrentHash || len(same.Versions) != 1 {
		t.Fatalf("dedupe failed: %+v", same)
	}

	// New content creates a second version; the first stays readable.
	second, err := library.Install(testTenant, skilllib.InstallRequest{
		Name: "deploy-flow", Description: "Ship a release", Body: "1. Build.\n2. Publish.\n",
	})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if second.CurrentHash == first.CurrentHash || len(second.Versions) != 2 {
		t.Fatalf("second record = %+v", second)
	}
	snapshot, err := library.List(testTenant, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snapshot.Skills) != 1 || len(snapshot.Skills[0].Versions) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	versionDir := filepath.Join(snapshot.Root, ".versions", first.CurrentHash)
	if _, err := skills.LoadBundle(versionDir); err != nil {
		t.Fatalf("first immutable version disappeared: %v", err)
	}
}

func TestVersionContentIsDeduplicatedAcrossSwitches(t *testing.T) {
	library := newLibrary(t)
	first, err := library.Install(testTenant, skilllib.InstallRequest{Name: "dedupe", Description: "v1", Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := library.Install(testTenant, skilllib.InstallRequest{Name: "dedupe", Description: "v2", Body: "two"})
	if err != nil {
		t.Fatal(err)
	}
	// Pin back to v1, then install v2's exact bytes again: the second version is
	// reused, not written a third time.
	if _, err := library.Pin(testTenant, "dedupe", first.CurrentHash); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Unpin(testTenant, "dedupe"); err != nil {
		t.Fatal(err)
	}
	again, err := library.Install(testTenant, skilllib.InstallRequest{Name: "dedupe", Description: "v2", Body: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if again.CurrentHash != second.CurrentHash {
		t.Fatalf("reinstall hash = %s, want %s", again.CurrentHash, second.CurrentHash)
	}
	if len(again.Versions) != 2 {
		t.Fatalf("versions = %d, want 2 (content-addressed dedupe)", len(again.Versions))
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(filepath.Dir(again.ActivePath)), ".versions"))
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("version directories = %d, want 2", len(entries))
	}
}

func TestModifiedActiveCopyIsAConflictNotAnUpdate(t *testing.T) {
	library := newLibrary(t)
	record, err := library.Install(testTenant, skilllib.InstallRequest{
		Name: "hand-edited", Description: "d", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Editing the active copy is outside the install path; the immutability check
	// must name it, and the version directory must stay untouched.
	if err := os.WriteFile(filepath.Join(record.ActivePath, "extra.md"), []byte("drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := library.List(testTenant, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Skills[0].Conflict; got != skilllib.ConflictModified {
		t.Fatalf("conflict = %q, want %q", got, skilllib.ConflictModified)
	}
	versionDir := filepath.Join(snapshot.Root, ".versions", record.CurrentHash)
	if _, err := skills.LoadBundle(versionDir); err != nil {
		t.Fatalf("version was modified by the active-copy drift: %v", err)
	}
}

func TestSourceTrustPinAndUpdateConflicts(t *testing.T) {
	library := newLibrary(t)
	sourceDir := filepath.Join(library.SourceRoot(), "team")
	writeSourceSkill(t, sourceDir, "alpha", "alpha v1", "body v1", map[string]string{"references/a.md": "one\n"})

	source, err := library.Subscribe(testTenant, skilllib.Source{
		ID: "team", Kind: skilllib.SourceLocal, URL: "team", Trust: skilllib.TrustTrusted,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if source.Trust != skilllib.TrustTrusted {
		t.Fatalf("source trust = %q", source.Trust)
	}
	installed, err := library.Install(testTenant, skilllib.InstallRequest{Name: "alpha", SourceID: "team"})
	if err != nil {
		t.Fatalf("Install from source: %v", err)
	}
	if installed.Trust != skilllib.TrustTrusted || installed.SourceID != "team" {
		t.Fatalf("installed = %+v", installed)
	}
	checked, err := library.List(testTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Skills[0].UpdateAvailable {
		t.Fatal("a fresh install must not report an update")
	}

	// The source moves: the read reports availability without changing anything.
	writeSourceSkill(t, sourceDir, "alpha", "alpha v2", "body v2", nil)
	checked, err = library.List(testTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	record := checked.Skills[0]
	if !record.UpdateAvailable || record.AvailableHash == "" {
		t.Fatalf("update not visible: %+v", record)
	}
	if record.CurrentHash != installed.CurrentHash {
		t.Fatal("List(check) changed the installed version")
	}

	updated, err := library.Update(testTenant, "alpha", skilllib.UpdateRequest{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != "updated" || updated.Record.CurrentHash != record.AvailableHash {
		t.Fatalf("update result = %+v", updated)
	}
	if updated.Record.Description != "alpha v2" {
		t.Fatalf("description = %q", updated.Record.Description)
	}

	// Pin holds the version: an update is a typed conflict, visible on read.
	pinned, err := library.Pin(testTenant, "alpha", "")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if pinned.PinnedHash != pinned.CurrentHash {
		t.Fatalf("pinned = %+v", pinned)
	}
	writeSourceSkill(t, sourceDir, "alpha", "alpha v3", "body v3", nil)
	checked, err = library.List(testTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Skills[0].Conflict != skilllib.ConflictPinned {
		t.Fatalf("pinned conflict = %q", checked.Skills[0].Conflict)
	}
	_, err = library.Update(testTenant, "alpha", skilllib.UpdateRequest{})
	requireCode(t, err, skilllib.CodePinned)

	// Unpin releases it; the same update now applies.
	if _, err := library.Unpin(testTenant, "alpha"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	updated, err = library.Update(testTenant, "alpha", skilllib.UpdateRequest{})
	if err != nil {
		t.Fatalf("Update after unpin: %v", err)
	}
	if updated.Status != "updated" || updated.Record.Description != "alpha v3" {
		t.Fatalf("update after unpin = %+v", updated)
	}
}

func TestUntrustedSourceNeedsAnExplicitAcknowledgement(t *testing.T) {
	library := newLibrary(t)
	sourceDir := filepath.Join(library.SourceRoot(), "unknown")
	writeSourceSkill(t, sourceDir, "beta", "beta v1", "body", nil)
	if _, err := library.Subscribe(testTenant, skilllib.Source{
		ID: "unknown", Kind: skilllib.SourceLocal, URL: "unknown", Trust: skilllib.TrustUntrusted,
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	_, err := library.Install(testTenant, skilllib.InstallRequest{Name: "beta", SourceID: "unknown"})
	requireCode(t, err, skilllib.CodeUntrusted)
	installed, err := library.Install(testTenant, skilllib.InstallRequest{
		Name: "beta", SourceID: "unknown", AllowUntrusted: true,
	})
	if err != nil {
		t.Fatalf("Install with acknowledgement: %v", err)
	}
	if installed.Trust != skilllib.TrustUntrusted {
		t.Fatalf("trust = %q, want untrusted", installed.Trust)
	}

	writeSourceSkill(t, sourceDir, "beta", "beta v2", "body v2", nil)
	_, err = library.Update(testTenant, "beta", skilllib.UpdateRequest{})
	requireCode(t, err, skilllib.CodeUntrusted)
	updated, err := library.Update(testTenant, "beta", skilllib.UpdateRequest{AllowUntrusted: true})
	if err != nil {
		t.Fatalf("Update with acknowledgement: %v", err)
	}
	if updated.Status != "updated" {
		t.Fatalf("update = %+v", updated)
	}

	// The source cannot be removed while it is the skill's origin.
	_, err = library.Unsubscribe(testTenant, "unknown")
	requireCode(t, err, skilllib.CodeSourceInUse)
	if _, err := library.Remove(testTenant, "beta"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := library.Unsubscribe(testTenant, "unknown"); err != nil {
		t.Fatalf("Unsubscribe after remove: %v", err)
	}
}

func TestSourceNameMismatchAndMissingSourceAreConflicts(t *testing.T) {
	library := newLibrary(t)
	sourceDir := filepath.Join(library.SourceRoot(), "src")
	writeSourceSkill(t, sourceDir, "gamma", "gamma", "body", nil)
	if _, err := library.Subscribe(testTenant, skilllib.Source{
		ID: "src", Kind: skilllib.SourceLocal, URL: "src", Trust: skilllib.TrustTrusted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Install(testTenant, skilllib.InstallRequest{Name: "gamma", SourceID: "src"}); err != nil {
		t.Fatal(err)
	}
	// A source whose SKILL.md now declares another name is a conflict, not an
	// update: installing it would silently repoint the skill.
	if err := os.WriteFile(filepath.Join(sourceDir, "gamma", skills.SkillFileName), []byte("---\nname: other\ndescription: x\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checked, err := library.List(testTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Skills[0].Conflict != skilllib.ConflictNameChanged {
		t.Fatalf("conflict = %q, want name_mismatch", checked.Skills[0].Conflict)
	}
	if _, err := library.Update(testTenant, "gamma", skilllib.UpdateRequest{}); err == nil {
		t.Fatal("Update accepted a renamed source")
	}

	// A source directory that disappeared is visible too.
	if err := os.RemoveAll(sourceDir); err != nil {
		t.Fatal(err)
	}
	checked, err = library.List(testTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Skills[0].Conflict != skilllib.ConflictSourceMiss {
		t.Fatalf("conflict = %q, want source_missing", checked.Skills[0].Conflict)
	}
}

func TestManageBackendCreatesVersionsAndNeverExecutes(t *testing.T) {
	library := newLibrary(t)
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: testTenant.String(), SessionID: "22222222-2222-4222-8222-222222222222",
	})
	created, err := library.SkillsManage(ctx, "create", json.RawMessage(`{
		"name":"learned-flow","description":"What we learned","body":"Run the migration first."
	}`))
	if err != nil {
		t.Fatalf("manage create: %v", err)
	}
	record, ok := created.(skilllib.Record)
	if !ok {
		t.Fatalf("create result = %T", created)
	}
	if record.Trust != skilllib.TrustUntrusted || len(record.Versions) != 1 {
		t.Fatalf("created = %+v", record)
	}

	patched, err := library.SkillsManage(ctx, "patch", json.RawMessage(`{
		"name":"learned-flow","body":"Run the migration first, then verify."
	}`))
	if err != nil {
		t.Fatalf("manage patch: %v", err)
	}
	patchedMap := patched.(map[string]any)
	if patchedMap["status"] != "updated" {
		t.Fatalf("patch status = %v", patchedMap["status"])
	}
	detail, err := library.Show(testTenant, "learned-flow")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.Body, "then verify") || len(detail.Versions) != 2 {
		t.Fatalf("detail = %+v", detail)
	}

	// A resource — including an executable-looking script — is stored as bytes.
	script := "#!/bin/sh\necho hi\n"
	if _, err := library.SkillsManage(ctx, "write_file", json.RawMessage(`{
		"name":"learned-flow","path":"scripts/deploy.sh","content":"#!/bin/sh\necho hi\n","mode":493
	}`)); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	detail, err = library.Show(testTenant, "learned-flow")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range detail.Files {
		if file.Path == "scripts/deploy.sh" {
			found = true
			if file.Mode != 0o755 {
				t.Fatalf("script mode = %#o, want 0755", file.Mode)
			}
		}
	}
	if !found {
		t.Fatalf("script resource missing: %+v", detail.Files)
	}
	bundle, err := library.Bundle(testTenant, "learned-flow")
	if err != nil {
		t.Fatal(err)
	}
	content, ok := bundle.ReadFile("scripts/deploy.sh")
	if !ok || string(content) != script {
		t.Fatalf("script bytes = %q, %v", content, ok)
	}
	// Nothing in the library executed it: there is no output, no side effect and
	// no process. Execution is the bash tool's path.

	if _, err := library.SkillsManage(ctx, "remove_file", json.RawMessage(`{
		"name":"learned-flow","path":"scripts/deploy.sh"
	}`)); err != nil {
		t.Fatalf("remove_file: %v", err)
	}
	detail, err = library.Show(testTenant, "learned-flow")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Files) != 1 {
		t.Fatalf("files after remove_file = %+v", detail.Files)
	}
	if _, err := library.SkillsManage(ctx, "delete", json.RawMessage(`{"name":"learned-flow"}`)); err != nil {
		t.Fatalf("manage delete: %v", err)
	}
	snapshot, err := library.List(testTenant, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Skills) != 0 {
		t.Fatalf("skills after delete = %+v", snapshot.Skills)
	}

	// The script-execution surface is deliberately absent from the closed ops.
	_, err = library.SkillsManage(ctx, "run", json.RawMessage(`{"name":"learned-flow","path":"scripts/deploy.sh"}`))
	requireCode(t, err, skilllib.CodeInvalid)
}

func TestSourcesBackendAndDiscovery(t *testing.T) {
	library := newLibrary(t)
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: testTenant.String(), SessionID: "33333333-3333-4333-8333-333333333333",
	})
	sourceDir := filepath.Join(library.SourceRoot(), "team")
	writeSourceSkill(t, sourceDir, "shared", "shared instructions", "body", nil)

	subscribed, err := library.SkillsSources(ctx, "subscribe", json.RawMessage(`{
		"id":"team","kind":"local","url":"team","trust":"trusted"
	}`))
	if err != nil {
		t.Fatalf("sources subscribe: %v", err)
	}
	// The request says trusted; the model face must record untrusted anyway.
	if subscribed.(skilllib.Source).Trust != skilllib.TrustUntrusted {
		t.Fatalf("model subscription trust = %q, want untrusted", subscribed.(skilllib.Source).Trust)
	}
	listed, err := library.SkillsSources(ctx, "list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.(map[string]any)["sources"].([]skilllib.Source)) != 1 {
		t.Fatalf("sources = %+v", listed)
	}

	if _, err := library.SkillsManage(ctx, "create", json.RawMessage(`{
		"name":"local-skill","description":"d","body":"b"
	}`)); err != nil {
		t.Fatal(err)
	}
	discovered, err := library.SkillsList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].Name != "local-skill" {
		t.Fatalf("discovered = %+v", discovered)
	}
	if !strings.HasSuffix(discovered[0].Path, filepath.Join("active", "local-skill", skills.SkillFileName)) {
		t.Fatalf("discovered path = %q", discovered[0].Path)
	}

	if _, err := library.SkillsSources(ctx, "unsubscribe", json.RawMessage(`{"id":"team"}`)); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
}

func TestInstallRejectsAPathLikeName(t *testing.T) {
	library := newLibrary(t)
	for _, name := range []string{"../escape", "a/b", "UPPER", "-leading"} {
		_, err := library.Install(testTenant, skilllib.InstallRequest{
			Name: name, Description: "d", Body: "b",
		})
		requireCode(t, err, skilllib.CodeInvalid)
	}
}

func TestManagedSourceRootRejectsHostPathsAndSymlinkEscapes(t *testing.T) {
	library := newLibrary(t)
	if err := os.MkdirAll(library.SourceRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeSourceSkill(t, outside, "evil", "evil", "body", nil)
	for _, url := range []string{"/etc", outside, ".."} {
		_, err := library.Subscribe(testTenant, skilllib.Source{
			ID: "bad", Kind: skilllib.SourceLocal, URL: url, Trust: skilllib.TrustUntrusted,
		})
		requireCode(t, err, skilllib.CodeSourceOutside)
	}

	escape := filepath.Join(library.SourceRoot(), "escape")
	if err := os.Symlink(outside, escape); err == nil {
		_, err := library.Subscribe(testTenant, skilllib.Source{
			ID: "escape", Kind: skilllib.SourceLocal, URL: "escape", Trust: skilllib.TrustUntrusted,
		})
		requireCode(t, err, skilllib.CodeSourceOutside)
	}

	// A legitimate directory inside the managed root is accepted, and an install
	// from it succeeds for the operator face.
	writeSourceSkill(t, filepath.Join(library.SourceRoot(), "team"), "alpha", "alpha", "body", nil)
	if _, err := library.Subscribe(testTenant, skilllib.Source{
		ID: "team", Kind: skilllib.SourceLocal, URL: "team", Trust: skilllib.TrustTrusted,
	}); err != nil {
		t.Fatalf("managed source: %v", err)
	}
	if _, err := library.Install(testTenant, skilllib.InstallRequest{Name: "alpha", SourceID: "team"}); err != nil {
		t.Fatalf("install from managed source: %v", err)
	}
}

func TestModelFaceCannotChooseTrustOrReadHostTrees(t *testing.T) {
	library := newLibrary(t)
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: testTenant.String(), SessionID: "44444444-4444-4444-8444-444444444444",
	})
	writeSourceSkill(t, filepath.Join(library.SourceRoot(), "team"), "alpha", "alpha", "body", nil)

	_, err := library.SkillsSources(ctx, "subscribe", json.RawMessage(`{
		"id":"evil","kind":"local","url":"/etc","trust":"trusted","allowUntrusted":true
	}`))
	requireCode(t, err, skilllib.CodeSourceOutside)

	// A managed source is allowed, but the trust argument has no effect.
	subscribed, err := library.SkillsSources(ctx, "subscribe", json.RawMessage(`{
		"id":"team","kind":"local","url":"team","trust":"trusted","allowUntrusted":true
	}`))
	if err != nil {
		t.Fatalf("managed subscribe: %v", err)
	}
	if subscribed.(skilllib.Source).Trust != skilllib.TrustUntrusted {
		t.Fatalf("trust = %q, want untrusted", subscribed.(skilllib.Source).Trust)
	}

	created, err := library.SkillsManage(ctx, "create", json.RawMessage(`{
		"name":"model-made","description":"d","body":"b","trust":"trusted","allowUntrusted":true
	}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.(skilllib.Record).Trust != skilllib.TrustUntrusted {
		t.Fatalf("model-created trust = %q, want untrusted", created.(skilllib.Record).Trust)
	}

	// A model create that names a trusted source still records untrusted: the
	// trust label is not a request field on this face.
	if _, err := library.Subscribe(testTenant, skilllib.Source{
		ID: "ops", Kind: skilllib.SourceLocal, URL: "team", Trust: skilllib.TrustTrusted,
	}); err != nil {
		t.Fatalf("operator subscribe: %v", err)
	}
	fromTrusted, err := library.SkillsManage(ctx, "create", json.RawMessage(`{
		"name":"alpha","description":"from source","source":"ops","trust":"trusted","allowUntrusted":true
	}`))
	if err != nil {
		t.Fatalf("create from trusted source: %v", err)
	}
	if fromTrusted.(skilllib.Record).Trust != skilllib.TrustUntrusted {
		t.Fatalf("model source install trust = %q, want untrusted", fromTrusted.(skilllib.Record).Trust)
	}
}

func TestNoContextOrTenantIsRefused(t *testing.T) {
	library := newLibrary(t)
	_, err := library.SkillsManage(context.Background(), "create", json.RawMessage(`{"name":"x","description":"d"}`))
	requireCode(t, err, skilllib.CodeInvalid)
	_, err = library.List(uuid.Nil, false)
	requireCode(t, err, skilllib.CodeInvalid)

	// A library with no root is not silently a no-op.
	unconfigured := skilllib.New(skilllib.Options{})
	_, err = unconfigured.List(testTenant, false)
	requireCode(t, err, skilllib.CodeLibraryNotWired)
}
