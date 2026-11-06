package durable

import (
	"fmt"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
)

// Store is the persistence seam of the durable core. It deliberately exposes
// records, not SQL: tests use MemStore, and a host can implement the same
// interface on PostgreSQL (internal/store/tasks.go is the projection used by
// Keel workers).
type Store interface {
	CreateTask(rec TaskRecord) (TaskRecord, bool)
	UpdateTask(id TaskID, fn func(*TaskRecord)) (TaskRecord, error)
	GetTask(id TaskID) (TaskRecord, bool)
	ListTasks(conv ConversationID) []TaskRecord

	CreateSubmission(rec SubmissionRecord) SubmissionRecord
	UpdateSubmission(id SubmissionID, fn func(*SubmissionRecord)) (SubmissionRecord, error)
	GetSubmission(id SubmissionID) (SubmissionRecord, bool)
	FindSubmissionByRequest(conv ConversationID, requestID string) (SubmissionRecord, bool)
	ListSubmissions(conv ConversationID) []SubmissionRecord

	AppendEntry(conv ConversationID, rec EntryRecord) EntryRecord
	Entries(conv ConversationID) []EntryRecord

	GetDoc(conv ConversationID, kind, key string) ([]byte, bool)
	SetDoc(conv ConversationID, kind, key string, value []byte)
	DeleteDoc(conv ConversationID, kind, key string)
	ListDocs(conv ConversationID, kind string) map[string][]byte

	CreateConversation(id ConversationID)
	Conversations() []ConversationID
}

// MemStore is the in-memory Store used by the ported tests. It is safe for
// concurrent use and survives a simulated harness restart: a new Harness built
// over the same MemStore recovers exactly the committed records and documents.
type MemStore struct {
	mu            sync.Mutex
	tasks         map[TaskID]TaskRecord
	taskOrder     []TaskID
	submissions   map[SubmissionID]SubmissionRecord
	subOrder      []SubmissionID
	requests      map[string]SubmissionID // conv+"\x00"+requestID
	entries       map[ConversationID][]EntryRecord
	nextEntry     map[ConversationID]EntryID
	docs          map[string][]byte // conv+"\x00"+kind+"\x00"+key
	conversations map[ConversationID]bool
	convOrder     []ConversationID
	nextTask      int64
	nextSub       int64
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		tasks:         map[TaskID]TaskRecord{},
		submissions:   map[SubmissionID]SubmissionRecord{},
		requests:      map[string]SubmissionID{},
		entries:       map[ConversationID][]EntryRecord{},
		nextEntry:     map[ConversationID]EntryID{},
		docs:          map[string][]byte{},
		conversations: map[ConversationID]bool{},
	}
}

func docKey(conv ConversationID, kind, key string) string {
	return string(conv) + "\x00" + kind + "\x00" + key
}

func requestKey(conv ConversationID, requestID string) string {
	return string(conv) + "\x00" + requestID
}

// CreateTask stores a task, idempotently by id.
func (s *MemStore) CreateTask(rec TaskRecord) (TaskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.tasks[rec.ID]; ok {
		return cloneTask(existing), false
	}
	if rec.ID == "" {
		s.nextTask++
		rec.ID = TaskID(fmt.Sprintf("task-%d", s.nextTask))
	}
	if rec.Version == 0 {
		rec.Version = 1
	}
	s.tasks[rec.ID] = cloneTask(rec)
	s.taskOrder = append(s.taskOrder, rec.ID)
	return cloneTask(rec), true
}

// UpdateTask mutates a task under the store lock.
func (s *MemStore) UpdateTask(id TaskID, fn func(*TaskRecord)) (TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[id]
	if !ok {
		return TaskRecord{}, fmt.Errorf("durable: task %s does not exist", id)
	}
	cp := cloneTask(rec)
	fn(&cp)
	s.tasks[id] = cp
	return cloneTask(cp), nil
}

// GetTask reads a task.
func (s *MemStore) GetTask(id TaskID) (TaskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[id]
	return cloneTask(rec), ok
}

// ListTasks returns a conversation's tasks in creation order.
func (s *MemStore) ListTasks(conv ConversationID) []TaskRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TaskRecord
	for _, id := range s.taskOrder {
		if rec := s.tasks[id]; rec.ConversationID == conv || conv == "" {
			out = append(out, cloneTask(rec))
		}
	}
	return out
}

// CreateSubmission stores a submission. A non-empty request id deduplicates
// per conversation; the existing record is returned on replay.
func (s *MemStore) CreateSubmission(rec SubmissionRecord) SubmissionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.RequestID != "" {
		if id, ok := s.requests[requestKey(rec.ConversationID, rec.RequestID)]; ok {
			return cloneSubmission(s.submissions[id])
		}
	}
	if rec.ID == "" {
		s.nextSub++
		rec.ID = SubmissionID(fmt.Sprintf("submission-%d", s.nextSub))
	}
	s.submissions[rec.ID] = cloneSubmission(rec)
	s.subOrder = append(s.subOrder, rec.ID)
	if rec.RequestID != "" {
		s.requests[requestKey(rec.ConversationID, rec.RequestID)] = rec.ID
	}
	return cloneSubmission(rec)
}

// UpdateSubmission mutates a submission under the store lock.
func (s *MemStore) UpdateSubmission(id SubmissionID, fn func(*SubmissionRecord)) (SubmissionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.submissions[id]
	if !ok {
		return SubmissionRecord{}, ErrSubmissionNotFound{ID: id}
	}
	cp := cloneSubmission(rec)
	fn(&cp)
	s.submissions[id] = cp
	return cloneSubmission(cp), nil
}

// GetSubmission reads a submission.
func (s *MemStore) GetSubmission(id SubmissionID) (SubmissionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.submissions[id]
	return cloneSubmission(rec), ok
}

// FindSubmissionByRequest reads a submission by request id within a conversation.
func (s *MemStore) FindSubmissionByRequest(conv ConversationID, requestID string) (SubmissionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.requests[requestKey(conv, requestID)]
	if !ok {
		return SubmissionRecord{}, false
	}
	return cloneSubmission(s.submissions[id]), true
}

// ListSubmissions returns a conversation's submissions in creation order.
func (s *MemStore) ListSubmissions(conv ConversationID) []SubmissionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SubmissionRecord
	for _, id := range s.subOrder {
		if rec := s.submissions[id]; rec.ConversationID == conv || conv == "" {
			out = append(out, cloneSubmission(rec))
		}
	}
	return out
}

// AppendEntry assigns a per-conversation sequence and stores an entry.
func (s *MemStore) AppendEntry(conv ConversationID, rec EntryRecord) EntryRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextEntry[conv]++
	rec.ID = s.nextEntry[conv]
	rec.ConversationID = conv
	s.entries[conv] = append(s.entries[conv], cloneEntry(rec))
	return cloneEntry(rec)
}

// Entries returns a conversation's entries in order.
func (s *MemStore) Entries(conv ConversationID) []EntryRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.entries[conv]
	out := make([]EntryRecord, len(src))
	for i, e := range src {
		out[i] = cloneEntry(e)
	}
	return out
}

// GetDoc reads a document value.
func (s *MemStore) GetDoc(conv ConversationID, kind, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.docs[docKey(conv, kind, key)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// SetDoc writes a document value.
func (s *MemStore) SetDoc(conv ConversationID, kind, key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[docKey(conv, kind, key)] = append([]byte(nil), value...)
}

// DeleteDoc removes a document value.
func (s *MemStore) DeleteDoc(conv ConversationID, kind, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, docKey(conv, kind, key))
}

// ListDocs returns all documents of one kind for a conversation.
func (s *MemStore) ListDocs(conv ConversationID, kind string) map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := string(conv) + "\x00" + kind + "\x00"
	out := map[string][]byte{}
	for k, v := range s.docs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out[k[len(prefix):]] = append([]byte(nil), v...)
		}
	}
	return out
}

// CreateConversation registers a conversation id.
func (s *MemStore) CreateConversation(id ConversationID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.conversations[id] {
		s.conversations[id] = true
		s.convOrder = append(s.convOrder, id)
	}
}

// Conversations returns registered conversations in creation order.
func (s *MemStore) Conversations() []ConversationID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ConversationID(nil), s.convOrder...)
}

func cloneTask(t TaskRecord) TaskRecord {
	t.Input = cloneRaw(t.Input)
	t.State.Checkpoint = cloneRaw(t.State.Checkpoint)
	t.State.On = append([]TaskID(nil), t.State.On...)
	if t.State.Outcome != nil {
		o := *t.State.Outcome
		o.Result = cloneRaw(o.Result)
		if o.Error != nil {
			e := *o.Error
			e.Detail = cloneRaw(e.Detail)
			o.Error = &e
		}
		t.State.Outcome = &o
	}
	return t
}

func cloneSubmission(s SubmissionRecord) SubmissionRecord {
	s.Content = cloneRaw(s.Content)
	if s.Entry != nil {
		e := *s.Entry
		e.Data = cloneRaw(e.Data)
		s.Entry = &e
	}
	if s.Write != nil {
		w := *s.Write
		w.Data = cloneRaw(w.Data)
		w.Model = cloneMessages(s.Write.Model)
		w.Edits = append([]ContextEdit(nil), s.Write.Edits...)
		s.Write = &w
	}
	return s
}

func cloneEntry(e EntryRecord) EntryRecord {
	e.Model = cloneMessages(e.Model)
	e.Data = cloneRaw(e.Data)
	e.Edits = append([]ContextEdit(nil), e.Edits...)
	return e
}

func cloneMessages(ms []agentcore.Message) []agentcore.Message {
	return append([]agentcore.Message(nil), ms...)
}

// marshalDoc encodes a document value.
func marshalDoc(v any) []byte { return rawJSON(v) }
