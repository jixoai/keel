// `keel skills ...`: the CLI of the tenant skill library — the same library
// behind settings /settings/skills and the agent's `skills manage` action.
//
// It is a thin HTTP client of a running `keel serve`, exactly like `keel model`
// and `keel team`: GET/POST /v1/settings/skills* are the only write path, so the
// CLI cannot drift from the page and never opens the database or the managed
// directory itself (an install from a local directory reads the files and sends
// them; the server validates and versions them).
//
// The write subset is explicit about the three states the spec asks to make
// visible: a skill's trust (trusted/untrusted/local), a pinned version, and an
// update conflict (`pinned`, `modified`, `source_missing`, `name_mismatch`). A
// pinned skill whose source moved is refused with the server's code; the CLI
// prints it instead of retrying with a bypass.
package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/skills"
)

func runSkillsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		skillsUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runSkillsList(args[1:], stdout, stderr)
	case "show":
		return runSkillsShow(args[1:], stdout, stderr)
	case "install":
		return runSkillsInstall(args[1:], stdout, stderr)
	case "update":
		return runSkillsUpdate(args[1:], stdout, stderr)
	case "pin":
		return runSkillsPin(args[1:], stdout, stderr)
	case "unpin":
		return runSkillsUnpin(args[1:], stdout, stderr)
	case "remove", "delete":
		return runSkillsRemove(args[1:], stdout, stderr)
	case "sources":
		return runSkillsSourcesCmd(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		skillsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "keel skills: unknown command %q\n\n", args[0])
		skillsUsage(stderr)
		return 2
	}
}

func skillsUsage(w io.Writer) {
	fmt.Fprint(w, `keel skills — manage the tenant skill library

Usage:
  keel skills list [--check=false] [--json]
  keel skills show <name> [--json]
  keel skills install <name> [--description <text>] [--body <text>|--body-file <path>]
                             [--dir <path>] [--source <id>] [--trust local|trusted|untrusted]
                             [--allow-untrusted] [--pin]
  keel skills install --dir <path> [--source <id>] [--allow-untrusted] [--pin]
  keel skills update <name> [--dir <path>] [--allow-untrusted]
  keel skills pin <name> [--hash <bundle-hash>]
  keel skills unpin <name>
  keel skills remove <name>
  keel skills sources list [--json]
  keel skills sources subscribe --id <id> --url <directory> [--kind local] [--trust trusted|untrusted]
  keel skills sources unsubscribe <id>

Commands:
  list       every installed skill: trust, current/pinned version, update and
             conflict state (--check=false skips reading the sources)
  show       one skill with its body and resource manifest
  install    install a bundle from a directory (--dir) or from the convenience
             form (--description/--body); --source records a subscription as
             the update origin, and an untrusted source needs --allow-untrusted
  update     advance a skill to its source's current bundle; a pinned skill is
             refused with the server's "pinned" conflict (unpin first)
  pin/unpin  hold an immutable version, or release it
  remove     uninstall a skill (its immutable versions stay until unreferenced)
  sources    list/subscribe/unsubscribe update sources; a source is refused
             while an installed skill still uses it

Common flags:
  --server <addr>   keel serve address (default $KEEL_SERVE_ADDR or 127.0.0.1:13750)
  --json            print the raw response instead of a table

The library is tenant-scoped by the server session. A skill's scripts are never
executed by this CLI (or by the library): run them through the bash tool so the
sandbox, job accounting and output bounds apply.
Authentication: KEEL_SERVE_PASSWORD is exchanged for the session cookie
(POST /v1/auth/login); it is never taken from a flag and never printed.
`)
}

// skillsListBody mirrors GET /v1/settings/skills.
type skillsListBody struct {
	Revision int64             `json:"revision"`
	Root     string            `json:"root"`
	Checked  bool              `json:"checked"`
	Skills   []skilllib.Record `json:"skills"`
	Sources  []skilllib.Source `json:"sources"`
}

func runSkillsList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills list", stderr)
	server, asJSON := commonFlags(fs)
	check := fs.Bool("check", true, "read each source and report updates/conflicts")
	if _, err := parseInterleaved(fs, args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body skillsListBody
	path := "/v1/settings/skills"
	if !*check {
		path += "?check=0"
	}
	if err := client.do(http.MethodGet, path, "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	if len(body.Skills) == 0 {
		fmt.Fprintln(stdout, "还没有技能。用 keel skills install 或让 Agent 调用 skills.manage create 创建一个。")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTRUST\tVERSION\tPINNED\tUPDATE\tCONFLICT\tDESCRIPTION")
	for _, skill := range body.Skills {
		update := "-"
		if skill.UpdateAvailable {
			update = shortHash(skill.AvailableHash)
		}
		pinned := "-"
		if skill.PinnedHash != "" {
			pinned = shortHash(skill.PinnedHash)
		}
		conflict := skill.Conflict
		if conflict == "" {
			conflict = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			skill.Name, skill.Trust, shortHash(skill.CurrentHash), pinned, update, conflict, skill.Description)
	}
	if err := w.Flush(); err != nil {
		return reportError(stderr, err)
	}
	fmt.Fprintf(stdout, "\n%d 个技能 · 版本 %d\n", len(body.Skills), body.Revision)
	for _, skill := range body.Skills {
		for _, diagnostic := range skill.Diagnostics {
			fmt.Fprintf(stdout, "  %s: %s\n", skill.Name, diagnostic)
		}
	}
	return 0
}

func runSkillsShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills show", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "keel skills show: a skill name is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var detail skilllib.Detail
	if err := client.do(http.MethodGet, "/v1/settings/skills/"+url.PathEscape(name), "", nil, &detail); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, detail); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "name:        %s\n", detail.Name)
	fmt.Fprintf(stdout, "description: %s\n", detail.Description)
	fmt.Fprintf(stdout, "trust:       %s\n", detail.Trust)
	fmt.Fprintf(stdout, "current:     %s\n", detail.CurrentHash)
	if detail.PinnedHash != "" {
		fmt.Fprintf(stdout, "pinned:      %s\n", detail.PinnedHash)
	}
	if detail.SourceID != "" {
		fmt.Fprintf(stdout, "source:      %s (%s) %s\n", detail.SourceID, detail.SourceKind, detail.SourceURL)
	}
	if detail.Conflict != "" {
		fmt.Fprintf(stdout, "conflict:    %s\n", detail.Conflict)
	}
	if detail.UpdateAvailable {
		fmt.Fprintf(stdout, "update:      %s\n", detail.AvailableHash)
	}
	fmt.Fprintf(stdout, "path:        %s\n", detail.Path)
	fmt.Fprintf(stdout, "versions:    %d\n", len(detail.Versions))
	for _, version := range detail.Versions {
		fmt.Fprintf(stdout, "  - %s  %d files  %d bytes  %s\n",
			shortHash(version.Hash), version.Files, version.Bytes, version.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	fmt.Fprintln(stdout, "\nresources:")
	for _, file := range detail.Files {
		fmt.Fprintf(stdout, "  %s  %d bytes\n", file.Path, file.Size)
	}
	fmt.Fprintf(stdout, "\n--- %s ---\n%s\n", skills.SkillFileName, strings.TrimRight(detail.Body, "\n"))
	return 0
}

func runSkillsInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills install", stderr)
	server, asJSON := commonFlags(fs)
	description := fs.String("description", "", "skill description (one line)")
	bodyText := fs.String("body", "", "SKILL.md body")
	bodyFile := fs.String("body-file", "", "read the SKILL.md body from a file")
	dir := fs.String("dir", "", "bundle directory to upload (SKILL.md at its root, or a directory of skills)")
	source := fs.String("source", "", "subscription source id (the update origin)")
	trust := fs.String("trust", skilllib.TrustLocal, "trust for a manual install: local|trusted|untrusted")
	allowUntrusted := fs.Bool("allow-untrusted", false, "acknowledge an untrusted source")
	pin := fs.Bool("pin", false, "pin the installed version")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	name := firstPositional(positional)
	bundleDir := strings.TrimSpace(*dir)
	if bundleDir == "" && name != "" && isDirectory(name) {
		bundleDir, name = name, ""
	}
	if bundleDir == "" && name == "" {
		fmt.Fprintln(stderr, "keel skills install: a skill name or --dir is required")
		return 2
	}
	if bundleDir == "" && strings.TrimSpace(*description) == "" {
		fmt.Fprintln(stderr, "keel skills install: --description is required when installing without --dir")
		return 2
	}
	body := *bodyText
	if *bodyFile != "" {
		raw, err := os.ReadFile(*bodyFile)
		if err != nil {
			return reportError(stderr, err)
		}
		body = string(raw)
	}
	request := map[string]any{
		"name":           name,
		"description":    *description,
		"body":           body,
		"sourceId":       *source,
		"trust":          *trust,
		"allowUntrusted": *allowUntrusted,
		"pin":            *pin,
	}
	if bundleDir != "" {
		files, err := readBundleFiles(bundleDir, name)
		if err != nil {
			return reportError(stderr, err)
		}
		request["files"] = files
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var record skilllib.Record
	if err := client.do(http.MethodPost, "/v1/settings/skills", "", request, &record); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, record); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "已安装 %s（%s，trust=%s，%d 个版本）\n",
		record.Name, shortHash(record.CurrentHash), record.Trust, len(record.Versions))
	return 0
}

func runSkillsUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills update", stderr)
	server, asJSON := commonFlags(fs)
	dir := fs.String("dir", "", "update from a bundle directory instead of the recorded source")
	allowUntrusted := fs.Bool("allow-untrusted", false, "acknowledge an untrusted source")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "keel skills update: a skill name is required")
		return 2
	}
	request := map[string]any{"allowUntrusted": *allowUntrusted}
	if *dir != "" {
		files, err := readBundleFiles(*dir, name)
		if err != nil {
			return reportError(stderr, err)
		}
		request["files"] = files
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var result skilllib.UpdateResult
	path := "/v1/settings/skills/" + url.PathEscape(name) + "/update"
	if err := client.do(http.MethodPost, path, "", request, &result); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, result); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	switch result.Status {
	case "up_to_date":
		fmt.Fprintf(stdout, "%s 已是最新版本（%s）\n", name, shortHash(result.Record.CurrentHash))
	case "updated":
		fmt.Fprintf(stdout, "%s 已更新：%s → %s（%d 个版本）\n",
			name, shortHash(result.PreviousHash), shortHash(result.Record.CurrentHash), len(result.Record.Versions))
	default:
		fmt.Fprintf(stdout, "%s 更新状态：%s\n", name, result.Status)
	}
	return 0
}

func runSkillsPin(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills pin", stderr)
	server, asJSON := commonFlags(fs)
	hash := fs.String("hash", "", "bundle hash to activate and pin (default: the current version)")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "keel skills pin: a skill name is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var record skilllib.Record
	path := "/v1/settings/skills/" + url.PathEscape(name) + "/pin"
	if err := client.do(http.MethodPost, path, "", map[string]any{"hash": *hash}, &record); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, record); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s 已固定在 %s\n", name, shortHash(record.PinnedHash))
	return 0
}

func runSkillsUnpin(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills unpin", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "keel skills unpin: a skill name is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var record skilllib.Record
	path := "/v1/settings/skills/" + url.PathEscape(name) + "/unpin"
	if err := client.do(http.MethodPost, path, "", nil, &record); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, record); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s 已解除固定（当前 %s）\n", name, shortHash(record.CurrentHash))
	return 0
}

func runSkillsRemove(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills remove", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	name := firstPositional(positional)
	if name == "" {
		fmt.Fprintln(stderr, "keel skills remove: a skill name is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body map[string]any
	if err := client.do(http.MethodDelete, "/v1/settings/skills/"+url.PathEscape(name), "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "已移除 %s\n", name)
	return 0
}

func runSkillsSourcesCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, `keel skills sources — list/subscribe/unsubscribe update sources

Usage:
  keel skills sources list [--json]
  keel skills sources subscribe --id <id> --url <directory> [--kind local] [--trust trusted|untrusted]
  keel skills sources unsubscribe <id>
`)
		return 2
	}
	switch args[0] {
	case "list":
		return runSkillsSourcesList(args[1:], stdout, stderr)
	case "subscribe":
		return runSkillsSourcesSubscribe(args[1:], stdout, stderr)
	case "unsubscribe":
		return runSkillsSourcesUnsubscribe(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "keel skills sources: unknown command %q\n", args[0])
		return 2
	}
}

func runSkillsSourcesList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills sources list", stderr)
	server, asJSON := commonFlags(fs)
	if _, err := parseInterleaved(fs, args); err != nil {
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body struct {
		Sources []skilllib.Source `json:"sources"`
	}
	if err := client.do(http.MethodGet, "/v1/settings/skills/sources", "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	if len(body.Sources) == 0 {
		fmt.Fprintln(stdout, "没有订阅源。")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tKIND\tTRUST\tURL")
	for _, source := range body.Sources {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", source.ID, source.Kind, source.Trust, source.URL)
	}
	if err := w.Flush(); err != nil {
		return reportError(stderr, err)
	}
	return 0
}

func runSkillsSourcesSubscribe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills sources subscribe", stderr)
	server, asJSON := commonFlags(fs)
	id := fs.String("id", "", "source id")
	kind := fs.String("kind", skilllib.SourceLocal, "source kind (local)")
	sourceURL := fs.String("url", "", "source directory")
	trust := fs.String("trust", skilllib.TrustUntrusted, "trusted|untrusted")
	if _, err := parseInterleaved(fs, args); err != nil {
		return 2
	}
	if *id == "" || *sourceURL == "" {
		fmt.Fprintln(stderr, "keel skills sources subscribe: --id and --url are required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var source skilllib.Source
	if err := client.do(http.MethodPost, "/v1/settings/skills/sources", "", map[string]any{
		"id": *id, "kind": *kind, "url": *sourceURL, "trust": *trust,
	}, &source); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, source); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "已订阅 %s（%s，trust=%s）\n", source.ID, source.Kind, source.Trust)
	return 0
}

func runSkillsSourcesUnsubscribe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("keel skills sources unsubscribe", stderr)
	server, asJSON := commonFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	id := firstPositional(positional)
	if id == "" {
		fmt.Fprintln(stderr, "keel skills sources unsubscribe: a source id is required")
		return 2
	}
	client, err := newCLIClient(*server)
	if err != nil {
		return reportError(stderr, err)
	}
	var body map[string]any
	if err := client.do(http.MethodDelete, "/v1/settings/skills/sources/"+url.PathEscape(id), "", nil, &body); err != nil {
		return reportError(stderr, err)
	}
	if *asJSON {
		if err := printJSON(stdout, body); err != nil {
			return reportError(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "已取消订阅 %s\n", id)
	return 0
}

// readBundleFiles reads a bundle directory into the wire form. Content is
// base64 so a binary resource round-trips byte-exactly; the server re-validates
// paths, modes and the bundle limits.
func readBundleFiles(dir, name string) ([]map[string]any, error) {
	bundle, err := loadCLIBundle(dir, name)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(bundle.Files))
	for _, file := range bundle.Files {
		content, ok := bundle.ReadFile(file.Path)
		if !ok {
			return nil, fmt.Errorf("bundle %s is missing %s", dir, file.Path)
		}
		out = append(out, map[string]any{
			"path":          file.Path,
			"contentBase64": base64.StdEncoding.EncodeToString(content),
			"mode":          file.Mode,
		})
	}
	return out, nil
}

func loadCLIBundle(dir, name string) (*skills.Bundle, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	candidates := []string{abs}
	if name != "" {
		candidates = append(candidates, filepath.Join(abs, name))
	}
	var lastErr error
	for _, candidate := range candidates {
		if _, statErr := os.Stat(filepath.Join(candidate, skills.SkillFileName)); statErr != nil {
			continue
		}
		bundle, loadErr := skills.LoadBundle(candidate)
		if loadErr != nil {
			lastErr = loadErr
			continue
		}
		if name != "" && bundle.Skill.Name != name {
			return nil, fmt.Errorf("the bundle at %s declares the skill %q, not %q", candidate, bundle.Skill.Name, name)
		}
		return bundle, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%s holds no %s", dir, skills.SkillFileName)
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// firstPositional returns the first non-flag argument (parseInterleaved keeps
// flags workable after a positional, e.g. `keel skills show pdf --json`).
func firstPositional(positional []string) string {
	if len(positional) == 0 {
		return ""
	}
	return strings.TrimSpace(positional[0])
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
