package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/oar"
)

// Serve runs one OAR sidecar connection on r/w until the peer closes the
// stream, a `close` request asks for shutdown, the context ends, or a simulated
// crash fires. It always tears down the runtime's process group before
// returning.
func Serve(ctx context.Context, r io.Reader, w io.Writer, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	serverCtx, cancel := context.WithCancel(ctx)
	server := &Server{
		cfg:      cfg,
		decl:     cfg.Declaration(),
		ctx:      serverCtx,
		cancel:   cancel,
		read:     gateway.NewFrameReader(r, gateway.DefaultMaxFrameBytes),
		write:    bufio.NewWriterSize(w, 64<<10),
		sessions: make(map[string]*sessionState),
	}
	defer server.shutdown()
	if cfg.SpawnChild {
		if err := server.startChild(); err != nil {
			return err
		}
	}
	if server.cfg.runtime() == RuntimeCodex {
		// Resolve at startup so an unavailable runtime is a clear initialize
		// error, not a message.send surprise. The host preflights too; this is
		// the sidecar's own half.
		if _, err := cfg.resolveCodexPath(); err != nil {
			server.runtimeErr = err
		}
	}
	return server.loop()
}

// Server is one sidecar connection.
type Server struct {
	cfg  Config
	decl oar.Declaration

	ctx    context.Context
	cancel context.CancelFunc

	read  *gateway.FrameReader
	write *bufio.Writer

	writeMu sync.Mutex

	mu          sync.Mutex
	sessions    map[string]*sessionState
	nextSession int64
	nextTurn    int64
	stopping    bool
	runtimeErr  error
	child       *exec.Cmd
	turns       sync.WaitGroup
	shutOnce    sync.Once
}

type sessionState struct {
	mu sync.Mutex

	id            string
	runtime       string
	reference     string
	attribution   oar.Attribution
	workdir       string
	model         string
	sandbox       string
	epoch         time.Time
	deterministic bool

	seq         int64
	records     []oar.Record
	subscribers []*subscriber
	active      *turnState
	closed      bool
}

type subscriber struct {
	ch   chan oar.Record
	done chan struct{}
}

type turnState struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

// turnResult is the driver's terminal report. errorRecorded means the driver
// already emitted a record describing the failure, so runTurn must not add a
// duplicate error record.
type turnResult struct {
	err           error
	errorRecorded bool
}

type rpcError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *rpcError) Error() string { return e.Message }

func (s *Server) loop() error {
	for {
		line, err := s.read.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		message, err := gateway.ParseMessage(line)
		if err != nil {
			_ = s.writeMessage(map[string]any{
				"jsonrpc": gateway.JSONRPCVersion,
				"id":      nil,
				"error":   map[string]any{"code": gateway.CodeParseError, "message": "invalid JSON-RPC message"},
			})
			continue
		}
		switch typed := message.(type) {
		case *gateway.Request:
			result, rpcErr := s.handle(typed.Method, typed.Params)
			if rpcErr != nil {
				if err := s.writeError(typed.ID, rpcErr); err != nil {
					return err
				}
			} else if err := s.writeResult(typed.ID, result); err != nil {
				return err
			}
			s.mu.Lock()
			stopping := s.stopping
			s.mu.Unlock()
			if stopping {
				return nil
			}
		case *gateway.Notification:
			// Request-lifecycle cancellation is not needed: the sidecar answers
			// requests synchronously or starts a turn. Turn cancellation is the
			// agent surface's cancel method.
		}
	}
}

func (s *Server) handle(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case gateway.MethodInitialize:
		return s.initialize()
	case gateway.MethodSessionOpen:
		return s.openSession(params)
	case gateway.MethodSessionResume:
		return s.resumeSession(params)
	case gateway.MethodMessageSend:
		return s.sendMessage(params)
	case gateway.MethodEventStream:
		return s.eventStream(params)
	case gateway.MethodCancel:
		return s.cancelTurn(params)
	case gateway.MethodHealth:
		return s.health()
	case gateway.MethodClose:
		return s.close(params)
	default:
		return nil, &rpcError{Code: gateway.CodeMethodNotFound, Message: "Method not found: " + method}
	}
}

// initialize returns the agent_backend handshake plus the OAR capability
// declaration under capabilities.experimental.oar.
func (s *Server) initialize() (any, *rpcError) {
	if s.runtimeErr != nil {
		return nil, oarRPCError(s.runtimeErr)
	}
	if err := s.decl.Validate(); err != nil {
		return nil, oarRPCError(err)
	}
	usage := s.decl.Has(oar.CapabilityUsage)
	metering := "none"
	if usage {
		metering = "external"
	}
	agentCapability := map[string]any{
		"events":    true,
		"cancel":    s.decl.Has(oar.CapabilityCancel),
		"usage":     usage,
		"streaming": s.decl.Has(oar.CapabilityStreaming),
	}
	return map[string]any{
		"protocolVersion": gateway.ProtocolVersion,
		"capabilities": map[string]any{
			"agent": agentCapability,
			"experimental": map[string]any{
				oar.ExperimentalKey: s.decl,
			},
		},
		"serverInfo": map[string]any{
			"name":    s.cfg.name(),
			"version": s.cfg.version(),
		},
		"provider": map[string]any{
			"type":       string(gateway.ProviderTypeAgentBackend),
			"stateOwner": string(gateway.StateOwnerExternal),
			"recovery":   gateway.RecoveryNone,
			"metering":   metering,
		},
		"instructions": "OAR external runtime. Records are reported out of band; the runtime never creates task cards.",
	}, nil
}

type openParams struct {
	Reference string          `json:"reference"`
	Config    json.RawMessage `json:"config"`
}

type openConfig struct {
	Attribution oar.Attribution `json:"attribution"`
	Workdir     string          `json:"workdir"`
	Model       string          `json:"model"`
	Sandbox     string          `json:"sandbox"`
}

func (s *Server) openSession(params json.RawMessage) (any, *rpcError) {
	var request openParams
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "session.open: invalid params"}
	}
	var config openConfig
	if len(request.Config) > 0 && string(request.Config) != "null" {
		if err := json.Unmarshal(request.Config, &config); err != nil {
			return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "session.open: config must be an object"}
		}
	}
	s.mu.Lock()
	s.nextSession++
	id := fmt.Sprintf("oar-%d", s.nextSession)
	session := &sessionState{
		id:            id,
		runtime:       s.cfg.runtime(),
		reference:     request.Reference,
		attribution:   config.Attribution,
		workdir:       firstNonEmpty(config.Workdir, s.cfg.Codex.Workdir),
		model:         firstNonEmpty(config.Model, s.cfg.Codex.Model),
		sandbox:       firstNonEmpty(config.Sandbox, s.cfg.Codex.CodexSandbox()),
		epoch:         s.cfg.fakeEpoch(),
		deterministic: s.cfg.runtime() == RuntimeFake,
	}
	s.sessions[id] = session
	s.mu.Unlock()
	metering := "none"
	if s.decl.Has(oar.CapabilityUsage) {
		metering = "external"
	}
	return map[string]any{
		"sessionId":  id,
		"reference":  request.Reference,
		"stateOwner": string(gateway.StateOwnerExternal),
		"recovery":   gateway.RecoveryNone,
		"metering":   metering,
		"status":     gateway.ExternalStatusActive,
		"lastSeq":    0,
	}, nil
}

func (s *Server) resumeSession(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "session.resume: sessionId is required"}
	}
	session := s.session(request.SessionID)
	if session == nil || session.isClosed() {
		return map[string]any{
			"sessionId":  request.SessionID,
			"stateOwner": string(gateway.StateOwnerExternal),
			"resumed":    false,
			"status":     gateway.ExternalStatusUnavailable,
		}, nil
	}
	last := session.lastSeq()
	return map[string]any{
		"sessionId":  request.SessionID,
		"stateOwner": string(gateway.StateOwnerExternal),
		"resumed":    true,
		"status":     gateway.ExternalStatusActive,
		"lastSeq":    last,
	}, nil
}

func (s *Server) sendMessage(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string          `json:"sessionId"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "message.send: sessionId is required"}
	}
	session := s.session(request.SessionID)
	if session == nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "message.send: unknown session"}
	}
	s.mu.Lock()
	s.nextTurn++
	turnID := fmt.Sprintf("turn-%d", s.nextTurn)
	s.mu.Unlock()

	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil, &rpcError{Code: gateway.CodeUnavailable, Message: "message.send: session is closed"}
	}
	if session.active != nil {
		session.mu.Unlock()
		return nil, &rpcError{Code: gateway.CodeUnavailable, Message: "message.send: a turn is already running"}
	}
	turnCtx, cancel := context.WithCancel(s.ctx)
	if s.cfg.runtime() == RuntimeCodex {
		// A wedged Codex CLI must become a terminal failed turn, not an
		// unbounded reconnect loop.
		turnCtx, cancel = context.WithTimeout(s.ctx, s.cfg.Codex.CodexTimeout())
	}
	turn := &turnState{id: turnID, cancel: cancel, done: make(chan struct{})}
	session.active = turn
	session.mu.Unlock()

	s.turns.Add(1)
	go func() {
		defer s.turns.Done()
		defer close(turn.done)
		s.runTurn(turnCtx, session, turn, request.Message)
		session.mu.Lock()
		if session.active == turn {
			session.active = nil
		}
		session.mu.Unlock()
	}()
	return map[string]any{"turnId": turnID}, nil
}

func (s *Server) runTurn(ctx context.Context, session *sessionState, turn *turnState, message json.RawMessage) {
	prompt := messageText(message)
	session.emit(oar.Record{
		Kind:    oar.KindStatus,
		Name:    "turn.started",
		Status:  oar.StatusStarted,
		TurnID:  turn.id,
		Message: "turn started",
	})
	var result turnResult
	switch s.cfg.runtime() {
	case RuntimeCodex:
		result = s.runCodexTurn(ctx, session, turn, prompt)
	default:
		result = s.runFakeTurn(ctx, session, turn, prompt)
	}
	status := oar.StatusCompleted
	detail := "turn completed"
	switch {
	case errors.Is(result.err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		status = oar.StatusCancelled
		detail = "turn cancelled"
	case result.err != nil:
		status = oar.StatusFailed
		detail = result.err.Error()
		if !result.errorRecorded {
			session.emit(oar.Record{
				Kind:    oar.KindError,
				Name:    "turn.failed",
				TurnID:  turn.id,
				Error:   &oar.RecordError{Code: oarErrorCode(result.err), Message: result.err.Error()},
				Message: result.err.Error(),
			})
		}
	}
	session.emit(oar.Record{
		Kind:    oar.KindStatus,
		Name:    "turn.finished",
		Status:  status,
		TurnID:  turn.id,
		Message: detail,
	})
}

func (s *Server) eventStream(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
		FromSeq   int64  `json:"fromSeq"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "event.stream: sessionId is required"}
	}
	session := s.session(request.SessionID)
	if session == nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "event.stream: unknown session"}
	}
	if session.isClosed() {
		return nil, &rpcError{Code: gateway.CodeUnavailable, Message: "event.stream: session is closed"}
	}
	sub := session.subscribe(request.FromSeq)
	s.mu.Lock()
	streamID := fmt.Sprintf("stream-%d", s.nextSession)
	s.mu.Unlock()
	go s.pump(session, sub)
	return map[string]any{
		"streamId": streamID,
		"cursor":   session.lastSeq(),
	}, nil
}

// pump writes one subscriber's records as event.stream notifications, in seq
// order. It stops when the session closes or the sidecar shuts down.
func (s *Server) pump(session *sessionState, sub *subscriber) {
	for {
		select {
		case record := <-sub.ch:
			data, err := json.Marshal(record)
			if err != nil {
				continue
			}
			_ = s.writeMessage(map[string]any{
				"jsonrpc": gateway.JSONRPCVersion,
				"method":  gateway.MethodEventStream,
				"params": gateway.AgentEvent{
					SessionID: record.SessionID,
					TurnID:    record.TurnID,
					Sequence:  record.Seq,
					Type:      oar.EventTypeRecord,
					Data:      data,
				},
			})
		case <-sub.done:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Server) cancelTurn(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
		TurnID    string `json:"turnId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "cancel: sessionId is required"}
	}
	session := s.session(request.SessionID)
	if session == nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "cancel: unknown session"}
	}
	session.mu.Lock()
	turn := session.active
	session.mu.Unlock()
	if turn != nil && (request.TurnID == "" || request.TurnID == turn.id) {
		turn.cancel()
	}
	return map[string]any{"cancelled": turn != nil}, nil
}

func (s *Server) health() (any, *rpcError) {
	status := gateway.BackendStatusOK
	detail := "runtime=" + s.cfg.runtime()
	if s.runtimeErr != nil {
		status = gateway.BackendStatusUnavailable
		detail = s.runtimeErr.Error()
	}
	return map[string]any{
		"status":  status,
		"version": s.cfg.version(),
		"detail":  detail,
	}, nil
}

func (s *Server) close(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal(params, &request)
	if request.SessionID == "" {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		return map[string]any{}, nil
	}
	session := s.session(request.SessionID)
	if session == nil {
		return map[string]any{}, nil
	}
	session.mu.Lock()
	session.closed = true
	turn := session.active
	subs := session.subscribers
	session.subscribers = nil
	session.mu.Unlock()
	if turn != nil {
		turn.cancel()
	}
	for _, sub := range subs {
		close(sub.done)
	}
	return map[string]any{}, nil
}

func (s *Server) session(id string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) shutdown() {
	s.shutOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		sessions := make([]*sessionState, 0, len(s.sessions))
		for _, session := range s.sessions {
			sessions = append(sessions, session)
		}
		child := s.child
		s.mu.Unlock()
		for _, session := range sessions {
			session.mu.Lock()
			turn := session.active
			subs := session.subscribers
			session.subscribers = nil
			session.closed = true
			session.mu.Unlock()
			if turn != nil {
				turn.cancel()
			}
			for _, sub := range subs {
				close(sub.done)
			}
		}
		done := make(chan struct{})
		go func() {
			s.turns.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		if child != nil && child.Process != nil {
			_ = terminateProcess(child.Process)
			_ = child.Wait()
		}
		// Belt and braces: a Codex group whose sidecar is being torn down must
		// not survive this process.
		if pid := readPIDFile(s.cfg.CodexPIDFile); pid > 0 {
			_ = gateway.KillTree(pid)
		}
	})
}

func (s *Server) startChild() error {
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		return fmt.Errorf("oar sidecar: spawn test child: %w", err)
	}
	s.mu.Lock()
	s.child = child
	s.mu.Unlock()
	if s.cfg.ChildPIDFile != "" {
		if err := os.WriteFile(s.cfg.ChildPIDFile, []byte(fmt.Sprintf("%d\n", child.Process.Pid)), 0o644); err != nil {
			_ = child.Process.Kill()
			return fmt.Errorf("oar sidecar: write child pid: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "oar-sidecar: spawned child pid=%d\n", child.Process.Pid)
	return nil
}

func (s *Server) writeResult(id gateway.ID, result any) error {
	return s.writeMessage(map[string]any{
		"jsonrpc": gateway.JSONRPCVersion,
		"id":      id,
		"result":  result,
	})
}

func (s *Server) writeError(id gateway.ID, rpcErr *rpcError) error {
	message := map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
	if len(rpcErr.Data) > 0 {
		message["data"] = json.RawMessage(rpcErr.Data)
	}
	return s.writeMessage(map[string]any{
		"jsonrpc": gateway.JSONRPCVersion,
		"id":      id,
		"error":   message,
	})
}

func (s *Server) writeMessage(message any) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.write.Write(raw); err != nil {
		return err
	}
	if err := s.write.WriteByte('\n'); err != nil {
		return err
	}
	return s.write.Flush()
}

// emit assigns seq/timestamp/session/attribution, validates and fans the
// record out to every subscriber in order. It holds the session lock while
// sending; subscriber channels are sized for the replay, so this is bounded.
func (session *sessionState) emit(record oar.Record) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return
	}
	session.seq++
	record.Schema = oar.RecordSchema
	record.Seq = session.seq
	record.SessionID = session.id
	if record.AgentPath == "" {
		record.AgentPath = oar.RootAgentPath
	}
	if record.Attribution.SessionID == "" {
		record.Attribution.SessionID = session.attribution.SessionID
	}
	if record.Attribution.TaskID == "" {
		record.Attribution.TaskID = session.attribution.TaskID
	}
	if record.Timestamp.IsZero() {
		if session.deterministic {
			record.Timestamp = session.epoch.Add(time.Duration(record.Seq) * time.Millisecond).UTC()
		} else {
			record.Timestamp = time.Now().UTC()
		}
	}
	if record.Runtime == "" {
		record.Runtime = session.runtime
	}
	if err := record.Validate(); err != nil {
		// A driver produced a malformed record: drop it with a diagnostic instead
		// of killing the turn. The host would reject it anyway.
		fmt.Fprintf(os.Stderr, "oar-sidecar: drop invalid record: %v\n", err)
		return
	}
	session.records = append(session.records, record)
	for _, sub := range session.subscribers {
		select {
		case sub.ch <- record:
		case <-sub.done:
		}
	}
}

func (session *sessionState) subscribe(fromSeq int64) *subscriber {
	session.mu.Lock()
	defer session.mu.Unlock()
	capacity := len(session.records) + 1024
	sub := &subscriber{ch: make(chan oar.Record, capacity), done: make(chan struct{})}
	session.subscribers = append(session.subscribers, sub)
	for _, record := range session.records {
		if record.Seq > fromSeq {
			sub.ch <- record
		}
	}
	return sub
}

func (session *sessionState) lastSeq() int64 {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.seq
}

func (session *sessionState) isClosed() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.closed
}

// messageText extracts the prompt from the protocol's message value. A string
// is the prompt itself; an object uses its "text" field; anything else is
// passed through as JSON text (never interpreted as an instruction by OAR).
func messageText(message json.RawMessage) string {
	if len(message) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(message, &text); err == nil {
		return text
	}
	var object struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(message, &object); err == nil && object.Text != "" {
		return object.Text
	}
	return string(message)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func oarRPCError(err error) *rpcError {
	var typed *oar.Error
	if errors.As(err, &typed) {
		data, _ := json.Marshal(map[string]any{
			"code":    string(typed.Code),
			"runtime": typed.Runtime,
			"detail":  typed.Detail,
		})
		return &rpcError{Code: gateway.CodeUnavailable, Message: err.Error(), Data: data}
	}
	return &rpcError{Code: gateway.CodeInternalError, Message: err.Error()}
}

func oarErrorCode(err error) string {
	if code := oar.CodeOf(err); code != "" {
		return string(code)
	}
	return "runtime_error"
}

// readPIDFile reads a pid from path (0 when absent or malformed).
func readPIDFile(path string) int {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil {
		return 0
	}
	return pid
}
