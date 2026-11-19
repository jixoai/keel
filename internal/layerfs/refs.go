package layerfs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Ref is the ledger reference that retains one object. The pair is the
// idempotency identity: replaying the same event (same session, same seq) never
// increments the count twice, and a second session writing identical bytes adds
// a second reference instead of a second object.
type Ref struct {
	Session string
	Seq     int64
}

func (r Ref) valid() bool {
	return strings.TrimSpace(r.Session) != "" &&
		!strings.ContainsAny(r.Session, " \t\r\n") && r.Seq >= 0
}

func (r Ref) key() string { return r.Session + "\x00" + strconv.FormatInt(r.Seq, 10) }

// RefEntry is one (object, reference) pair for a batch retain (a capture
// retains every file and tree object with the capture event's sequence).
type RefEntry struct {
	Hash Hash
	Ref  Ref
}

// refIndex is the in-memory reference index plus its append-only journal. The
// journal makes the index survive a process restart; a torn trailing line is
// ignored (the journal is derived state, the event log remains the truth).
type refIndex struct {
	mu      sync.Mutex
	path    string
	journal *os.File
	byHash  map[Hash]map[string]Ref
}

func loadRefIndex(path string) (*refIndex, error) {
	index := &refIndex{
		path:   path,
		byHash: make(map[Hash]map[string]Ref),
	}
	if raw, err := os.ReadFile(path); err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(raw)))
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) != 4 {
				// A torn final line from a crash is not a reason to refuse boot.
				continue
			}
			op, hashValue, session, seqValue := fields[0], fields[1], fields[2], fields[3]
			hash, err := ParseHash(hashValue)
			if err != nil {
				continue
			}
			seq, err := strconv.ParseInt(seqValue, 10, 64)
			if err != nil {
				continue
			}
			ref := Ref{Session: session, Seq: seq}
			switch op {
			case "+":
				index.addLocked(hash, ref)
			case "-":
				index.removeLocked(hash, ref)
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("%w: read refs journal: %v", ErrIO, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: open refs journal: %v", ErrIO, err)
	}
	journal, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("%w: open refs journal: %v", ErrIO, err)
	}
	index.journal = journal
	return index, nil
}

func (r *refIndex) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.journal == nil {
		return nil
	}
	err := r.journal.Close()
	r.journal = nil
	return err
}

func (r *refIndex) addLocked(hash Hash, ref Ref) bool {
	refs := r.byHash[hash]
	if refs == nil {
		refs = make(map[string]Ref)
		r.byHash[hash] = refs
	}
	key := ref.key()
	if _, exists := refs[key]; exists {
		return false
	}
	refs[key] = ref
	return true
}

func (r *refIndex) removeLocked(hash Hash, ref Ref) bool {
	refs := r.byHash[hash]
	if refs == nil {
		return false
	}
	key := ref.key()
	if _, exists := refs[key]; !exists {
		return false
	}
	delete(refs, key)
	if len(refs) == 0 {
		delete(r.byHash, hash)
	}
	return true
}

func (r *refIndex) appendLocked(lines string) error {
	if r.journal == nil {
		return fmt.Errorf("%w: refs journal is closed", ErrUnavailable)
	}
	if lines == "" {
		return nil
	}
	if _, err := r.journal.WriteString(lines); err != nil {
		return fmt.Errorf("%w: append refs journal: %v", ErrIO, err)
	}
	return nil
}

// retain records one reference. It reports whether the reference was new.
func (r *refIndex) retain(hash Hash, ref Ref) (bool, error) {
	if !ref.valid() {
		return false, fmt.Errorf("%w: reference (%q, %d) is malformed", ErrInvalid, ref.Session, ref.Seq)
	}
	if !hash.Valid() {
		return false, fmt.Errorf("%w: object hash %q", ErrInvalid, string(hash))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.addLocked(hash, ref) {
		return false, nil
	}
	if err := r.appendLocked("+ " + string(hash) + " " + ref.Session + " " + strconv.FormatInt(ref.Seq, 10) + "\n"); err != nil {
		r.removeLocked(hash, ref)
		return false, err
	}
	return true, nil
}

// release removes one reference. It reports whether the reference existed.
func (r *refIndex) release(hash Hash, ref Ref) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.removeLocked(hash, ref) {
		return false, nil
	}
	if err := r.appendLocked("- " + string(hash) + " " + ref.Session + " " + strconv.FormatInt(ref.Seq, 10) + "\n"); err != nil {
		// The in-memory view is the operational truth; a journal failure is
		// reported but the release still took effect for this process.
		return true, err
	}
	return true, nil
}

// retainBatch records many references with one journal flush per chunk. It
// returns how many references were new.
func (r *refIndex) retainBatch(entries []RefEntry) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range entries {
		if !entry.Hash.Valid() || !entry.Ref.valid() {
			return 0, fmt.Errorf("%w: malformed retain entry", ErrInvalid)
		}
	}
	added := 0
	var builder strings.Builder
	flush := func() error {
		if builder.Len() == 0 {
			return nil
		}
		err := r.appendLocked(builder.String())
		builder.Reset()
		return err
	}
	for _, entry := range entries {
		if !r.addLocked(entry.Hash, entry.Ref) {
			continue
		}
		added++
		builder.WriteString("+ ")
		builder.WriteString(string(entry.Hash))
		builder.WriteByte(' ')
		builder.WriteString(entry.Ref.Session)
		builder.WriteByte(' ')
		builder.WriteString(strconv.FormatInt(entry.Ref.Seq, 10))
		builder.WriteByte('\n')
		if builder.Len() >= 1<<20 {
			if err := flush(); err != nil {
				return added, err
			}
		}
	}
	return added, flush()
}

func (r *refIndex) count(hash Hash) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byHash[hash])
}

func (r *refIndex) refs(hash Hash) []Ref {
	r.mu.Lock()
	defer r.mu.Unlock()
	refs := make([]Ref, 0, len(r.byHash[hash]))
	for _, ref := range r.byHash[hash] {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Session != refs[j].Session {
			return refs[i].Session < refs[j].Session
		}
		return refs[i].Seq < refs[j].Seq
	})
	return refs
}

// releaseSession drops every reference a session owns (a deleted session). It
// is not the GC: an object shared with a fork keeps its other references.
func (r *refIndex) releaseSession(session string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var builder strings.Builder
	released := 0
	for hash, refs := range r.byHash {
		for _, ref := range refs {
			if ref.Session != session {
				continue
			}
			delete(refs, ref.key())
			released++
			builder.WriteString("- ")
			builder.WriteString(string(hash))
			builder.WriteByte(' ')
			builder.WriteString(ref.Session)
			builder.WriteByte(' ')
			builder.WriteString(strconv.FormatInt(ref.Seq, 10))
			builder.WriteByte('\n')
		}
		if len(refs) == 0 {
			delete(r.byHash, hash)
		}
	}
	if err := r.appendLocked(builder.String()); err != nil {
		return released, err
	}
	return released, nil
}

// compact rewrites the journal with only the live references, so releases do
// not grow the file without bound.
func (r *refIndex) compact() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(r.path), "refs-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: compact refs: %v", ErrIO, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	writer := bufio.NewWriter(tmp)
	for hash, refs := range r.byHash {
		for _, ref := range refs {
			if _, err := fmt.Fprintf(writer, "+ %s %s %d\n", hash, ref.Session, ref.Seq); err != nil {
				tmp.Close()
				return fmt.Errorf("%w: compact refs: %v", ErrIO, err)
			}
		}
	}
	if err := writer.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: compact refs: %v", ErrIO, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: compact refs: %v", ErrIO, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: compact refs: %v", ErrIO, err)
	}
	if r.journal != nil {
		_ = r.journal.Close()
		r.journal = nil
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		return fmt.Errorf("%w: compact refs: %v", ErrIO, err)
	}
	journal, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("%w: reopen refs journal: %v", ErrIO, err)
	}
	r.journal = journal
	return nil
}
