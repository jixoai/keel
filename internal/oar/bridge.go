package oar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/gateway"
)

// EventTypeRecord is the gateway event type that carries one OAR record. An OAR
// sidecar is an ordinary agent_backend; the record stream rides on
// event.stream notifications, so the existing external gateway (and
// externalgw's external.event projection) needs no new transport.
const EventTypeRecord = "oar.record"

// EventTypeStatus is an optional non-record status event a runtime may emit
// (for example a degraded adapter). The bridge reports it as a diagnostic and
// never turns it into a record.
const EventTypeStatus = "oar.status"

// SpawnOptions configures one OAR sidecar connection.
type SpawnOptions struct {
	// Command is the OAR sidecar executable (required).
	Command string
	// Args are appended to Command.
	Args []string
	// Dir is the sidecar's working directory.
	Dir string
	// Env is the exact child environment. Nil means gateway.MinimalEnv of the
	// host environment; a caller that needs the runtime's own variables appends
	// them explicitly (never the whole host environment).
	Env []string
	// Stderr receives the sidecar's stderr.
	Stderr io.Writer
	// Runtime is the expected adapter id ("codex", "fake"). Empty accepts any
	// declared runtime. A mismatch is rejected before a session opens.
	Runtime string
	// Required is the extra capability set the caller needs on top of
	// BaseCapabilities.
	Required []Capability
	// Journal is the durable record journal. Nil keeps records in memory only.
	Journal *Journal
	// RequestTimeout bounds one gateway request. Zero uses the gateway default.
	RequestTimeout time.Duration
	// RecordBuffer is the live record channel capacity. Zero uses 256; a
	// negative value disables the live channel (journal/OnRecord only).
	RecordBuffer int
	// OnRecord observes every accepted record. It runs on the bridge goroutine
	// and must not block.
	OnRecord func(Record)
	// OnDiagnostic receives non-fatal gateway diagnostics.
	OnDiagnostic func(gateway.Diagnostic)
}

// Session is one OAR session as the host knows it.
type Session struct {
	// Ref is the runtime's opaque session handle.
	Ref string `json:"ref"`
	// HostSessionID is the Keel session that owns the external call.
	HostSessionID string `json:"hostSessionId,omitempty"`
	// TaskID is the task card the external call is attributed to.
	TaskID string `json:"taskId,omitempty"`
	// Status mirrors the gateway external_status mark.
	Status string `json:"status"`
	// LastSeq is the highest record seq observed for the session.
	LastSeq int64 `json:"lastSeq"`
}

// OpenRequest opens an OAR session. Config is the runtime-specific JSON config
// (workdir, model, sandbox...). Attribution is added here, out of band; it is
// never written into the prompt.
type OpenRequest struct {
	Reference     string
	HostSessionID string
	TaskID        string
	Config        json.RawMessage
	Workdir       string
	Model         string
}

// Bridge is one live OAR sidecar connection: handshake, capability gate, record
// decoding, seq enforcement, journaling and session calls.
type Bridge struct {
	opts        SpawnOptions
	conn        *gateway.Conn
	client      *gateway.AgentClient
	transport   gateway.Transport
	handshake   gateway.Handshake
	declaration Declaration

	mu       sync.Mutex
	sessions map[string]*Session
	firstErr error
	closed   bool

	tracker   *SeqTracker
	records   chan Record
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	consumer  sync.WaitGroup
	dropped   int64
}

// Dial spawns the OAR sidecar, performs the gateway handshake, enforces the OAR
// capability contract and starts the record bridge. Any failure closes the
// spawned process group before returning.
func Dial(ctx context.Context, opts SpawnOptions) (*Bridge, error) {
	if opts.Command == "" {
		return nil, &Error{Code: CodeRuntimeUnavailable, Runtime: opts.Runtime, Detail: "OAR sidecar command is required"}
	}
	if opts.Env == nil {
		opts.Env = gateway.MinimalEnv(os.Environ())
	}
	if opts.RecordBuffer == 0 {
		opts.RecordBuffer = 256
	}
	transport, err := gateway.SpawnStdio(gateway.SpawnOptions{
		Command: opts.Command,
		Args:    opts.Args,
		Dir:     opts.Dir,
		Env:     opts.Env,
		Stderr:  opts.Stderr,
	})
	if err != nil {
		return nil, &Error{Code: CodeRuntimeUnavailable, Runtime: opts.Runtime, Detail: "cannot spawn the OAR sidecar", Err: err}
	}
	conn := gateway.NewConn(gateway.ConnOptions{
		ClientInfo:      gateway.Implementation{Name: "keel-oar", Version: "1.0.0"},
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeAgentBackend},
		RequestTimeout:  opts.RequestTimeout,
		OnDiagnostic:    opts.OnDiagnostic,
	})
	handshake, err := conn.Connect(ctx, transport)
	if err != nil {
		_ = transport.Close()
		return nil, &Error{Code: CodeProtocol, Runtime: opts.Runtime, Detail: "OAR sidecar handshake failed", Err: err}
	}
	raw, ok := handshake.Capabilities.Experimental[ExperimentalKey]
	if !ok {
		_ = conn.Close()
		return nil, &Error{
			Code:    CodeProtocol,
			Runtime: opts.Runtime,
			Detail:  fmt.Sprintf("initialize did not declare capabilities.experimental.%s", ExperimentalKey),
		}
	}
	declaration, err := ParseDeclaration(raw)
	if err == nil {
		err = declaration.Validate()
	}
	if err == nil {
		required := append(append([]Capability(nil), BaseCapabilities...), opts.Required...)
		err = declaration.Require(required)
	}
	if err == nil && opts.Runtime != "" && declaration.Runtime != opts.Runtime {
		err = &Error{
			Code:    CodeProtocol,
			Runtime: declaration.Runtime,
			Detail:  fmt.Sprintf("sidecar declared runtime %q, expected %q", declaration.Runtime, opts.Runtime),
		}
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	bridge := &Bridge{
		opts:        opts,
		conn:        conn,
		client:      gateway.NewAgentBackend(conn),
		transport:   transport,
		handshake:   handshake,
		declaration: declaration,
		sessions:    make(map[string]*Session),
		tracker:     NewSeqTracker(),
		done:        make(chan struct{}),
		errors:      make(chan error, 16),
	}
	if opts.RecordBuffer > 0 {
		bridge.records = make(chan Record, opts.RecordBuffer)
	}
	bridge.consumer.Add(1)
	go bridge.consume()
	return bridge, nil
}

// Declaration returns the negotiated runtime capability declaration.
func (b *Bridge) Declaration() Declaration { return b.declaration }

// Handshake returns the negotiated gateway handshake.
func (b *Bridge) Handshake() gateway.Handshake { return b.handshake }

// PID returns the sidecar process id (0 when the transport has no process).
func (b *Bridge) PID() int {
	if stdio, ok := b.transport.(*gateway.StdioTransport); ok {
		return stdio.PID()
	}
	return 0
}

// Records is the live record channel (nil when RecordBuffer is negative). It
// is a best-effort live view; the journal and OnRecord are authoritative when
// the consumer falls behind.
func (b *Bridge) Records() <-chan Record { return b.records }

// Errors reports record-contract violations (ordering, unknown session,
// malformed record) without tearing down a working connection. The channel is
// buffered and never closed; read it with a select timeout.
func (b *Bridge) Errors() <-chan error { return b.errors }

// Err returns the first record-contract violation, nil when none.
func (b *Bridge) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.firstErr
}

// DroppedRecords reports live records lost to a full Records() buffer. It is
// observability only; the journal still has them.
func (b *Bridge) DroppedRecords() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Open opens a session, persists the host attribution in the session-open
// config and subscribes to its record stream.
func (b *Bridge) Open(ctx context.Context, request OpenRequest) (Session, error) {
	config, err := openConfig(request)
	if err != nil {
		return Session{}, err
	}
	session, err := b.client.Open(ctx, gateway.SessionOpenRequest{
		Reference: request.Reference,
		Config:    config,
	})
	if err != nil {
		return Session{}, err
	}
	info := Session{
		Ref:           session.SessionID,
		HostSessionID: request.HostSessionID,
		TaskID:        request.TaskID,
		Status:        session.Status,
	}
	b.mu.Lock()
	b.sessions[session.SessionID] = &info
	b.mu.Unlock()
	if _, err := b.client.EventStream(ctx, gateway.EventStreamRequest{SessionID: session.SessionID}); err != nil {
		b.mu.Lock()
		delete(b.sessions, session.SessionID)
		b.mu.Unlock()
		_ = b.client.Close(context.WithoutCancel(ctx), gateway.CloseRequest{SessionID: session.SessionID, Reason: "subscribe failed"})
		return Session{}, err
	}
	return b.Session(session.SessionID), nil
}

// openConfig merges the caller's runtime config with the out-of-band
// attribution. The prompt is not part of this document.
func openConfig(request OpenRequest) (json.RawMessage, error) {
	config := map[string]any{}
	if len(request.Config) > 0 && string(request.Config) != "null" {
		if err := json.Unmarshal(request.Config, &config); err != nil {
			return nil, &Error{Code: CodeProtocol, Detail: "session config is not a JSON object", Err: err}
		}
	}
	config["attribution"] = Attribution{SessionID: request.HostSessionID, TaskID: request.TaskID}
	if request.Workdir != "" {
		config["workdir"] = request.Workdir
	}
	if request.Model != "" {
		config["model"] = request.Model
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, &Error{Code: CodeProtocol, Detail: "encode session config", Err: err}
	}
	return raw, nil
}

// Send hands one user message to the runtime. The message is the caller's
// exact prompt; OAR adds no task-card instruction.
func (b *Bridge) Send(ctx context.Context, sessionRef string, message json.RawMessage) (gateway.MessageSendResult, error) {
	if sessionRef == "" {
		return gateway.MessageSendResult{}, errors.New("oar: message.send requires a session")
	}
	return b.client.Send(ctx, gateway.MessageSendRequest{SessionID: sessionRef, Message: message})
}

// Cancel cancels the session's current turn.
func (b *Bridge) Cancel(ctx context.Context, sessionRef, turnID, reason string) error {
	if sessionRef == "" {
		return errors.New("oar: cancel requires a session")
	}
	return b.client.Cancel(ctx, gateway.CancelRequest{SessionID: sessionRef, TurnID: turnID, Reason: reason})
}

// Resume asks the runtime to resume a known session from the journal cursor.
func (b *Bridge) Resume(ctx context.Context, sessionRef string) (Session, error) {
	if sessionRef == "" {
		return Session{}, errors.New("oar: session.resume requires a session")
	}
	resumed, err := b.client.Resume(ctx, gateway.SessionResumeRequest{SessionID: sessionRef})
	if err != nil {
		return Session{}, err
	}
	if _, err := b.client.EventStream(ctx, gateway.EventStreamRequest{SessionID: sessionRef, FromSeq: resumed.LastSeq}); err != nil {
		return Session{}, err
	}
	b.mu.Lock()
	info := b.sessions[sessionRef]
	if info == nil {
		// A host may resume a session it knows from its own journal after this
		// process lost its in-memory map; the runtime is the authority on whether
		// the handle still works.
		info = &Session{Ref: sessionRef}
		b.sessions[sessionRef] = info
	}
	info.Status = resumed.Status
	b.mu.Unlock()
	return b.Session(sessionRef), nil
}

// CloseSession closes one OAR session.
func (b *Bridge) CloseSession(ctx context.Context, sessionRef, reason string) error {
	if sessionRef == "" {
		return errors.New("oar: close requires a session")
	}
	if err := b.client.Close(ctx, gateway.CloseRequest{SessionID: sessionRef, Reason: reason}); err != nil {
		return err
	}
	b.mu.Lock()
	if info := b.sessions[sessionRef]; info != nil {
		info.Status = gateway.ExternalStatusClosed
	}
	b.mu.Unlock()
	return nil
}

// Health probes the sidecar.
func (b *Bridge) Health(ctx context.Context) (gateway.HealthResult, error) {
	return b.client.Health(ctx)
}

// Session returns one session snapshot (zero when unknown).
func (b *Bridge) Session(ref string) Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	if info := b.sessions[ref]; info != nil {
		return *info
	}
	return Session{}
}

// Sessions returns every known session snapshot.
func (b *Bridge) Sessions() []Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Session, 0, len(b.sessions))
	for _, info := range b.sessions {
		out = append(out, *info)
	}
	return out
}

// Close closes the bridge, every known session (best effort), the connection
// and the sidecar's process group. It is idempotent and safe after a crash.
func (b *Bridge) Close() error {
	var closeErr error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		refs := make([]string, 0, len(b.sessions))
		for ref := range b.sessions {
			refs = append(refs, ref)
		}
		b.mu.Unlock()
		if len(refs) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			for _, ref := range refs {
				_ = b.client.Close(ctx, gateway.CloseRequest{SessionID: ref, Reason: "oar bridge closed"})
			}
			cancel()
		}
		closeErr = b.conn.Close()
		b.consumer.Wait()
		close(b.done)
	})
	return closeErr
}

// consume decodes gateway events into records, enforces ordering and
// attribution, journals them and forwards them to the live view.
func (b *Bridge) consume() {
	defer b.consumer.Done()
	for event := range b.client.Events() {
		if event.Type == EventTypeStatus {
			b.diagnostic(gateway.Diagnostic{Kind: "oar_status", Detail: truncate(string(event.Data), 512)})
			continue
		}
		if event.Type != EventTypeRecord {
			b.diagnostic(gateway.Diagnostic{Kind: "oar_unknown_event", Detail: "type " + event.Type})
			continue
		}
		var record Record
		if err := json.Unmarshal(event.Data, &record); err != nil {
			b.recordError(&Error{Code: CodeInvalidRecord, Detail: "record is not valid JSON", Err: err})
			continue
		}
		if record.SessionID == "" {
			record.SessionID = event.SessionID
		}
		if event.SessionID != "" && record.SessionID != "" && event.SessionID != record.SessionID {
			b.recordError(&Error{
				Code:   CodeInvalidRecord,
				Detail: fmt.Sprintf("event session %q does not match record session %q", event.SessionID, record.SessionID),
			})
			continue
		}
		b.mu.Lock()
		info := b.sessions[record.SessionID]
		b.mu.Unlock()
		if info == nil {
			b.recordError(&Error{Code: CodeUnknownSession, Detail: fmt.Sprintf("record for unknown session %q", record.SessionID)})
			continue
		}
		if record.Attribution.SessionID == "" {
			record.Attribution.SessionID = info.HostSessionID
		}
		if record.Attribution.TaskID == "" {
			record.Attribution.TaskID = info.TaskID
		}
		if info.HostSessionID != "" && record.Attribution.SessionID != "" && record.Attribution.SessionID != info.HostSessionID {
			b.recordError(&Error{
				Code:   CodeInvalidRecord,
				Detail: fmt.Sprintf("record attribution session %q does not match the opened session %q", record.Attribution.SessionID, info.HostSessionID),
			})
			continue
		}
		if info.TaskID != "" && record.Attribution.TaskID != "" && record.Attribution.TaskID != info.TaskID {
			b.recordError(&Error{
				Code:   CodeInvalidRecord,
				Detail: fmt.Sprintf("record attribution task %q does not match the opened task %q", record.Attribution.TaskID, info.TaskID),
			})
			continue
		}
		if record.Runtime == "" {
			record.Runtime = b.declaration.Runtime
		}
		if err := record.Validate(); err != nil {
			b.recordError(err)
			continue
		}
		duplicate, err := b.tracker.Observe(record.SessionID, record.Seq)
		if err != nil {
			b.recordError(err)
			continue
		}
		if duplicate {
			continue
		}
		if b.opts.Journal != nil {
			if _, err := b.opts.Journal.Append(record); err != nil {
				b.recordError(err)
				continue
			}
		}
		b.mu.Lock()
		if current := b.sessions[record.SessionID]; current != nil && record.Seq > current.LastSeq {
			current.LastSeq = record.Seq
		}
		b.mu.Unlock()
		if b.opts.OnRecord != nil {
			b.opts.OnRecord(record)
		}
		if b.records == nil {
			continue
		}
		select {
		case b.records <- record:
		default:
			b.mu.Lock()
			b.dropped++
			b.mu.Unlock()
		}
	}
}

func (b *Bridge) diagnostic(diagnostic gateway.Diagnostic) {
	if b.opts.OnDiagnostic != nil {
		b.opts.OnDiagnostic(diagnostic)
	}
}

func (b *Bridge) recordError(err error) {
	b.mu.Lock()
	if b.firstErr == nil {
		b.firstErr = err
	}
	b.mu.Unlock()
	select {
	case b.errors <- err:
	default:
	}
	if b.opts.OnDiagnostic != nil {
		b.opts.OnDiagnostic(gateway.Diagnostic{Kind: "oar_record_error", Detail: err.Error()})
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
