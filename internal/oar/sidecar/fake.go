package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jixoai/keel/internal/oar"
)

// runFakeTurn emits the deterministic fake record stream. It is the test
// double for a real runtime: the content is fixed, timestamps are derived from
// the configured epoch and the records exercise every shape the renderer folds
// (call/result on two agent paths, a message and optional usage). It never
// claims a live model.
func (s *Server) runFakeTurn(ctx context.Context, session *sessionState, turn *turnState, prompt string) turnResult {
	childPath := oar.ChildAgentPath(oar.RootAgentPath, "worker-1")
	records := []oar.Record{
		{
			Kind:      oar.KindCall,
			AgentPath: oar.RootAgentPath,
			TurnID:    turn.id,
			CallID:    "call-1",
			Name:      "shell",
			Status:    oar.StatusStarted,
			Input:     mustRaw(map[string]any{"command": "printf OAR-OK"}),
		},
		{
			Kind:      oar.KindResult,
			AgentPath: oar.RootAgentPath,
			TurnID:    turn.id,
			CallID:    "call-1",
			Name:      "shell",
			Status:    oar.StatusCompleted,
			Output:    mustRaw(map[string]any{"stdout": "OAR-OK", "exitCode": 0}),
		},
		{
			Kind:      oar.KindCall,
			AgentPath: childPath,
			TurnID:    turn.id,
			CallID:    "call-2",
			Name:      "read_file",
			Status:    oar.StatusStarted,
			Input:     mustRaw(map[string]any{"path": "README.md"}),
		},
		{
			Kind:      oar.KindResult,
			AgentPath: childPath,
			TurnID:    turn.id,
			CallID:    "call-2",
			Name:      "read_file",
			Status:    oar.StatusCompleted,
			Output:    mustRaw(map[string]any{"bytes": 42}),
		},
		{
			Kind:      oar.KindMessage,
			AgentPath: oar.RootAgentPath,
			TurnID:    turn.id,
			Name:      "agent_message",
			Message:   s.cfg.PromptPrefix + "fake answer to: " + prompt,
		},
	}
	if s.cfg.Usage {
		records = append(records, oar.Record{
			Kind:      oar.KindUsage,
			AgentPath: oar.RootAgentPath,
			TurnID:    turn.id,
			Name:      "turn.usage",
			Usage: &oar.RecordUsage{
				Metering:     "external",
				Model:        "fake-model",
				InputTokens:  12,
				OutputTokens: 5,
				TotalTokens:  17,
			},
		})
	}
	for index, record := range records {
		if err := s.fakeWait(ctx); err != nil {
			return turnResult{err: err}
		}
		session.emit(record)
		if s.cfg.CrashAfter > 0 && index+1 >= s.cfg.CrashAfter {
			// Simulate a sidecar crash: no defers, no process cleanup. The host's
			// process-group shutdown and pid-file reaper are what the test then
			// exercises.
			fmt.Fprintln(os.Stderr, "oar-sidecar: simulated crash")
			time.Sleep(25 * time.Millisecond)
			os.Exit(3)
		}
	}
	return turnResult{}
}

// fakeWait applies StepDelay between fake records, honouring cancellation
// unless IgnoreCancel is set (a cancellation test needs a runtime that ignores
// it).
func (s *Server) fakeWait(ctx context.Context) error {
	if s.cfg.StepDelay <= 0 {
		if s.cfg.IgnoreCancel {
			return nil
		}
		return ctx.Err()
	}
	if s.cfg.IgnoreCancel {
		time.Sleep(s.cfg.StepDelay)
		return nil
	}
	timer := time.NewTimer(s.cfg.StepDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mustRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}
