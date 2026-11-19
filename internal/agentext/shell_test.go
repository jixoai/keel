package agentext

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/jobqueue"
)

func TestShellPluginContributesOnlyInternalTools(t *testing.T) {
	plugin := NewShellPlugin(ShellOptions{})
	if plugin.ID != ShellPluginID {
		t.Fatalf("id = %q", plugin.ID)
	}
	if len(plugin.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(plugin.Tools))
	}
	for _, tool := range plugin.Tools {
		if !tool.Internal {
			t.Fatalf("tool %q is model-facing; keel.shell tools must stay internal", tool.Name)
		}
	}
	if len(plugin.PromptSections) != 0 {
		t.Fatalf("keel.shell must not contribute a prompt section: %v", plugin.PromptSections)
	}
}

func TestShellBackendWithoutManagerIsPluginState(t *testing.T) {
	_, err := (ShellBackend{}).Invoke(context.Background(), "run", nil)
	if !errors.Is(err, ErrPluginState) {
		t.Fatalf("err = %v, want ErrPluginState", err)
	}
}

func TestShellInvokeWithoutServiceIsPluginState(t *testing.T) {
	_, err := shellInvoke(context.Background(), ShellOptions{}, "run", []byte(`{"command":"ls"}`))
	if !errors.Is(err, ErrPluginState) {
		t.Fatalf("err = %v, want ErrPluginState", err)
	}
}

func TestShellEnvAllowlist(t *testing.T) {
	if err := validateShellEnv(map[string]string{"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}); err != nil {
		t.Fatalf("allowed env rejected: %v", err)
	}
	for _, name := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "IFS", "HOME_ESCAPE"} {
		err := validateShellEnv(map[string]string{name: "x"})
		if err == nil {
			t.Fatalf("env %q was accepted", name)
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("env %q error = %v", name, err)
		}
	}
	tooLarge := map[string]string{"PATH": strings.Repeat("x", shellMaxEnvValue+1)}
	if err := validateShellEnv(tooLarge); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized env error = %v", err)
	}
}

func TestShellRunTextIsBounded(t *testing.T) {
	exit := 0
	jobID := uuid.New()
	result := jobqueue.RunResult{
		JobID:    &jobID,
		Status:   "running",
		TimedOut: true,
		Cwd:      "/workspace",
		Duration: 250 * time.Millisecond,
		ExitCode: &exit,
		Stdout: jobqueue.Output{
			Head:  "HEAD",
			Tail:  "TAIL",
			Bytes: 200_000_000,
		},
		Stderr: jobqueue.Output{Head: "ERR", Bytes: 3},
	}
	text := shellRunText(result)
	if !strings.Contains(text, "continues as background job "+jobID.String()) {
		t.Fatalf("timed-out text does not name the job: %q", text)
	}
	if !strings.Contains(text, `jobId:"`+jobID.String()+`"`) &&
		!strings.Contains(text, "jobId:"+jobID.String()) {
		t.Fatalf("timed-out text does not tell the model how to query the job: %q", text)
	}
	if !strings.Contains(text, "HEAD") || !strings.Contains(text, "TAIL") {
		t.Fatalf("bounded output missing: %q", text)
	}
	if !strings.Contains(text, "[199999992 bytes omitted]") {
		t.Fatalf("omitted byte count missing: %q", text)
	}
	if len(text) > 64<<10 {
		t.Fatalf("text is %d bytes; the model view must stay bounded", len(text))
	}
}

func TestShellJobsListTextIsBoundedAndInformative(t *testing.T) {
	jobs := make([]jobqueue.JobView, 0, 20)
	for i := 0; i < 20; i++ {
		id := uuid.New()
		exit := i
		jobs = append(jobs, jobqueue.JobView{
			JobID:     id,
			Command:   strings.Repeat("long-command ", 100),
			Cwd:       "/workspace",
			Status:    "exited",
			ExitCode:  &exit,
			CreatedAt: time.Now(),
		})
	}
	text := shellJobsListText(jobs)
	if !strings.Contains(text, "20 background job(s)") {
		t.Fatalf("text = %q", text)
	}
	if !strings.Contains(text, jobs[0].JobID.String()) || !strings.Contains(text, "exitCode=0") {
		t.Fatalf("text lost the job facts: %q", text)
	}
	if len(text) > 32<<10 {
		t.Fatalf("list text is %d bytes; it must stay bounded", len(text))
	}
	if shellJobsListText(nil) != "no background jobs" {
		t.Fatalf("empty list text = %q", shellJobsListText(nil))
	}
}

func TestShellErrorShape(t *testing.T) {
	result := shellError(shellCodeInvalidArguments, "bad", map[string]any{"availableOperations": []string{"list"}})
	if result.IsError == nil || !*result.IsError {
		t.Fatal("shellError must set IsError")
	}
	document, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", result.StructuredContent)
	}
	problem := document["error"].(map[string]any)
	if problem["code"] != shellCodeInvalidArguments || problem["tool"] != "bash" {
		t.Fatalf("error document = %#v", problem)
	}
	if text := agentcore.ContentToText(result.Content); !strings.Contains(text, `"code":"invalid_arguments"`) {
		t.Fatalf("text = %q", text)
	}
}
