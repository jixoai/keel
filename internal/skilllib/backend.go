package skilllib

// The toolsurface backend of the `skills` aggregate tool.
//
// `skills manage` is how an agent turns session experience into a skill
// (`/learn`), edits a skill, or removes one. The operation set is the frozen
// one from the tool-surface spec (create/patch/put/delete/write_file/
// remove_file); each operation produces a *new immutable version* through the
// same library the settings page and CLI use, so the three surfaces cannot
// disagree about what is installed.
//
// Script execution is deliberately absent. A resource named scripts/deploy.sh
// is stored as bytes; a model that wants to run it calls the bash tool, which
// is where sandboxing, job accounting, timeout-to-background and output bounds
// live. Nothing in this file (or in internal/skills) imports os/exec.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/skills"
)

// SkillsList implements toolsurface.SkillsBackend.
func (l *Library) SkillsList(ctx context.Context) ([]skills.Skill, error) {
	tenantID, err := scopeTenant(ctx)
	if err != nil {
		return nil, err
	}
	return l.SkillsForDiscovery(ctx, tenantID)
}

// SkillsManage implements toolsurface.SkillsBackend.
func (l *Library) SkillsManage(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	tenantID, err := scopeTenant(ctx)
	if err != nil {
		return nil, err
	}
	args, err := decodeArgs(raw)
	if err != nil {
		return nil, err
	}
	switch op {
	case "create":
		return l.manageCreate(tenantID, args)
	case "patch":
		return l.managePatch(tenantID, args)
	case "put":
		return l.managePut(tenantID, args)
	case "delete":
		return l.manageDelete(tenantID, args)
	case "write_file":
		return l.manageWriteFile(tenantID, args)
	case "remove_file":
		return l.manageRemoveFile(tenantID, args)
	default:
		return nil, errorf(CodeInvalid, "unknown manage operation %q", op)
	}
}

// SkillsSources implements toolsurface.SkillsBackend.
func (l *Library) SkillsSources(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	tenantID, err := scopeTenant(ctx)
	if err != nil {
		return nil, err
	}
	args, err := decodeArgs(raw)
	if err != nil {
		return nil, err
	}
	switch op {
	case "list":
		sources, err := l.Sources(tenantID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sources": sources}, nil
	case "subscribe":
		id := args.string("id", "source", "sourceId")
		source := Source{
			ID:    id,
			Kind:  args.string("kind"),
			URL:   args.string("url", "path"),
			Trust: args.string("trust"),
		}
		if source.Kind == "" {
			source.Kind = SourceLocal
		}
		stored, err := l.Subscribe(tenantID, source)
		if err != nil {
			return nil, err
		}
		return stored, nil
	case "unsubscribe":
		removed, err := l.Unsubscribe(tenantID, args.string("id", "source", "sourceId"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"removed": removed}, nil
	case "pin":
		name := args.string("name")
		if name == "" {
			return nil, errorf(CodeInvalid, "pin needs the skill name")
		}
		if args.bool("unpin") {
			return l.Unpin(tenantID, name)
		}
		return l.Pin(tenantID, name, args.string("hash"))
	default:
		return nil, errorf(CodeInvalid, "unknown sources operation %q", op)
	}
}

func (l *Library) manageCreate(tenantID uuid.UUID, args args) (any, error) {
	name := args.string("name")
	if name == "" {
		return nil, errorf(CodeInvalid, "create needs a name")
	}
	description := args.string("description")
	if description == "" {
		return nil, errorf(CodeInvalid, "create needs a description")
	}
	body := args.string("body", "content")
	files, err := args.files()
	if err != nil {
		return nil, err
	}
	document, err := skills.ComposeSkillDocument(name, description, body, args.metadata())
	if err != nil {
		return nil, bundleError(err)
	}
	if !hasSkillDocument(files) {
		files = append(files, skills.FileInput{Path: skills.SkillFileName, Content: document})
	}
	record, err := l.Install(tenantID, InstallRequest{
		Name:           name,
		Files:          files,
		SourceID:       args.string("source", "sourceId"),
		Trust:          TrustLocal,
		AllowUntrusted: args.bool("allowUntrusted"),
		Pin:            args.bool("pin"),
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (l *Library) managePatch(tenantID uuid.UUID, args args) (any, error) {
	name := args.string("name")
	if name == "" {
		return nil, errorf(CodeInvalid, "patch needs a name")
	}
	current, err := l.Bundle(tenantID, name)
	if err != nil {
		return nil, err
	}
	description := args.string("description")
	if description == "" {
		description = current.Skill.Description
	}
	body := args.string("body", "content")
	if body == "" {
		body = current.Skill.Body
	}
	document, err := skills.ComposeSkillDocument(name, description, body, current.Skill.Metadata)
	if err != nil {
		return nil, bundleError(err)
	}
	files := replaceSkillDocument(current.Template(), document)
	return l.installEdited(tenantID, name, files)
}

func (l *Library) managePut(tenantID uuid.UUID, args args) (any, error) {
	name := args.string("name")
	if name == "" {
		return nil, errorf(CodeInvalid, "put needs a name")
	}
	files, err := args.files()
	if err != nil {
		return nil, err
	}
	if !hasSkillDocument(files) {
		return nil, errorf(CodeInvalid, "put needs the complete bundle, including %s", skills.SkillFileName)
	}
	return l.installEdited(tenantID, name, files)
}

func (l *Library) manageDelete(tenantID uuid.UUID, args args) (any, error) {
	name := args.string("name")
	if name == "" {
		return nil, errorf(CodeInvalid, "delete needs a name")
	}
	removed, err := l.Remove(tenantID, name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "removed": removed}, nil
}

func (l *Library) manageWriteFile(tenantID uuid.UUID, args args) (any, error) {
	name := args.string("name")
	path := args.string("path")
	if name == "" || path == "" {
		return nil, errorf(CodeInvalid, "write_file needs name and path")
	}
	content, ok, err := args.contentBytes()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errorf(CodeInvalid, "write_file needs content (or contentBase64)")
	}
	current, err := l.Bundle(tenantID, name)
	if err != nil {
		return nil, err
	}
	files := current.Template()
	replaced := false
	for i := range files {
		if files[i].Path == strings.TrimPrefix(path, "./") {
			files[i].Content = content
			files[i].Mode = args.mode(files[i].Mode)
			replaced = true
			break
		}
	}
	if !replaced {
		files = append(files, skills.FileInput{Path: path, Content: content, Mode: args.mode(0)})
	}
	return l.installEdited(tenantID, name, files)
}

func (l *Library) manageRemoveFile(tenantID uuid.UUID, args args) (any, error) {
	name := args.string("name")
	path := strings.TrimPrefix(args.string("path"), "./")
	if name == "" || path == "" {
		return nil, errorf(CodeInvalid, "remove_file needs name and path")
	}
	if path == skills.SkillFileName {
		return nil, errorf(CodeInvalid, "remove_file cannot remove %s (delete the skill instead)", skills.SkillFileName)
	}
	current, err := l.Bundle(tenantID, name)
	if err != nil {
		return nil, err
	}
	files := make([]skills.FileInput, 0, len(current.Files))
	found := false
	for _, file := range current.Template() {
		if file.Path == path {
			found = true
			continue
		}
		files = append(files, file)
	}
	if !found {
		return nil, errorf(CodeNotFound, "skill %q has no file %q", name, path)
	}
	return l.installEdited(tenantID, name, files)
}

// installEdited writes an agent edit as a new version. It preserves the
// skill's source and trust (an edit cannot upgrade an untrusted subscription)
// and returns the record plus whether anything actually changed.
func (l *Library) installEdited(tenantID uuid.UUID, name string, files []skills.FileInput) (any, error) {
	current, err := l.Bundle(tenantID, name)
	if err != nil {
		return nil, err
	}
	bundle, err := skills.BundleFromFilesNamed(files, name)
	if err != nil {
		return nil, bundleError(err)
	}
	if bundle.Hash == current.Hash {
		record, err := l.recordFor(tenantID, name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "up_to_date", "skill": record}, nil
	}
	detail, err := l.Show(tenantID, name)
	if err != nil {
		return nil, err
	}
	record, err := l.Install(tenantID, InstallRequest{
		Name:           name,
		Files:          files,
		SourceID:       detail.SourceID,
		Trust:          detail.Trust,
		AllowUntrusted: true,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "updated", "skill": record}, nil
}

func (l *Library) recordFor(tenantID uuid.UUID, name string) (Record, error) {
	snapshot, err := l.List(tenantID, false)
	if err != nil {
		return Record{}, err
	}
	for _, record := range snapshot.Skills {
		if record.Name == name {
			return record, nil
		}
	}
	return Record{}, errorf(CodeNotFound, "no skill named %q", name)
}

// --- argument parsing ----------------------------------------------------

type args map[string]json.RawMessage

func decodeArgs(raw json.RawMessage) (args, error) {
	if len(raw) == 0 {
		return args{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errorf(CodeInvalid, "arguments are not a JSON object: %v", err)
	}
	return args(fields), nil
}

// string reads the first non-empty string among the accepted spellings.
func (a args) string(keys ...string) string {
	for _, key := range keys {
		raw, ok := a[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (a args) bool(key string) bool {
	raw, ok := a[key]
	if !ok {
		return false
	}
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

func (a args) mode(fallback uint32) uint32 {
	raw, ok := a["mode"]
	if !ok {
		return fallback
	}
	var value uint32
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	return value
}

func (a args) metadata() map[string]string {
	raw, ok := a["metadata"]
	if !ok {
		return nil
	}
	var value map[string]string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func (a args) contentBytes() ([]byte, bool, error) {
	if encoded := a.string("contentBase64"); encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, false, errorf(CodeInvalid, "contentBase64 is not valid base64: %v", err)
		}
		return decoded, true, nil
	}
	raw, ok := a["content"]
	if !ok {
		return nil, false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, errorf(CodeInvalid, "content must be a string")
	}
	return []byte(value), true, nil
}

type fileArg struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	ContentBase64 string `json:"contentBase64"`
	Mode          uint32 `json:"mode"`
}

func (a args) files() ([]skills.FileInput, error) {
	raw, ok := a["files"]
	if !ok {
		return nil, nil
	}
	var list []fileArg
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errorf(CodeInvalid, "files must be an array of {path, content|contentBase64, mode?}: %v", err)
	}
	out := make([]skills.FileInput, 0, len(list))
	for _, file := range list {
		content := []byte(file.Content)
		if file.ContentBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(file.ContentBase64)
			if err != nil {
				return nil, errorf(CodeInvalid, "file %q contentBase64 is not valid base64: %v", file.Path, err)
			}
			content = decoded
		}
		out = append(out, skills.FileInput{Path: file.Path, Content: content, Mode: file.Mode})
	}
	return out, nil
}

func hasSkillDocument(files []skills.FileInput) bool {
	for _, file := range files {
		if strings.TrimPrefix(file.Path, "./") == skills.SkillFileName {
			return true
		}
	}
	return false
}

func replaceSkillDocument(files []skills.FileInput, document []byte) []skills.FileInput {
	out := make([]skills.FileInput, 0, len(files))
	replaced := false
	for _, file := range files {
		if strings.TrimPrefix(file.Path, "./") == skills.SkillFileName {
			if !replaced {
				out = append(out, skills.FileInput{Path: skills.SkillFileName, Content: document})
				replaced = true
			}
			continue
		}
		out = append(out, file)
	}
	if !replaced {
		out = append(out, skills.FileInput{Path: skills.SkillFileName, Content: document})
	}
	return out
}

func scopeTenant(ctx context.Context) (uuid.UUID, error) {
	scope, ok := agentcore.ToolRunScopeFromContext(ctx)
	if !ok {
		return uuid.Nil, errorf(CodeInvalid, "the skill library needs the run's tenant scope")
	}
	tenantID, err := uuid.Parse(scope.TenantID)
	if err != nil {
		return uuid.Nil, errorf(CodeInvalid, "the run scope carries no valid tenant: %v", err)
	}
	return tenantID, nil
}

// String renders an argument map for diagnostics without exposing file
// contents (which may be large or sensitive).
func (a args) String() string {
	keys := make([]string, 0, len(a))
	for key := range a {
		keys = append(keys, key)
	}
	return fmt.Sprintf("args%v", keys)
}
