package fileplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
)

// LeaseReader reads the live fencing token of a session. *kernel.Kernel
// implements it; a test can supply a fixed token.
type LeaseReader interface {
	LeaseState(ctx context.Context, scope kernel.Scope) (kernel.LeaseState, error)
}

// Options wires the file service.
type Options struct {
	// Store is the layerfs byte layer. Required.
	Store *layerfs.Store
	// Manifest is the session-scoped path projection. Nil uses an in-memory
	// manifest (tests; a deployment without the migration).
	Manifest Manifest
	// Committer is the kernel write seam. Nil keeps reads working and makes
	// writes answer unavailable.
	Committer kernel.Committer
	// Leases supplies the live owner token every fs.write/capture commit needs.
	Leases LeaseReader
	// Surface is the materialized execution space. Nil disables write-through.
	Surface Surface
	// CaptureRoot is the workspace directory Capture walks. Empty disables
	// capture.
	CaptureRoot string
	// Ignore is the capture ignore list. Nil uses layerfs.DefaultIgnore.
	Ignore []string
	// Cache is the capture stat cache. Nil creates one.
	Cache *layerfs.StatCache
	// MaxReadLines / MaxReadBytes bound one read page (0 uses the defaults).
	MaxReadLines int
	MaxReadBytes int
	Logger       *slog.Logger
	Now          func() time.Time
}

// Service is the file plugin's implementation. It is safe for concurrent use;
// the manifest and layerfs store provide their own synchronization.
type Service struct {
	store       *layerfs.Store
	manifest    Manifest
	committer   kernel.Committer
	leases      LeaseReader
	surface     Surface
	captureRoot string
	ignore      []string
	cache       *layerfs.StatCache
	maxLines    int
	maxBytes    int
	logger      *slog.Logger
	now         func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("fileplugin: a layerfs store is required")
	}
	manifest := opts.Manifest
	if manifest == nil {
		manifest = NewMemoryManifest()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	cache := opts.Cache
	if cache == nil {
		cache = layerfs.NewStatCache()
	}
	return &Service{
		store:       opts.Store,
		manifest:    manifest,
		committer:   opts.Committer,
		leases:      opts.Leases,
		surface:     opts.Surface,
		captureRoot: strings.TrimSpace(opts.CaptureRoot),
		ignore:      opts.Ignore,
		cache:       cache,
		maxLines:    opts.MaxReadLines,
		maxBytes:    opts.MaxReadBytes,
		logger:      logger,
		now:         now,
	}, nil
}

// Manifest exposes the projection (panel/tests).
func (s *Service) Manifest() Manifest { return s.manifest }

// Read implements the read tool.
func (s *Service) Read(ctx context.Context, args json.RawMessage) (ReadResult, error) {
	var input struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return ReadResult{}, Errorf(CodeInvalid, "read arguments: %v", err)
	}
	scope, err := scopeFrom(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	if input.Offset < 0 {
		input.Offset = 0
	}
	path, err := NormalizePath(input.Path)
	if err != nil {
		return ReadResult{}, err
	}
	entry, ok, err := s.manifest.Get(ctx, scope.TenantID, scope.SessionID, path)
	if err != nil {
		return ReadResult{}, mapStoreError(err)
	}
	if !ok {
		return ReadResult{}, Errorf(CodeNotFound, "file %q does not exist in this session", path)
	}
	object, err := s.store.Open(entry.Hash)
	if err != nil {
		return ReadResult{}, mapLayerfsError(err)
	}
	defer object.Close()
	endsWithNewline := true
	if object.Size > 0 {
		if _, err := object.Seek(object.Size-1, io.SeekStart); err == nil {
			var last [1]byte
			if _, err := io.ReadFull(object, last[:]); err == nil {
				endsWithNewline = last[0] == '\n'
			}
		}
		if _, err := object.Seek(0, io.SeekStart); err != nil {
			return ReadResult{}, mapLayerfsError(err)
		}
	}
	limit := input.Limit
	if limit <= 0 {
		limit = s.maxLines
	}
	if limit <= 0 {
		limit = DefaultReadLines
	}
	if limit > MaxReadLines {
		limit = MaxReadLines
	}
	page, err := readLinePage(object, input.Offset, limit, s.maxBytes)
	if err != nil {
		return ReadResult{}, mapLayerfsError(err)
	}
	// The page builder terminates every returned line; the file's final line
	// keeps its exact form (a missing trailing newline stays missing).
	if !endsWithNewline && page.ReturnedLines > 0 && page.NextOffset == page.TotalLines {
		page.Content = strings.TrimSuffix(page.Content, "\n")
	}
	return ReadResult{
		Path:          path,
		Hash:          entry.Hash.Prefixed(),
		Size:          entry.Size,
		Mode:          entry.Mode,
		Seq:           entry.UpdatedSeq,
		Offset:        input.Offset,
		Limit:         limit,
		TotalLines:    page.TotalLines,
		ReturnedLines: page.ReturnedLines,
		NextOffset:    page.NextOffset,
		Truncated:     page.Truncated,
		Content:       page.Content,
	}, nil
}

// WriteOptions carries the write preconditions.
type WriteOptions struct {
	// ExpectedHash is the content hash the model read. Required for an existing
	// file; must be empty for a create.
	ExpectedHash string
	// Mode is the new permission bits (0 keeps the current/default mode).
	Mode uint32
	// TaskID is the attribution fact (never an isolation boundary).
	TaskID string
}

// Write implements the write tool.
func (s *Service) Write(ctx context.Context, args json.RawMessage) (WriteResult, error) {
	var input struct {
		Path         string `json:"path"`
		Content      string `json:"content"`
		ExpectedHash string `json:"expectedHash"`
		Mode         uint32 `json:"mode"`
		TaskID       string `json:"taskId"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return WriteResult{}, Errorf(CodeInvalid, "write arguments: %v", err)
	}
	scope, err := scopeFrom(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	callID, ok := CallIDFromContext(ctx)
	if !ok {
		return WriteResult{}, Errorf(CodeInvalid, "write needs the ambient tool-call id")
	}
	return s.WriteStream(ctx, scope, callID, input.Path, strings.NewReader(input.Content), WriteOptions{
		ExpectedHash: input.ExpectedHash,
		Mode:         input.Mode,
		TaskID:       input.TaskID,
	})
}

// WriteStream is the one write path. The content is streamed into layerfs; the
// fs.write event commits before the manifest projection changes, so the ledger
// is always the truth.
func (s *Service) WriteStream(ctx context.Context, scope Scope, callID, rawPath string, r io.Reader, opts WriteOptions) (WriteResult, error) {
	if !scope.Valid() {
		return WriteResult{}, Errorf(CodeRunScopeMissing, "the file plugin needs the ambient session scope")
	}
	if strings.TrimSpace(callID) == "" {
		return WriteResult{}, Errorf(CodeInvalid, "a write needs its tool-call id")
	}
	path, err := NormalizePath(rawPath)
	if err != nil {
		return WriteResult{}, err
	}
	taskID := s.taskID(scope, opts.TaskID)
	if replay, found, err := s.lookupWrite(ctx, scope, callID, path); err != nil {
		return WriteResult{}, err
	} else if found {
		return replay, nil
	}
	current, exists, err := s.manifest.Get(ctx, scope.TenantID, scope.SessionID, path)
	if err != nil {
		return WriteResult{}, mapStoreError(err)
	}
	expected, err := parseExpectedHash(opts.ExpectedHash)
	if err != nil {
		return WriteResult{}, err
	}
	switch {
	case exists && expected == "":
		return WriteResult{}, Errorf(CodeConflict,
			"file %q already exists (hash %s); read it first and pass expectedHash to overwrite", path, current.Hash.Prefixed())
	case exists && expected != current.Hash:
		return WriteResult{}, Errorf(CodeConflict,
			"file %q changed: expected %s, current %s; read it again", path, expected.Prefixed(), current.Hash.Prefixed())
	case !exists && expected != "":
		return WriteResult{}, Errorf(CodeConflict, "file %q does not exist; omit expectedHash to create it", path)
	}
	mode := opts.Mode & 0o777
	if mode == 0 {
		if exists {
			mode = current.Mode
		} else {
			mode = 0o644
		}
	}
	result, err := s.store.PutStatus(r)
	if err != nil {
		return WriteResult{}, mapLayerfsError(err)
	}
	receipt, err := s.commitWrite(ctx, scope, callID, path, result.Hash, result.Size, mode, taskID)
	if err != nil {
		return WriteResult{}, err
	}
	entry := Entry{Path: path, Hash: result.Hash, Mode: mode, Size: result.Size, UpdatedSeq: receipt.Seq, LastTaskID: taskID}
	if err := s.manifest.Upsert(ctx, scope.TenantID, scope.SessionID, []Entry{entry}); err != nil {
		return WriteResult{}, Errorf(CodeIO, "write committed at seq %d but the manifest update failed: %v", receipt.Seq, err)
	}
	if _, err := s.store.Retain(result.Hash, layerfs.Ref{Session: scope.SessionID.String(), Seq: receipt.Seq}); err != nil {
		return WriteResult{}, Errorf(CodeIO, "write committed at seq %d but the reference was not recorded: %v", receipt.Seq, err)
	}
	if err := s.writeThrough(path, result.Hash, mode); err != nil {
		return WriteResult{}, Errorf(CodeIO, "write committed at seq %d but write-through failed: %v", receipt.Seq, err)
	}
	return WriteResult{
		Path:    path,
		Hash:    result.Hash.Prefixed(),
		Size:    result.Size,
		Mode:    mode,
		Seq:     receipt.Seq,
		Created: !exists,
	}, nil
}

// Edit implements the edit tool. The match is exact and must be unique unless
// replaceAll is set; the precondition is the same content hash write uses.
func (s *Service) Edit(ctx context.Context, args json.RawMessage) (EditResult, error) {
	var input struct {
		Path         string `json:"path"`
		OldText      string `json:"oldText"`
		NewText      string `json:"newText"`
		ExpectedHash string `json:"expectedHash"`
		ReplaceAll   bool   `json:"replaceAll"`
		TaskID       string `json:"taskId"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return EditResult{}, Errorf(CodeInvalid, "edit arguments: %v", err)
	}
	if input.OldText == "" {
		return EditResult{}, Errorf(CodeInvalid, "edit requires a non-empty oldText")
	}
	scope, err := scopeFrom(ctx)
	if err != nil {
		return EditResult{}, err
	}
	callID, ok := CallIDFromContext(ctx)
	if !ok {
		return EditResult{}, Errorf(CodeInvalid, "edit needs the ambient tool-call id")
	}
	path, err := NormalizePath(input.Path)
	if err != nil {
		return EditResult{}, err
	}
	if replay, found, err := s.lookupEdit(ctx, scope, callID, path); err != nil {
		return EditResult{}, err
	} else if found {
		return replay, nil
	}
	current, exists, err := s.manifest.Get(ctx, scope.TenantID, scope.SessionID, path)
	if err != nil {
		return EditResult{}, mapStoreError(err)
	}
	if !exists {
		return EditResult{}, Errorf(CodeNotFound, "file %q does not exist in this session", path)
	}
	expected, err := parseExpectedHash(input.ExpectedHash)
	if err != nil {
		return EditResult{}, err
	}
	if expected == "" {
		return EditResult{}, Errorf(CodeConflict,
			"edit %q needs expectedHash (read the file first); current hash is %s", path, current.Hash.Prefixed())
	}
	if expected != current.Hash {
		return EditResult{}, Errorf(CodeConflict,
			"file %q changed: expected %s, current %s; read it again", path, expected.Prefixed(), current.Hash.Prefixed())
	}
	needle, replacement := []byte(input.OldText), []byte(input.NewText)
	max := 2
	if input.ReplaceAll {
		max = maxReplacements
	}
	source, err := s.store.Open(current.Hash)
	if err != nil {
		return EditResult{}, mapLayerfsError(err)
	}
	offsets, err := findOccurrences(source, needle, max)
	source.Close()
	if err != nil {
		return EditResult{}, err
	}
	if len(offsets) == 0 {
		return EditResult{}, Errorf(CodeConflict, "oldText does not occur in %q", path)
	}
	if !input.ReplaceAll && len(offsets) > 1 {
		return EditResult{}, Errorf(CodeConflict, "oldText occurs %d times in %q; pass replaceAll or a longer unique snippet", len(offsets), path)
	}
	if input.ReplaceAll && len(offsets) >= maxReplacements {
		return EditResult{}, Errorf(CodeConflict, "oldText occurs at least %d times in %q; edit a smaller file or use write", maxReplacements, path)
	}
	source, err = s.store.Open(current.Hash)
	if err != nil {
		return EditResult{}, mapLayerfsError(err)
	}
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(replaceStream(source, writer, needle, replacement, offsets))
		source.Close()
	}()
	result, err := s.store.PutStatus(reader)
	reader.Close()
	if err != nil {
		return EditResult{}, mapLayerfsError(err)
	}
	taskID := s.taskID(scope, input.TaskID)
	receipt, err := s.commitWrite(ctx, scope, callID, path, result.Hash, result.Size, current.Mode, taskID)
	if err != nil {
		return EditResult{}, err
	}
	entry := Entry{Path: path, Hash: result.Hash, Mode: current.Mode, Size: result.Size, UpdatedSeq: receipt.Seq, LastTaskID: taskID}
	if err := s.manifest.Upsert(ctx, scope.TenantID, scope.SessionID, []Entry{entry}); err != nil {
		return EditResult{}, Errorf(CodeIO, "edit committed at seq %d but the manifest update failed: %v", receipt.Seq, err)
	}
	if _, err := s.store.Retain(result.Hash, layerfs.Ref{Session: scope.SessionID.String(), Seq: receipt.Seq}); err != nil {
		return EditResult{}, Errorf(CodeIO, "edit committed at seq %d but the reference was not recorded: %v", receipt.Seq, err)
	}
	if err := s.writeThrough(path, result.Hash, current.Mode); err != nil {
		return EditResult{}, Errorf(CodeIO, "edit committed at seq %d but write-through failed: %v", receipt.Seq, err)
	}
	return EditResult{
		Path:         path,
		Hash:         result.Hash.Prefixed(),
		Size:         result.Size,
		Mode:         current.Mode,
		Seq:          receipt.Seq,
		Replacements: len(offsets),
	}, nil
}

// Capture implements the capture tool (internal; not model-facing).
func (s *Service) Capture(ctx context.Context, args json.RawMessage) (CaptureResult, error) {
	var input struct {
		BaseSeq    int64  `json:"baseSeq"`
		ToolCallID string `json:"toolCallId"`
		TaskID     string `json:"taskId"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &input); err != nil {
			return CaptureResult{}, Errorf(CodeInvalid, "capture arguments: %v", err)
		}
	}
	scope, err := scopeFrom(ctx)
	if err != nil {
		return CaptureResult{}, err
	}
	callID := input.ToolCallID
	if callID == "" {
		callID, _ = CallIDFromContext(ctx)
	}
	if callID == "" {
		callID = "capture"
	}
	return s.CaptureWorkspace(ctx, scope, callID, input.BaseSeq, input.TaskID)
}

// CaptureWorkspace snapshots the execution space and commits exactly one
// fs.capture event (or one fs.capture_failed on the failure path).
func (s *Service) CaptureWorkspace(ctx context.Context, scope Scope, callID string, baseSeq int64, taskID string) (CaptureResult, error) {
	if !scope.Valid() {
		return CaptureResult{}, Errorf(CodeRunScopeMissing, "capture needs the ambient session scope")
	}
	if s.captureRoot == "" {
		return CaptureResult{}, Errorf(CodeUnavailable, "no execution space is attached for capture")
	}
	if s.committer == nil {
		return CaptureResult{}, Errorf(CodeUnavailable, "no kernel committer is wired")
	}
	taskID = s.taskID(scope, taskID)
	key := callID + ":" + strconv.FormatInt(baseSeq, 10)
	if replay, found, err := s.lookupCapture(ctx, scope, key, baseSeq); err != nil {
		return CaptureResult{}, err
	} else if found {
		return replay, nil
	}
	snapshot, err := s.store.Capture(s.captureRoot, layerfs.CaptureOptions{Ignore: s.ignore, Cache: s.cache})
	if err != nil {
		s.commitCaptureFailed(ctx, scope, callID, baseSeq, taskID, err)
		return CaptureResult{}, mapLayerfsError(err)
	}
	token, err := s.liveToken(ctx, scope)
	if err != nil {
		s.commitCaptureFailed(ctx, scope, callID, baseSeq, taskID, err)
		return CaptureResult{}, err
	}
	payload := map[string]any{
		"sessionId":     scope.SessionID.String(),
		"baseSeq":       baseSeq,
		"count":         snapshot.Count,
		"treeHash":      snapshot.Tree.Prefixed(),
		"newObjects":    snapshot.NewObjects,
		"reusedObjects": snapshot.ReusedObjects,
		"bytes":         snapshot.Bytes,
	}
	if taskID != "" {
		payload["taskId"] = taskID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return CaptureResult{}, Errorf(CodeIO, "encode capture payload: %v", err)
	}
	receipt, err := s.committer.Commit(ctx, kernel.CommitRequest{
		Scope:        kernelScope(scope),
		Kind:         kernel.KindFSCapture,
		Token:        token,
		FSCaptureKey: key,
		Payload:      raw,
	})
	if err != nil {
		s.commitCaptureFailed(ctx, scope, callID, baseSeq, taskID, err)
		return CaptureResult{}, mapKernelError(err)
	}
	entries := make([]Entry, len(snapshot.Files))
	refs := make([]layerfs.RefEntry, 0, len(snapshot.Files)+1)
	ref := layerfs.Ref{Session: scope.SessionID.String(), Seq: receipt.Seq}
	for index, file := range snapshot.Files {
		entries[index] = Entry{
			Path: file.Path, Hash: file.Hash, Mode: file.Mode, Size: file.Size,
			UpdatedSeq: receipt.Seq, LastTaskID: taskID,
		}
		refs = append(refs, layerfs.RefEntry{Hash: file.Hash, Ref: ref})
	}
	refs = append(refs, layerfs.RefEntry{Hash: snapshot.Tree, Ref: ref})
	if err := s.manifest.Upsert(ctx, scope.TenantID, scope.SessionID, entries); err != nil {
		return CaptureResult{}, Errorf(CodeIO, "capture committed at seq %d but the manifest update failed: %v", receipt.Seq, err)
	}
	if _, err := s.store.RetainBatch(refs); err != nil {
		return CaptureResult{}, Errorf(CodeIO, "capture committed at seq %d but references were not recorded: %v", receipt.Seq, err)
	}
	if err := s.manifest.PutSnapshot(ctx, scope.TenantID, scope.SessionID, receipt.Seq, snapshot.Tree); err != nil {
		return CaptureResult{}, Errorf(CodeIO, "capture committed at seq %d but the snapshot was not recorded: %v", receipt.Seq, err)
	}
	return CaptureResult{
		Tree: snapshot.Tree.Prefixed(), Count: snapshot.Count, Seq: receipt.Seq, BaseSeq: baseSeq,
		NewObjects: snapshot.NewObjects, ReusedObjects: snapshot.ReusedObjects, Bytes: snapshot.Bytes,
	}, nil
}

// Materialize writes the session's current manifest view under dir.
func (s *Service) Materialize(ctx context.Context, scope Scope, dir string) error {
	if !scope.Valid() {
		return Errorf(CodeRunScopeMissing, "materialize needs a session scope")
	}
	if strings.TrimSpace(dir) == "" {
		return Errorf(CodeInvalid, "materialize needs a target directory")
	}
	entries, err := s.manifest.List(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return mapStoreError(err)
	}
	for _, entry := range entries {
		if err := s.materializeEntry(entry, dir); err != nil {
			return err
		}
	}
	return nil
}

// MaterializeSnapshot materializes the tree recorded at an event sequence.
func (s *Service) MaterializeSnapshot(ctx context.Context, scope Scope, seq int64, dir string) error {
	if !scope.Valid() {
		return Errorf(CodeRunScopeMissing, "materialize needs a session scope")
	}
	if strings.TrimSpace(dir) == "" {
		return Errorf(CodeInvalid, "materialize needs a target directory")
	}
	tree, ok, err := s.manifest.Snapshot(ctx, scope.TenantID, scope.SessionID, seq)
	if err != nil {
		return mapStoreError(err)
	}
	if !ok {
		return Errorf(CodeNotFound, "no session snapshot at seq %d", seq)
	}
	if err := s.store.Materialize(tree, dir); err != nil {
		return mapLayerfsError(err)
	}
	return nil
}

// List returns the current manifest view.
func (s *Service) List(ctx context.Context, scope Scope) ([]Entry, error) {
	if !scope.Valid() {
		return nil, Errorf(CodeRunScopeMissing, "list needs a session scope")
	}
	entries, err := s.manifest.List(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return entries, nil
}

// --- internals -------------------------------------------------------------

func scopeFrom(ctx context.Context) (Scope, error) {
	scope, ok := ScopeFromContext(ctx)
	if !ok {
		return Scope{}, Errorf(CodeRunScopeMissing, "the file plugin needs the ambient session scope")
	}
	return scope, nil
}

func kernelScope(scope Scope) kernel.Scope {
	return kernel.Scope{TenantID: scope.TenantID, SessionID: scope.SessionID}
}

func (s *Service) taskID(scope Scope, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return scope.TaskID
}

func parseExpectedHash(raw string) (layerfs.Hash, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	hash, err := layerfs.ParseHash(raw)
	if err != nil {
		return "", Errorf(CodeInvalid, "expectedHash: %v", err)
	}
	return hash, nil
}

func (s *Service) liveToken(ctx context.Context, scope Scope) (kernel.OwnerToken, error) {
	if s.leases == nil {
		return kernel.OwnerToken{}, Errorf(CodeUnavailable, "no lease seam is wired; fs.write needs the run fence")
	}
	state, err := s.leases.LeaseState(ctx, kernelScope(scope))
	if err != nil {
		if errors.Is(err, kernel.ErrNotFound) {
			return kernel.OwnerToken{}, Errorf(CodeNotFound, "session %s does not exist", scope.SessionID)
		}
		return kernel.OwnerToken{}, Errorf(CodeUnavailable, "read the run lease: %v", err)
	}
	if !state.Found || !state.Live {
		return kernel.OwnerToken{}, Errorf(CodeUnavailable, "the run lease for session %s is not live; fs.write cannot be fenced", scope.SessionID)
	}
	return state.Lease.Token, nil
}

func (s *Service) commitWrite(ctx context.Context, scope Scope, callID, path string, hash layerfs.Hash, size int64, mode uint32, taskID string) (kernel.Receipt, error) {
	if s.committer == nil {
		return kernel.Receipt{}, Errorf(CodeUnavailable, "no kernel committer is wired")
	}
	token, err := s.liveToken(ctx, scope)
	if err != nil {
		return kernel.Receipt{}, err
	}
	payload := map[string]any{
		"sessionId": scope.SessionID.String(),
		"path":      path,
		"hash":      hash.Prefixed(),
		"mode":      mode,
		"size":      size,
	}
	if taskID != "" {
		payload["taskId"] = taskID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return kernel.Receipt{}, Errorf(CodeIO, "encode fs.write payload: %v", err)
	}
	receipt, err := s.committer.Commit(ctx, kernel.CommitRequest{
		Scope:      kernelScope(scope),
		Kind:       kernel.KindFSWrite,
		Token:      token,
		FSWriteKey: callID + ":" + path,
		Payload:    raw,
	})
	if err != nil {
		return kernel.Receipt{}, mapKernelError(err)
	}
	return receipt, nil
}

func (s *Service) lookupWrite(ctx context.Context, scope Scope, callID, path string) (WriteResult, bool, error) {
	if s.committer == nil {
		return WriteResult{}, false, nil
	}
	receipt, found, err := s.committer.Lookup(ctx, kernelScope(scope), kernel.KeyRef{Kind: kernel.KeyFSWrite, Value: callID + ":" + path})
	if err != nil {
		return WriteResult{}, false, mapKernelError(err)
	}
	if !found {
		return WriteResult{}, false, nil
	}
	entry, ok, err := s.manifest.Get(ctx, scope.TenantID, scope.SessionID, path)
	if err != nil {
		return WriteResult{}, false, mapStoreError(err)
	}
	if !ok {
		return WriteResult{}, false, Errorf(CodeUnavailable, "the write event is committed at seq %d but the manifest projection is missing; retry after a rebuild", receipt.Seq)
	}
	// A retry must finish the side effects the first attempt may have missed
	// after the commit (reference + write-through); both are idempotent.
	if _, err := s.store.Retain(entry.Hash, layerfs.Ref{Session: scope.SessionID.String(), Seq: receipt.Seq}); err != nil {
		return WriteResult{}, false, Errorf(CodeIO, "write replay at seq %d could not record its reference: %v", receipt.Seq, err)
	}
	if err := s.writeThrough(path, entry.Hash, entry.Mode); err != nil {
		return WriteResult{}, false, Errorf(CodeIO, "write replay at seq %d could not write through: %v", receipt.Seq, err)
	}
	return WriteResult{
		Path: path, Hash: entry.Hash.Prefixed(), Size: entry.Size, Mode: entry.Mode, Seq: receipt.Seq,
		Created: false, Replayed: true,
	}, true, nil
}

func (s *Service) lookupEdit(ctx context.Context, scope Scope, callID, path string) (EditResult, bool, error) {
	if s.committer == nil {
		return EditResult{}, false, nil
	}
	receipt, found, err := s.committer.Lookup(ctx, kernelScope(scope), kernel.KeyRef{Kind: kernel.KeyFSWrite, Value: callID + ":" + path})
	if err != nil {
		return EditResult{}, false, mapKernelError(err)
	}
	if !found {
		return EditResult{}, false, nil
	}
	entry, ok, err := s.manifest.Get(ctx, scope.TenantID, scope.SessionID, path)
	if err != nil {
		return EditResult{}, false, mapStoreError(err)
	}
	if !ok {
		return EditResult{}, false, Errorf(CodeUnavailable, "the edit event is committed at seq %d but the manifest projection is missing; retry after a rebuild", receipt.Seq)
	}
	if _, err := s.store.Retain(entry.Hash, layerfs.Ref{Session: scope.SessionID.String(), Seq: receipt.Seq}); err != nil {
		return EditResult{}, false, Errorf(CodeIO, "edit replay at seq %d could not record its reference: %v", receipt.Seq, err)
	}
	if err := s.writeThrough(path, entry.Hash, entry.Mode); err != nil {
		return EditResult{}, false, Errorf(CodeIO, "edit replay at seq %d could not write through: %v", receipt.Seq, err)
	}
	return EditResult{Path: path, Hash: entry.Hash.Prefixed(), Size: entry.Size, Mode: entry.Mode, Seq: receipt.Seq, Replayed: true}, true, nil
}

func (s *Service) lookupCapture(ctx context.Context, scope Scope, key string, baseSeq int64) (CaptureResult, bool, error) {
	if s.committer == nil {
		return CaptureResult{}, false, nil
	}
	receipt, found, err := s.committer.Lookup(ctx, kernelScope(scope), kernel.KeyRef{Kind: kernel.KeyFSCapture, Value: key})
	if err != nil {
		return CaptureResult{}, false, mapKernelError(err)
	}
	if !found {
		return CaptureResult{}, false, nil
	}
	entries, err := s.manifest.List(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return CaptureResult{}, false, mapStoreError(err)
	}
	tree, _, err := s.manifest.Snapshot(ctx, scope.TenantID, scope.SessionID, receipt.Seq)
	if err != nil {
		return CaptureResult{}, false, mapStoreError(err)
	}
	return CaptureResult{Tree: tree.Prefixed(), Count: len(entries), Seq: receipt.Seq, BaseSeq: baseSeq, Replayed: true}, true, nil
}

func (s *Service) commitCaptureFailed(ctx context.Context, scope Scope, callID string, baseSeq int64, taskID string, cause error) {
	if s.committer == nil {
		return
	}
	payload := map[string]any{
		"sessionId":  scope.SessionID.String(),
		"toolCallId": callID,
		"baseSeq":    baseSeq,
		"error":      cause.Error(),
	}
	if taskID != "" {
		payload["taskId"] = taskID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.logger.Warn("encode fs.capture_failed", "err", err)
		return
	}
	if _, err := s.committer.Commit(ctx, kernel.CommitRequest{
		Scope:              kernelScope(scope),
		Kind:               kernel.KindFSCaptureFailed,
		System:             true,
		FSCaptureFailedKey: fmt.Sprintf("%s:%s:%d", scope.SessionID, callID, baseSeq),
		Payload:            raw,
	}); err != nil {
		s.logger.Warn("commit fs.capture_failed", "session_id", scope.SessionID, "err", err)
	}
}

func (s *Service) writeThrough(path string, hash layerfs.Hash, mode uint32) error {
	if s.surface == nil {
		return nil
	}
	object, err := s.store.Open(hash)
	if err != nil {
		return err
	}
	defer object.Close()
	return s.surface.WriteFile(path, mode, object)
}

func (s *Service) materializeEntry(entry Entry, dir string) error {
	path, err := NormalizePath(entry.Path)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.FromSlash(path))
	rootAbs, err := filepath.Abs(dir)
	if err != nil {
		return mapLayerfsError(err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return mapLayerfsError(err)
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Errorf(CodeInvalid, "manifest path %q escapes the materialize target", entry.Path)
	}
	if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
		return Errorf(CodeIO, "create materialize parent: %v", err)
	}
	object, err := s.store.Open(entry.Hash)
	if err != nil {
		return mapLayerfsError(err)
	}
	defer object.Close()
	file, err := os.OpenFile(targetAbs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Errorf(CodeIO, "create materialized file: %v", err)
	}
	if _, err := io.Copy(file, object); err != nil {
		file.Close()
		return Errorf(CodeIO, "write materialized file: %v", err)
	}
	if err := file.Close(); err != nil {
		return Errorf(CodeIO, "close materialized file: %v", err)
	}
	mode := os.FileMode(entry.Mode & 0o777)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(targetAbs, mode); err != nil {
		return Errorf(CodeIO, "chmod materialized file: %v", err)
	}
	return nil
}

func mapLayerfsError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, layerfs.ErrQuotaExceeded):
		return &Error{Code: CodeQuotaExceeded, Message: err.Error(), Err: err}
	case errors.Is(err, layerfs.ErrNotFound):
		return &Error{Code: CodeNotFound, Message: err.Error(), Err: err}
	case errors.Is(err, layerfs.ErrInvalid):
		return &Error{Code: CodeInvalid, Message: err.Error(), Err: err}
	case errors.Is(err, layerfs.ErrUnavailable):
		return &Error{Code: CodeUnavailable, Message: err.Error(), Err: err}
	default:
		return &Error{Code: CodeIO, Message: err.Error(), Err: err}
	}
}

func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: CodeIO, Message: err.Error(), Err: err}
}

func mapKernelError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, kernel.ErrNotFound):
		return &Error{Code: CodeNotFound, Message: err.Error(), Err: err}
	case errors.Is(err, kernel.ErrQuota):
		return &Error{Code: CodeQuotaExceeded, Message: err.Error(), Err: err}
	case errors.Is(err, kernel.ErrInvalidRequest), errors.Is(err, kernel.ErrKeyKindMismatch):
		return &Error{Code: CodeInvalid, Message: err.Error(), Err: err}
	case errors.Is(err, kernel.ErrMissingToken), errors.Is(err, kernel.ErrStaleEpoch), errors.Is(err, kernel.ErrLeaseLost):
		return &Error{Code: CodeUnavailable, Message: err.Error(), Err: err}
	default:
		return &Error{Code: CodeIO, Message: err.Error(), Err: err}
	}
}
