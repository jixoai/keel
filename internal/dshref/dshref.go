// Package dshref is a minimal reference AgentBackend/ToolProvider sidecar: it
// implements the external-gateway protocol over stdio (newline-delimited
// JSON-RPC) so the runtime's external gateway can be exercised end to end
// without a Node/DSH installation.
//
// It is a *reference*, not a DSH substitute: the real DSH sidecar is a Node
// process pinned to @deepseek-ai/dsh and documented in docs/DSH-REFERENCE.md.
// This package exists so the wiring (session marks, event projection, cancel,
// timeout, crash isolation, process reaping) has automated tests and a
// one-command smoke, exactly like the PI conformance suite uses a fake loop.
//
// The sidecar deliberately implements only the protocol's own method set -
// initialize, session.open, session.resume, message.send, event.stream,
// cancel, health, close - and the MCP-shaped tools/* surface for the
// tool_provider kind. It never claims to be an MCP server for agent methods.
package dshref

import (
	"bufio"
	"bytes"
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
)

// Default identity reported in initialize.
const (
	DefaultName    = "dsh-ref"
	DefaultVersion = "0.2.1-rc.5-ref"
	// MaxFrameBytes is the largest frame the reference sidecar accepts. It
	// matches the gateway's default so a test can prove the client's frame cap
	// is enforced on both sides.
	MaxFrameBytes = 16 << 20
)

// ErrForbiddenEnv reports that the process inherited an environment variable a
// spawned provider must never see. The command exits non-zero, which surfaces
// as a handshake failure instead of a silent credential leak.
var ErrForbiddenEnv = errors.New("dshref: forbidden host environment is visible")

// ErrMissingEnv reports that a required variable was not injected.
var ErrMissingEnv = errors.New("dshref: required environment variable is missing")

// ErrSimulatedCrash is returned when the sidecar is configured to die on
// purpose (crash-isolation tests). The command maps it to exit status 2.
var ErrSimulatedCrash = errors.New("dshref: simulated sidecar crash")

// Tool is one tool_provider catalogue entry.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Config tunes the reference sidecar. The zero value is a working agent
// backend named dsh-ref that streams an echo answer and reports no usage
// (metering=none).
type Config struct {
	// Kind is "agent" (default) or "tool".
	Kind string
	// Name and Version are the initialize server identity.
	Name    string
	Version string
	// ProtocolVersion overrides the negotiated revision (version-mismatch
	// tests answer an old revision and must not activate).
	ProtocolVersion string
	// Usage declares capabilities.agent.usage and reports token usage on the
	// final event; false means metering=none and no usage anywhere.
	Usage bool
	// Metering overrides the provider's metering declaration.
	Metering string
	// Recovery overrides the declared recovery mode (default "none").
	Recovery string
	// Slow delays each streamed chunk; cancellation interrupts the wait.
	Slow time.Duration
	// HandshakeDelay delays the initialize answer (a stuck-provider test).
	HandshakeDelay time.Duration
	// OpenDelay delays the session.open answer (a stuck-provider test).
	OpenDelay time.Duration
	// OpenLog appends one line per created session to this file (append mode).
	// It is the observable the single-flight test counts: a concurrent Open of
	// one Keel scope must hand the backend exactly one session.
	OpenLog string
	// IgnoreCancel makes the cancel method a no-op (a stuck-backend test).
	IgnoreCancel bool
	// CrashOnSend terminates the sidecar instead of answering message.send.
	CrashOnSend bool
	// CrashAfterEvents terminates the sidecar after emitting this many events
	// (0 = never).
	CrashAfterEvents int
	// SpawnChild starts a long-lived grandchild so process-group reaping can be
	// asserted end to end. The child's pid is written to stderr.
	SpawnChild bool
	// Prompt prefixes the echoed answer.
	Prompt string
	// Tools is the tool_provider catalogue (kind=tool). Nil uses the default
	// echo/fail pair.
	Tools []Tool
	// ForbiddenEnv lists environment variable names whose presence must abort
	// startup. The runtime's spawn path is expected to pass only the minimal
	// allowlist plus explicitly configured variables.
	ForbiddenEnv []string
	// RequiredEnv lists variables the sidecar needs; a missing one aborts
	// startup (evidence that the adapter's explicit env injection works).
	RequiredEnv []string
}

func (c Config) kind() string {
	if c.Kind == "" {
		return "agent"
	}
	return c.Kind
}

func (c Config) name() string {
	if c.Name == "" {
		return DefaultName
	}
	return c.Name
}

func (c Config) version() string {
	if c.Version == "" {
		return DefaultVersion
	}
	return c.Version
}

func (c Config) protocolVersion() string {
	if c.ProtocolVersion != "" {
		return c.ProtocolVersion
	}
	return gateway.ProtocolVersion
}

func (c Config) recovery() string {
	if c.Recovery == "" {
		return gateway.RecoveryNone
	}
	return c.Recovery
}

// MeteringMark is the metering mark the sidecar declares and reports.
func (c Config) MeteringMark() string {
	if c.Metering != "" {
		return c.Metering
	}
	if c.Usage {
		return string(gateway.MeteringExternal)
	}
	return string(gateway.MeteringNone)
}

type turnState struct {
	id     string
	cancel chan struct{}
	once   sync.Once
}

func (t *turnState) stop() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.cancel) })
}

type sessionState struct {
	mu        sync.Mutex
	id        string
	reference string
	seq       int64
	events    []gateway.AgentEvent
	turn      *turnState
	closed    bool
}

func (s *sessionState) nextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

func (s *sessionState) remember(event gateway.AgentEvent, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	if limit > 0 && len(s.events) > limit {
		s.events = append([]gateway.AgentEvent(nil), s.events[len(s.events)-limit:]...)
	}
}

func (s *sessionState) replay(after int64) []gateway.AgentEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]gateway.AgentEvent, 0, len(s.events))
	for _, event := range s.events {
		if event.Sequence > after {
			out = append(out, event)
		}
	}
	return out
}

// Server is one sidecar connection.
type Server struct {
	cfg   Config
	read  *bufio.Reader
	write *bufio.Writer

	writeMu sync.Mutex

	mu          sync.Mutex
	sessions    map[string]*sessionState
	nextSession int
	nextTurn    int
	stopping    bool

	fatal chan error
}

// Serve runs the sidecar protocol on r/w until the peer closes the stream, the
// close method asks for shutdown, or a simulated crash fires.
func Serve(r io.Reader, w io.Writer, cfg Config) error {
	if err := checkForbiddenEnv(cfg.ForbiddenEnv); err != nil {
		return err
	}
	if err := checkRequiredEnv(cfg.RequiredEnv); err != nil {
		return err
	}
	if cfg.kind() != "agent" && cfg.kind() != "tool" {
		return fmt.Errorf("dshref: unknown kind %q (want agent|tool)", cfg.Kind)
	}
	if cfg.SpawnChild {
		child := exec.Command("sleep", "300")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "dshref: spawn child:", err)
			return err
		}
		fmt.Fprintf(os.Stderr, "dshref: spawned child pid=%d\n", child.Process.Pid)
		// Reap the child when it exits so it never becomes a zombie inside the
		// sidecar; the group kill takes it down first in the reaping test.
		go func() { _ = child.Wait() }()
	}
	s := &Server{
		cfg:      cfg,
		read:     bufio.NewReaderSize(r, 64<<10),
		write:    bufio.NewWriterSize(w, 64<<10),
		sessions: make(map[string]*sessionState),
		fatal:    make(chan error, 1),
	}
	done := make(chan error, 1)
	go func() { done <- s.loop() }()
	select {
	case err := <-done:
		return err
	case err := <-s.fatal:
		return err
	}
}

func checkForbiddenEnv(names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		if _, ok := os.LookupEnv(name); ok {
			fmt.Fprintf(os.Stderr, "dshref: refusing to start: host environment variable %s is visible\n", name)
			return fmt.Errorf("%w: %s", ErrForbiddenEnv, name)
		}
	}
	return nil
}

func checkRequiredEnv(names []string) error {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); !ok || value == "" {
			fmt.Fprintf(os.Stderr, "dshref: required environment variable %s is missing\n", name)
			return fmt.Errorf("%w: %s", ErrMissingEnv, name)
		}
	}
	return nil
}

func (s *Server) crash(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

func (s *Server) wait(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

func (s *Server) loop() error {
	for {
		line, err := s.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		var message struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &message); err != nil {
			// A frame that is not JSON is a bad frame: report it and keep
			// going, exactly like the gateway's own framing contract.
			if err := s.writeMessage(map[string]any{
				"jsonrpc": gateway.JSONRPCVersion,
				"id":      nil,
				"error":   map[string]any{"code": gateway.CodeParseError, "message": "invalid JSON"},
			}); err != nil {
				return err
			}
			continue
		}
		if len(message.ID) == 0 || bytes.Equal(bytes.TrimSpace(message.ID), []byte("null")) {
			s.handleNotification(message.Method, message.Params)
			continue
		}
		result, rpcErr := s.handle(message.Method, message.Params)
		var out map[string]any
		if rpcErr != nil {
			out = map[string]any{
				"jsonrpc": gateway.JSONRPCVersion,
				"id":      message.ID,
				"error":   map[string]any{"code": rpcErr.Code, "message": rpcErr.Message},
			}
		} else {
			out = map[string]any{
				"jsonrpc": gateway.JSONRPCVersion,
				"id":      message.ID,
				"result":  result,
			}
		}
		if err := s.writeMessage(out); err != nil {
			return err
		}
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return nil
		}
	}
}

func (s *Server) readFrame() ([]byte, error) {
	line, err := s.read.ReadBytes('\n')
	if len(line) > MaxFrameBytes {
		return nil, fmt.Errorf("dshref: frame exceeds %d bytes", MaxFrameBytes)
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) > 0 {
			return bytes.TrimSpace(line), nil
		}
		return nil, err
	}
	return bytes.TrimSpace(line), nil
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

// handleNotification handles the MCP request-lifecycle cancellation. The
// reference sidecar answers requests synchronously or starts a turn, so there
// is no in-flight request to abort; the turn-level cancel is the agent method.
func (s *Server) handleNotification(method string, params json.RawMessage) {
	if method == gateway.MethodRequestCancel {
		var payload struct {
			RequestID string `json:"requestId"`
			Reason    string `json:"reason"`
		}
		_ = json.Unmarshal(params, &payload)
	}
}

type rpcError struct {
	Code    int
	Message string
}

func (s *Server) handle(method string, params json.RawMessage) (any, *rpcError) {
	switch s.cfg.kind() {
	case "tool":
		switch method {
		case gateway.MethodInitialize:
			s.wait(s.cfg.HandshakeDelay)
			return s.initializeTool(), nil
		case gateway.MethodToolsList:
			return s.toolsList(), nil
		case gateway.MethodToolsCall:
			return s.toolsCall(params)
		}
	case "agent":
		switch method {
		case gateway.MethodInitialize:
			s.wait(s.cfg.HandshakeDelay)
			return s.initializeAgent(), nil
		case gateway.MethodSessionOpen:
			return s.openSession(params)
		case gateway.MethodSessionResume:
			return s.resumeSession(params)
		case gateway.MethodMessageSend:
			return s.messageSend(params)
		case gateway.MethodEventStream:
			return s.eventStream(params)
		case gateway.MethodCancel:
			return s.cancel(params)
		case gateway.MethodHealth:
			return s.health(), nil
		case gateway.MethodClose:
			return s.close(params)
		}
	}
	return nil, &rpcError{Code: gateway.CodeMethodNotFound, Message: "method not found: " + method}
}

func (s *Server) initializeAgent() map[string]any {
	return map[string]any{
		"protocolVersion": s.cfg.protocolVersion(),
		"capabilities": map[string]any{
			"agent": map[string]any{
				"events": true,
				"cancel": true,
				"usage":  s.cfg.Usage,
			},
		},
		"serverInfo": map[string]any{"name": s.cfg.name(), "version": s.cfg.version()},
		"provider": map[string]any{
			"type":       string(gateway.ProviderTypeAgentBackend),
			"stateOwner": string(gateway.StateOwnerExternal),
			"recovery":   s.cfg.recovery(),
			"metering":   s.cfg.MeteringMark(),
		},
		"instructions": "keel dsh reference sidecar",
	}
}

func (s *Server) initializeTool() map[string]any {
	return map[string]any{
		"protocolVersion": s.cfg.protocolVersion(),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.cfg.name(), "version": s.cfg.version()},
		"provider":        map[string]any{"type": string(gateway.ProviderTypeToolProvider)},
	}
}

func (s *Server) openSession(params json.RawMessage) (any, *rpcError) {
	s.wait(s.cfg.OpenDelay)
	var request struct {
		Reference string          `json:"reference"`
		Config    json.RawMessage `json:"config"`
		Context   json.RawMessage `json:"context"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "invalid session.open params"}
		}
	}
	s.mu.Lock()
	s.nextSession++
	id := fmt.Sprintf("%s-session-%d", s.cfg.name(), s.nextSession)
	session := &sessionState{id: id, reference: request.Reference}
	s.sessions[id] = session
	if s.cfg.OpenLog != "" {
		if file, err := os.OpenFile(s.cfg.OpenLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = fmt.Fprintln(file, id)
			_ = file.Close()
		}
	}
	s.mu.Unlock()
	return map[string]any{
		"sessionId":  id,
		"reference":  request.Reference,
		"stateOwner": string(gateway.StateOwnerExternal),
		"status":     gateway.ExternalStatusActive,
		"metering":   s.cfg.MeteringMark(),
		"recovery":   s.cfg.recovery(),
		"lastSeq":    0,
	}, nil
}

func (s *Server) resumeSession(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
		FromSeq   int64  `json:"fromSeq"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "session.resume requires a sessionId"}
	}
	s.mu.Lock()
	session, ok := s.sessions[request.SessionID]
	s.mu.Unlock()
	if !ok || session == nil {
		return map[string]any{"sessionId": request.SessionID, "resumed": false, "status": gateway.ExternalStatusUnavailable}, nil
	}
	session.mu.Lock()
	lastSeq := session.seq
	session.mu.Unlock()
	return map[string]any{
		"sessionId":  request.SessionID,
		"resumed":    true,
		"status":     gateway.ExternalStatusActive,
		"lastSeq":    lastSeq,
		"metering":   s.cfg.MeteringMark(),
		"recovery":   s.cfg.recovery(),
		"stateOwner": string(gateway.StateOwnerExternal),
	}, nil
}

func (s *Server) messageSend(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string          `json:"sessionId"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "message.send requires a sessionId"}
	}
	s.mu.Lock()
	session, ok := s.sessions[request.SessionID]
	s.mu.Unlock()
	if !ok || session == nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "unknown session " + request.SessionID}
	}
	if s.cfg.CrashOnSend {
		// Die with the request still unanswered: the caller sees a transport
		// failure and the supervisor must mark the backend unavailable.
		s.crash(fmt.Errorf("%w: message.send", ErrSimulatedCrash))
		return nil, &rpcError{Code: gateway.CodeUnavailable, Message: "sidecar is crashing"}
	}
	s.mu.Lock()
	s.nextTurn++
	turnID := fmt.Sprintf("%s-turn-%d", s.cfg.name(), s.nextTurn)
	s.mu.Unlock()
	turn := &turnState{id: turnID, cancel: make(chan struct{})}
	session.mu.Lock()
	previous := session.turn
	session.turn = turn
	text := extractText(request.Message)
	session.mu.Unlock()
	previous.stop()
	go s.runTurn(session, turn, text)
	return map[string]any{"turnId": turnID}, nil
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Text != "" {
		return payload.Text
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain
	}
	return string(raw)
}

func (s *Server) runTurn(session *sessionState, turn *turnState, text string) {
	answer := strings.TrimSpace(s.cfg.Prompt + text)
	if answer == "" {
		answer = "dsh-ref answer"
	}
	emit := func(eventType string, data map[string]any, usage *gateway.Usage) error {
		seq := session.nextSeq()
		event := gateway.AgentEvent{
			SessionID: session.id,
			TurnID:    turn.id,
			Sequence:  seq,
			Type:      eventType,
			Data:      mustJSON(data),
			Usage:     usage,
		}
		session.remember(event, 256)
		if err := s.writeMessage(map[string]any{
			"jsonrpc": gateway.JSONRPCVersion,
			"method":  gateway.MethodEventStream,
			"params":  event,
		}); err != nil {
			return err
		}
		if s.cfg.CrashAfterEvents > 0 && int(seq) >= s.cfg.CrashAfterEvents {
			return fmt.Errorf("%w: after %d events", ErrSimulatedCrash, seq)
		}
		return nil
	}
	if err := emit("turn.started", map[string]any{"turnId": turn.id}, nil); err != nil {
		s.crash(err)
		return
	}
	chunks := splitChunks(answer, 3)
	for i, chunk := range chunks {
		if !s.sleep(turn, s.cfg.Slow) {
			_ = emit("turn.canceled", map[string]any{"turnId": turn.id, "reason": "canceled"}, nil)
			return
		}
		if err := emit("assistant.delta", map[string]any{"turnId": turn.id, "text": chunk, "index": i}, nil); err != nil {
			s.crash(err)
			return
		}
	}
	var usage *gateway.Usage
	if s.cfg.Usage && s.cfg.MeteringMark() == string(gateway.MeteringExternal) {
		usage = &gateway.Usage{
			Metering:     gateway.MeteringExternal,
			Model:        "dsh-ref",
			InputTokens:  3,
			OutputTokens: int64(len(chunks) * 2),
			TotalTokens:  3 + int64(len(chunks)*2),
		}
	}
	if err := emit("assistant.message", map[string]any{"turnId": turn.id, "text": answer}, usage); err != nil {
		s.crash(err)
		return
	}
	if err := emit("turn.completed", map[string]any{"turnId": turn.id}, nil); err != nil {
		s.crash(err)
		return
	}
}

// sleep waits for d, honoring cancellation unless IgnoreCancel is set. It
// returns false when the turn was canceled.
func (s *Server) sleep(turn *turnState, d time.Duration) bool {
	if s.cfg.IgnoreCancel {
		if d > 0 {
			time.Sleep(d)
		}
		return true
	}
	if d <= 0 {
		select {
		case <-turn.cancel:
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-turn.cancel:
		return false
	case <-timer.C:
		return true
	}
}

func (s *Server) eventStream(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
		FromSeq   int64  `json:"fromSeq"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "event.stream requires a sessionId"}
	}
	s.mu.Lock()
	session, ok := s.sessions[request.SessionID]
	s.mu.Unlock()
	if !ok || session == nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "unknown session " + request.SessionID}
	}
	replay := session.replay(request.FromSeq)
	for _, event := range replay {
		if err := s.writeMessage(map[string]any{
			"jsonrpc": gateway.JSONRPCVersion,
			"method":  gateway.MethodEventStream,
			"params":  event,
		}); err != nil {
			return nil, &rpcError{Code: gateway.CodeInternalError, Message: "stream write failed"}
		}
	}
	return map[string]any{"streamId": request.SessionID + "-stream", "cursor": request.FromSeq}, nil
}

func (s *Server) cancel(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
		TurnID    string `json:"turnId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "cancel requires a sessionId"}
	}
	s.mu.Lock()
	session, ok := s.sessions[request.SessionID]
	s.mu.Unlock()
	if !ok || session == nil {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "unknown session " + request.SessionID}
	}
	session.mu.Lock()
	turn := session.turn
	session.mu.Unlock()
	if !s.cfg.IgnoreCancel {
		turn.stop()
	}
	return map[string]any{"canceled": !s.cfg.IgnoreCancel}, nil
}

func (s *Server) health() map[string]any {
	return map[string]any{"status": "ok", "version": s.cfg.version()}
}

func (s *Server) close(params json.RawMessage) (any, *rpcError) {
	var request struct {
		SessionID string `json:"sessionId"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "invalid close params"}
		}
	}
	if request.SessionID == "" {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		return map[string]any{"closed": true, "shutdown": true}, nil
	}
	s.mu.Lock()
	session := s.sessions[request.SessionID]
	delete(s.sessions, request.SessionID)
	s.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		session.closed = true
		turn := session.turn
		session.mu.Unlock()
		turn.stop()
	}
	return map[string]any{"closed": true}, nil
}

func (s *Server) toolsList() map[string]any {
	tools := s.cfg.Tools
	if tools == nil {
		tools = []Tool{
			{
				Name:        "echo",
				Description: "Echo the JSON arguments back as text.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":true}`),
			},
			{
				Name:        "fail",
				Description: "Always answers an isError result.",
				InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
			},
		}
	}
	return map[string]any{"tools": tools}
}

func (s *Server) toolsCall(params json.RawMessage) (any, *rpcError) {
	var request struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &request); err != nil || request.Name == "" {
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "tools/call requires a name"}
	}
	switch request.Name {
	case "echo":
		text := strings.TrimSpace(string(request.Arguments))
		if text == "" {
			text = "{}"
		}
		return map[string]any{
			"content":           []map[string]any{{"type": "text", "text": "echo: " + text}},
			"structuredContent": request.Arguments,
		}, nil
	case "fail":
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "the fail tool always fails"}},
			"isError": true,
		}, nil
	default:
		return nil, &rpcError{Code: gateway.CodeInvalidParams, Message: "unknown tool " + request.Name}
	}
}

func splitChunks(text string, count int) []string {
	if count <= 1 || len(text) == 0 {
		return []string{text}
	}
	size := (len(text) + count - 1) / count
	chunks := make([]string, 0, count)
	for start := 0; start < len(text); start += size {
		end := start + size
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[start:end])
	}
	return chunks
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// ServeCommand is the command entry point: it maps a simulated crash onto a
// non-zero exit so the parent observes a dead sidecar rather than a clean
// shutdown.
func ServeCommand(cfg Config) int {
	err := Serve(os.Stdin, os.Stdout, cfg)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrSimulatedCrash):
		fmt.Fprintln(os.Stderr, "dshref:", err)
		return 2
	case errors.Is(err, ErrForbiddenEnv):
		return 3
	default:
		fmt.Fprintln(os.Stderr, "dshref:", err)
		return 1
	}
}
