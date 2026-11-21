package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/skills"
)

// skillsStub is the HTTP surface the CLI talks to. It records write bodies so a
// test can assert the CLI sent the documented shape (and never a secret or a
// tenant).
type skillsStub struct {
	requests []string
	bodies   map[string]map[string]any
	record   skilllib.Record
	detail   skilllib.Detail
	update   skilllib.UpdateResult
	sources  []skilllib.Source
}

func newSkillsStub(t *testing.T) (*skillsStub, *httptest.Server) {
	t.Helper()
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	stub := &skillsStub{
		bodies: map[string]map[string]any{},
		record: skilllib.Record{
			Name: "deploy-flow", Description: "Ship a release", Trust: skilllib.TrustLocal,
			CurrentHash: strings.Repeat("a", 64), InstalledAt: now, UpdatedAt: now,
			Path:     "/tmp/skills/deploy-flow",
			Versions: []skilllib.Version{{Hash: strings.Repeat("a", 64), CreatedAt: now, Files: 1, Bytes: 42}},
		},
		detail: skilllib.Detail{
			Record: skilllib.Record{Name: "deploy-flow", Description: "Ship a release", Trust: skilllib.TrustLocal,
				CurrentHash: strings.Repeat("a", 64)},
			Body:  "1. Build.\n",
			Files: []skills.BundleFile{{Path: "SKILL.md", Size: 42, Hash: strings.Repeat("b", 64), Mode: 0o644}},
		},
		sources: []skilllib.Source{{ID: "team", Kind: "local", URL: "/srv/skills", Trust: "trusted", CreatedAt: now}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests = append(stub.requests, r.Method+" "+r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		if len(bytes.TrimSpace(raw)) > 0 {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("stub: bad body on %s %s: %v", r.Method, r.URL.Path, err)
			}
			stub.bodies[r.Method+" "+r.URL.Path] = body
		}
		respond := func(status int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/settings/skills":
			respond(200, map[string]any{
				"revision": 3, "root": "/tmp/skills", "checked": r.URL.RawQuery == "",
				"skills": []skilllib.Record{stub.record}, "sources": stub.sources,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/settings/skills/deploy-flow":
			respond(200, stub.detail)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/settings/skills/sources":
			respond(200, map[string]any{"sources": stub.sources})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/settings/skills":
			respond(201, stub.record)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/settings/skills/deploy-flow/update":
			stub.update = skilllib.UpdateResult{
				Status: "updated", PreviousHash: strings.Repeat("a", 64), Record: stub.record,
			}
			respond(200, stub.update)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/settings/skills/deploy-flow/pin":
			record := stub.record
			record.PinnedHash = record.CurrentHash
			respond(200, record)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/settings/skills/deploy-flow/unpin":
			respond(200, stub.record)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/settings/skills/deploy-flow":
			respond(200, map[string]any{"removed": true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/settings/skills/sources":
			respond(201, stub.sources[0])
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/settings/skills/sources/team":
			respond(200, map[string]any{"removed": true})
		default:
			t.Errorf("stub: no route for %s %s", r.Method, r.URL.Path)
			respond(404, map[string]any{"error": "no route"})
		}
	}))
	t.Cleanup(server.Close)
	return stub, server
}

func runSkillsCLI(t *testing.T, server *httptest.Server, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"skills"}, append(args, "--server", server.URL)...), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestSkillsCLIListShowAndSources(t *testing.T) {
	_, server := newSkillsStub(t)

	stdout, stderr, code := runSkillsCLI(t, server, "list")
	if code != 0 {
		t.Fatalf("list = %d: %s", code, stderr)
	}
	for _, want := range []string{"NAME", "deploy-flow", skilllib.TrustLocal, "aaaaaaaaaaaa"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("list output missing %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, code = runSkillsCLI(t, server, "list", "--check=false")
	if code != 0 {
		t.Fatalf("list --check=false = %d: %s", code, stderr)
	}

	stdout, stderr, code = runSkillsCLI(t, server, "show", "deploy-flow")
	if code != 0 {
		t.Fatalf("show = %d: %s", code, stderr)
	}
	for _, want := range []string{"description:", "Ship a release", "1. Build.", "SKILL.md"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("show output missing %q:\n%s", want, stdout)
		}
	}

	stdout, stderr, code = runSkillsCLI(t, server, "sources", "list")
	if code != 0 {
		t.Fatalf("sources list = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "team") || !strings.Contains(stdout, "trusted") {
		t.Fatalf("sources output:\n%s", stdout)
	}
}

func TestSkillsCLIInstallReadsADirectory(t *testing.T) {
	stub, server := newSkillsStub(t)
	dir := t.TempDir()
	document, err := skills.ComposeSkillDocument("deploy-flow", "Ship a release", "1. Build.\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skills.SkillFileName), document, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSkillsCLI(t, server, "install", "deploy-flow", "--dir", dir, "--pin", "--source", "team", "--allow-untrusted")
	if code != 0 {
		t.Fatalf("install = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "已安装 deploy-flow") {
		t.Fatalf("install output: %s", stdout)
	}
	body := stub.bodies["POST /v1/settings/skills"]
	if body == nil {
		t.Fatal("install sent no body")
	}
	if body["name"] != "deploy-flow" || body["sourceId"] != "team" || body["pin"] != true || body["allowUntrusted"] != true {
		t.Fatalf("install body = %+v", body)
	}
	files, ok := body["files"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %#v", body["files"])
	}
	foundScript := false
	for _, item := range files {
		entry := item.(map[string]any)
		if entry["path"] == "scripts" {
			decoded, err := base64.StdEncoding.DecodeString(entry["contentBase64"].(string))
			if err != nil || string(decoded) != "#!/bin/sh\n" {
				t.Fatalf("script bytes = %q, %v", decoded, err)
			}
			foundScript = true
		}
	}
	if !foundScript {
		t.Fatalf("the resource was not uploaded: %#v", files)
	}
}

func TestSkillsCLIInstallFromSourceNeedsNoDescription(t *testing.T) {
	stub, server := newSkillsStub(t)
	_, stderr, code := runSkillsCLI(t, server, "install", "team-review", "--source", "team")
	if code != 0 {
		t.Fatalf("install from source = %d: %s", code, stderr)
	}
	body := stub.bodies["POST /v1/settings/skills"]
	if body == nil {
		t.Fatal("install sent no body")
	}
	if body["name"] != "team-review" || body["sourceId"] != "team" {
		t.Fatalf("install body = %+v", body)
	}
	if _, has := body["files"]; has {
		t.Fatalf("a source install must not upload files: %+v", body["files"])
	}
}

func TestSkillsCLIUpdatePinUnpinRemove(t *testing.T) {
	stub, server := newSkillsStub(t)

	stdout, stderr, code := runSkillsCLI(t, server, "update", "deploy-flow")
	if code != 0 {
		t.Fatalf("update = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "已更新") {
		t.Fatalf("update output: %s", stdout)
	}
	if stub.bodies["POST /v1/settings/skills/deploy-flow/update"] == nil {
		t.Fatal("update sent no body")
	}

	stdout, stderr, code = runSkillsCLI(t, server, "pin", "deploy-flow")
	if code != 0 {
		t.Fatalf("pin = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "已固定") {
		t.Fatalf("pin output: %s", stdout)
	}
	stdout, stderr, code = runSkillsCLI(t, server, "unpin", "deploy-flow")
	if code != 0 {
		t.Fatalf("unpin = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "已解除固定") {
		t.Fatalf("unpin output: %s", stdout)
	}
	stdout, stderr, code = runSkillsCLI(t, server, "remove", "deploy-flow")
	if code != 0 {
		t.Fatalf("remove = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "已移除") {
		t.Fatalf("remove output: %s", stdout)
	}

	stdout, stderr, code = runSkillsCLI(t, server, "sources", "subscribe", "--id", "team", "--url", "/srv/skills", "--trust", "trusted")
	if code != 0 {
		t.Fatalf("sources subscribe = %d: %s", code, stderr)
	}
	if stub.bodies["POST /v1/settings/skills/sources"]["trust"] != "trusted" {
		t.Fatalf("subscribe body = %+v", stub.bodies["POST /v1/settings/skills/sources"])
	}
	stdout, stderr, code = runSkillsCLI(t, server, "sources", "unsubscribe", "team")
	if code != 0 {
		t.Fatalf("sources unsubscribe = %d: %s", code, stderr)
	}
}

func TestSkillsCLIRefusalCarriesTheServerCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"skill \"alpha\" is pinned at abc; unpin before updating to def","code":"pinned"}`))
	}))
	t.Cleanup(server.Close)
	_, stderr, code := runSkillsCLI(t, server, "update", "alpha")
	if code != 1 {
		t.Fatalf("update refusal exit = %d", code)
	}
	if !strings.Contains(stderr, "pinned") || !strings.Contains(stderr, "unpin") {
		t.Fatalf("refusal output: %s", stderr)
	}
}
