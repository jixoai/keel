package oar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// JournalOptions tunes the durable record journal.
type JournalOptions struct {
	// NoSync disables fsync after every append. The default (false) is the
	// durable setting: a record that the journal accepted survives a host
	// crash. Tests may disable it to measure throughput; production should not.
	NoSync bool
	// MaxRecordBytes overrides MaxRecordBytes for journal appends. Zero uses
	// the contract limit.
	MaxRecordBytes int
}

// Journal is an append-only NDJSON record log, one file per backend session.
// It is the durable, developer-observable half of the record stream:
//
//	session-<id>.ndjson
//	{"schema":"keel.oar.record/1","seq":1,...}
//	{"schema":"keel.oar.record/1","seq":2,...}
//
// Ordering rules: seq is strictly increasing per session; duplicate delivery of
// a byte-identical record is idempotent; a regressed or reused seq with
// different content is a typed ordering error. Gaps are allowed (a crashed
// runtime can lose in-flight records) and are visible to the CLI.
type Journal struct {
	dir  string
	opts JournalOptions

	mu     sync.Mutex
	files  map[string]*journalFile
	closed bool
}

type journalFile struct {
	path    string
	file    *os.File
	lastSeq int64
	lastRaw []byte
	count   int64
}

// NewJournal opens (lazily) a journal directory. The directory is created with
// 0o755 when missing.
func NewJournal(dir string, opts JournalOptions) (*Journal, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("oar: journal directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("oar: create journal directory: %w", err)
	}
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = MaxRecordBytes
	}
	return &Journal{dir: dir, opts: opts, files: make(map[string]*journalFile)}, nil
}

// Dir returns the journal directory.
func (j *Journal) Dir() string { return j.dir }

// Path is the journal file of one backend session.
func (j *Journal) Path(sessionID string) string {
	return filepath.Join(j.dir, journalFileName(sessionID))
}

// JournalPath is the journal file of one backend session inside dir. It is the
// read-side companion of Journal.Path for a caller that only locates a journal
// (the product projection route) and must not open or create one: the same file
// name sanitization applies, so a backend reference can never address a path
// outside dir.
func JournalPath(dir, sessionID string) string {
	return filepath.Join(dir, journalFileName(sessionID))
}

func journalFileName(sessionID string) string {
	var b strings.Builder
	safe := true
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			safe = false
			b.WriteByte('_')
		}
	}
	name := b.String()
	name = strings.Trim(name, "._-")
	if name == "" {
		name = "session"
	}
	if !safe || len(name) > 80 {
		sum := sha256.Sum256([]byte(sessionID))
		suffix := hex.EncodeToString(sum[:4])
		if len(name) > 72 {
			name = name[:72]
		}
		name = name + "-" + suffix
	}
	return "session-" + name + ".ndjson"
}

// Append validates and writes one record. It returns appended=false when the
// exact same record (same seq, same bytes) was already the last record of the
// session: replay is idempotent. Any other seq regression is an *Error with
// CodeRecordOrder.
func (j *Journal) Append(record Record) (bool, error) {
	if err := record.Validate(); err != nil {
		return false, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return false, &Error{Code: CodeInvalidRecord, Runtime: record.Runtime, Detail: "record cannot be encoded", Err: err}
	}
	if len(raw) > j.opts.MaxRecordBytes {
		return false, &Error{
			Code:    CodeRecordTooLarge,
			Runtime: record.Runtime,
			Detail:  fmt.Sprintf("record is %d bytes, limit %d", len(raw), j.opts.MaxRecordBytes),
		}
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return false, errors.New("oar: journal is closed")
	}
	file, err := j.fileLocked(record.SessionID)
	if err != nil {
		return false, err
	}
	if record.Seq <= file.lastSeq {
		if record.Seq == file.lastSeq && EqualRecordBytes(raw, file.lastRaw) {
			return false, nil
		}
		return false, &Error{
			Code:    CodeRecordOrder,
			Runtime: record.Runtime,
			Detail: fmt.Sprintf("session %s seq %d is not greater than the last seq %d",
				record.SessionID, record.Seq, file.lastSeq),
		}
	}
	line := make([]byte, 0, len(raw)+1)
	line = append(line, raw...)
	line = append(line, '\n')
	if _, err := file.file.Write(line); err != nil {
		return false, fmt.Errorf("oar: append journal %s: %w", file.path, err)
	}
	if !j.opts.NoSync {
		if err := file.file.Sync(); err != nil {
			return false, fmt.Errorf("oar: fsync journal %s: %w", file.path, err)
		}
	}
	file.lastSeq = record.Seq
	file.lastRaw = append(file.lastRaw[:0], raw...)
	file.count++
	return true, nil
}

// LastSeq returns the last persisted seq of a session (0 when none). It opens
// the session file when this process has not written it yet, so a restarted
// host continues at the right sequence.
func (j *Journal) LastSeq(sessionID string) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return 0, errors.New("oar: journal is closed")
	}
	file, err := j.fileLocked(sessionID)
	if err != nil {
		return 0, err
	}
	return file.lastSeq, nil
}

// Close flushes and closes every open session file. It is idempotent.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	var first error
	for _, file := range j.files {
		if !j.opts.NoSync {
			if err := file.file.Sync(); err != nil && first == nil {
				first = err
			}
		}
		if err := file.file.Close(); err != nil && first == nil {
			first = err
		}
	}
	j.files = nil
	return first
}

// fileLocked returns (opening if needed) the session file. The caller holds
// j.mu.
func (j *Journal) fileLocked(sessionID string) (*journalFile, error) {
	if file := j.files[sessionID]; file != nil {
		return file, nil
	}
	path := j.Path(sessionID)
	file := &journalFile{path: path}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		last, lastRaw, count, err := scanJournalBytes(existing)
		if err != nil {
			return nil, fmt.Errorf("oar: journal %s: %w", path, err)
		}
		file.lastSeq = last.Seq
		file.lastRaw = lastRaw
		file.count = count
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("oar: read journal %s: %w", path, err)
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("oar: open journal %s: %w", path, err)
	}
	file.file = handle
	j.files[sessionID] = file
	return file, nil
}

// scanJournalBytes parses every non-empty line and returns the last record, its
// raw bytes plus the record count. A malformed line is an error: a corroded
// journal must be visible instead of silently truncated.
func scanJournalBytes(raw []byte) (Record, []byte, int64, error) {
	var (
		last    Record
		lastRaw []byte
		count   int64
	)
	lines := strings.Split(string(raw), "\n")
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return Record{}, nil, 0, fmt.Errorf("line %d is not a record: %w", index+1, err)
		}
		if err := record.Validate(); err != nil {
			return Record{}, nil, 0, fmt.Errorf("line %d is invalid: %w", index+1, err)
		}
		if record.Seq <= last.Seq {
			return Record{}, nil, 0, fmt.Errorf("line %d: seq %d is not greater than %d", index+1, record.Seq, last.Seq)
		}
		last = record
		lastRaw = []byte(line)
		count++
	}
	return last, lastRaw, count, nil
}

// ReadJournal reads one journal file into records, preserving file order.
func ReadJournal(path string) ([]Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("oar: read journal %s: %w", path, err)
	}
	return ParseJournal(raw)
}

// ParseJournal parses NDJSON journal bytes. It is used by the CLI and by tests
// that embed a journal fixture.
func ParseJournal(raw []byte) ([]Record, error) {
	lines := strings.Split(string(raw), "\n")
	records := make([]Record, 0, len(lines))
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, fmt.Errorf("oar: journal line %d: %w", index+1, err)
		}
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("oar: journal line %d: %w", index+1, err)
		}
		records = append(records, record)
	}
	return records, nil
}

// CopyJournal streams one journal file to w. It is the CLI's file path without
// loading the whole journal into memory.
func CopyJournal(w io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("oar: open journal %s: %w", path, err)
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}
