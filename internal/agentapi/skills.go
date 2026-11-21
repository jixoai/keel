package agentapi

// The skill settings surface (change keel-harness-v2, W7 skills).
//
// Settings /settings/skills and `keel skills` are two faces of the same
// registry: GET/POST /v1/settings/skills* is the only write path, the CLI is a
// thin HTTP client and the agent's `skills manage` action reaches the same
// library in-process. Nothing here touches the database: the library is a
// tenant-scoped, content-addressed directory tree (internal/skilllib), and the
// tenant comes from the session middleware like every other settings route.
//
// Wire discipline:
//   - A record carries hashes, trust and conflict codes, never a file system
//     path outside the managed root and never a script's execution state.
//   - An install body may carry a bundle (SKILL.md + resources). The body is
//     bounded by the bundle limit, and a body that names a tenant is refused.
//   - A pinned skill whose source moved is a 409 with `code:"pinned"`: the
//     update is visible, and it is not silently applied.
//   - An untrusted source needs `allowUntrusted:true`; the refusal is a 403 and
//     the record keeps `trust:"untrusted"` forever.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/skills"
)

// skillBodyLimit bounds one install/update request: the bundle limit plus the
// JSON envelope (a base64 resource grows by 4/3, so the extra mebibyte is not
// spare).
const skillBodyLimit = skills.MaxBundleBytes*2 + 1<<20

// SkillLibrary is the tenant-scoped skill registry the routes drive. It is the
// internal/skilllib API verbatim, so *skilllib.Library satisfies it without an
// adapter; a test can substitute an in-memory double.
type SkillLibrary interface {
	List(tenantID uuid.UUID, check bool) (skilllib.Snapshot, error)
	Show(tenantID uuid.UUID, name string) (skilllib.Detail, error)
	Install(tenantID uuid.UUID, req skilllib.InstallRequest) (skilllib.Record, error)
	Update(tenantID uuid.UUID, name string, req skilllib.UpdateRequest) (skilllib.UpdateResult, error)
	Pin(tenantID uuid.UUID, name, hash string) (skilllib.Record, error)
	Unpin(tenantID uuid.UUID, name string) (skilllib.Record, error)
	Remove(tenantID uuid.UUID, name string) (bool, error)
	Sources(tenantID uuid.UUID) ([]skilllib.Source, error)
	Subscribe(tenantID uuid.UUID, source skilllib.Source) (skilllib.Source, error)
	Unsubscribe(tenantID uuid.UUID, id string) (bool, error)
}

func (s *Server) skillsWired() bool { return s.cfg.Skills != nil }

// skillsRoutes registers the settings face. The literal /sources paths are
// registered before the {name} patterns; Go's ServeMux prefers a literal
// segment, so /v1/settings/skills/sources never resolves as a skill named
// "sources".
func (s *Server) skillsRoutes() {
	s.mux.HandleFunc("GET /v1/settings/skills", s.listSkills)
	s.mux.HandleFunc("POST /v1/settings/skills", s.installSkill)
	s.mux.HandleFunc("GET /v1/settings/skills/sources", s.listSkillSources)
	s.mux.HandleFunc("POST /v1/settings/skills/sources", s.subscribeSkillSource)
	s.mux.HandleFunc("DELETE /v1/settings/skills/sources/{id}", s.unsubscribeSkillSource)
	s.mux.HandleFunc("GET /v1/settings/skills/{name}", s.showSkill)
	s.mux.HandleFunc("DELETE /v1/settings/skills/{name}", s.removeSkill)
	s.mux.HandleFunc("POST /v1/settings/skills/{name}/update", s.updateSkill)
	s.mux.HandleFunc("POST /v1/settings/skills/{name}/pin", s.pinSkill)
	s.mux.HandleFunc("POST /v1/settings/skills/{name}/unpin", s.unpinSkill)
}

// skillsResponse is the GET /v1/settings/skills body. checked says whether the
// sources were read (the update/conflict fields are only meaningful when it is
// true).
type skillsResponse struct {
	Revision int64              `json:"revision"`
	Root     string             `json:"root"`
	Checked  bool               `json:"checked"`
	Skills   []skilllib.Record  `json:"skills"`
	Sources  []skilllib.Source  `json:"sources"`
	Limits   map[string]int     `json:"limits"`
	Policy   skillsPolicyOnWire `json:"policy"`
}

type skillsPolicyOnWire struct {
	// TrustValues are the trust labels a record may carry.
	TrustValues []string `json:"trustValues"`
	// SourceKinds are the subscription kinds this build can sync.
	SourceKinds []string `json:"sourceKinds"`
	// Bundle limits are what an install may carry.
	MaxFiles     int `json:"maxFiles"`
	MaxBytes     int `json:"maxBytes"`
	MaxFileBytes int `json:"maxFileBytes"`
	// ScriptExecution states where skill scripts run (the bash tool), so a UI
	// never offers "run" here.
	ScriptExecution string `json:"scriptExecution"`
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	check := true
	if raw := strings.TrimSpace(r.URL.Query().Get("check")); raw != "" {
		check = raw != "0" && !strings.EqualFold(raw, "false")
	}
	snapshot, err := s.cfg.Skills.List(tenantID, check)
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	if snapshot.Skills == nil {
		snapshot.Skills = []skilllib.Record{}
	}
	if snapshot.Sources == nil {
		snapshot.Sources = []skilllib.Source{}
	}
	writeJSON(w, http.StatusOK, skillsResponse{
		Revision: snapshot.Revision,
		Root:     snapshot.Root,
		Checked:  check,
		Skills:   snapshot.Skills,
		Sources:  snapshot.Sources,
		Limits: map[string]int{
			"maxFiles":     skills.MaxBundleFiles,
			"maxBytes":     skills.MaxBundleBytes,
			"maxFileBytes": skills.MaxBundleFileBytes,
		},
		Policy: skillsPolicyOnWire{
			TrustValues:     []string{skilllib.TrustTrusted, skilllib.TrustUntrusted, skilllib.TrustLocal},
			SourceKinds:     []string{skilllib.SourceLocal},
			MaxFiles:        skills.MaxBundleFiles,
			MaxBytes:        skills.MaxBundleBytes,
			MaxFileBytes:    skills.MaxBundleFileBytes,
			ScriptExecution: "bash",
		},
	})
}

// skillsInstallRequest is the POST body. Description+Body is the convenience
// form (`/learn` and the CLI use it); Files is the full-bundle form. Exactly
// one is required.
type skillsInstallRequest struct {
	Name           string             `json:"name"`
	Description    string             `json:"description"`
	Body           string             `json:"body"`
	Files          []skillsFileOnWire `json:"files"`
	SourceID       string             `json:"sourceId"`
	Trust          string             `json:"trust"`
	AllowUntrusted bool               `json:"allowUntrusted"`
	Pin            bool               `json:"pin"`
}

type skillsFileOnWire struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	ContentBase64 string `json:"contentBase64"`
	Mode          uint32 `json:"mode"`
}

func (s *Server) installSkill(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	var body skillsInstallRequest
	if err := decodeSkillBody(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	files, err := body.files()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.cfg.Skills.Install(tenantID, skilllib.InstallRequest{
		Name:           strings.TrimSpace(body.Name),
		Description:    body.Description,
		Body:           body.Body,
		Files:          files,
		SourceID:       strings.TrimSpace(body.SourceID),
		Trust:          strings.TrimSpace(body.Trust),
		AllowUntrusted: body.AllowUntrusted,
		Pin:            body.Pin,
	})
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

// skillsUpdateRequest is the POST /{name}/update body. All fields are optional:
// an empty body reads the recorded source.
type skillsUpdateRequest struct {
	Files          []skillsFileOnWire `json:"files"`
	Description    string             `json:"description"`
	Body           string             `json:"body"`
	SourceID       string             `json:"sourceId"`
	AllowUntrusted bool               `json:"allowUntrusted"`
}

func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	name := r.PathValue("name")
	var body skillsUpdateRequest
	if err := decodeSkillBody(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	files, err := body.files()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.cfg.Skills.Update(tenantID, name, skilllib.UpdateRequest{
		Files:          files,
		Description:    body.Description,
		Body:           body.Body,
		SourceID:       strings.TrimSpace(body.SourceID),
		AllowUntrusted: body.AllowUntrusted,
	})
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) showSkill(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	detail, err := s.cfg.Skills.Show(tenantID, r.PathValue("name"))
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) removeSkill(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	removed, err := s.cfg.Skills.Remove(tenantID, r.PathValue("name"))
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) pinSkill(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	var body struct {
		Hash string `json:"hash"`
	}
	if err := decodeSkillBody(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.cfg.Skills.Pin(tenantID, r.PathValue("name"), strings.TrimSpace(body.Hash))
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) unpinSkill(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	record, err := s.cfg.Skills.Unpin(tenantID, r.PathValue("name"))
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// --- sources -------------------------------------------------------------

type skillSourcesResponse struct {
	Sources []skilllib.Source `json:"sources"`
	Limits  map[string]int    `json:"limits"`
}

func (s *Server) listSkillSources(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	sources, err := s.cfg.Skills.Sources(tenantID)
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	if sources == nil {
		sources = []skilllib.Source{}
	}
	writeJSON(w, http.StatusOK, skillSourcesResponse{
		Sources: sources,
		Limits:  map[string]int{"maxSources": 32},
	})
}

func (s *Server) subscribeSkillSource(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		URL   string `json:"url"`
		Trust string `json:"trust"`
	}
	if err := decodeSkillBody(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if kind == "" {
		kind = skilllib.SourceLocal
	}
	source, err := s.cfg.Skills.Subscribe(tenantID, skilllib.Source{
		ID:    strings.TrimSpace(body.ID),
		Kind:  kind,
		URL:   strings.TrimSpace(body.URL),
		Trust: strings.TrimSpace(body.Trust),
	})
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, source)
}

func (s *Server) unsubscribeSkillSource(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if !s.skillsWired() {
		notWired(w, "the skill library is not wired in this deployment")
		return
	}
	removed, err := s.cfg.Skills.Unsubscribe(tenantID, r.PathValue("id"))
	if err != nil {
		s.writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// --- wire helpers --------------------------------------------------------

func (r skillsInstallRequest) files() ([]skills.FileInput, error) {
	return decodeSkillFiles(r.Files)
}

func (r skillsUpdateRequest) files() ([]skills.FileInput, error) {
	return decodeSkillFiles(r.Files)
}

func decodeSkillFiles(inputs []skillsFileOnWire) ([]skills.FileInput, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]skills.FileInput, 0, len(inputs))
	for _, input := range inputs {
		content := []byte(input.Content)
		if input.ContentBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(input.ContentBase64)
			if err != nil {
				return nil, fmt.Errorf("file %q contentBase64 is not valid base64: %w", input.Path, err)
			}
			content = decoded
		}
		out = append(out, skills.FileInput{Path: input.Path, Content: content, Mode: input.Mode})
	}
	return out, nil
}

// decodeSkillBody reads a JSON body bounded by the bundle limit. An empty body
// is valid for update/pin (read the source/current version); the caller
// decides whether the operation needs more.
func decodeSkillBody(w http.ResponseWriter, r *http.Request, dst any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, skillBodyLimit))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return fmt.Errorf("the body must be a JSON object: %w", err)
	}
	if _, ok := fields["tenantId"]; ok {
		return errors.New("the body must not carry a tenantId: the tenant comes from the session")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// writeSkillError maps the library's typed failures onto the wire. The code is
// stable and carried next to the message so the settings page, the CLI and the
// agent all render the same fact.
func (s *Server) writeSkillError(w http.ResponseWriter, err error) {
	var typed *skilllib.Error
	if !errors.As(err, &typed) {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	status := http.StatusBadRequest
	switch typed.Code {
	case skilllib.CodeNotFound, skilllib.CodeVersionMissing:
		status = http.StatusNotFound
	case skilllib.CodeAlreadyExists, skilllib.CodeNameConflict, skilllib.CodeSourceInUse,
		skilllib.CodePinned, skilllib.CodeBundleMismatch:
		status = http.StatusConflict
	case skilllib.CodeUntrusted:
		status = http.StatusForbidden
	case skilllib.CodeUnsupported:
		status = http.StatusNotImplemented
	case skilllib.CodeSourceMissing:
		status = http.StatusUnprocessableEntity
	case skilllib.CodeLibraryNotWired:
		status = http.StatusNotImplemented
	}
	writeJSON(w, status, map[string]any{
		"error": typed.Message,
		"code":  typed.Code,
	})
}
