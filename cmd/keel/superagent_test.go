package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/superagent"
)

func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestSuperagentConfigDefaultsDisabled(t *testing.T) {
	cfg, err := superagentConfigFromEnv(envMap(nil))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("super-agent must stay disabled without KEEL_SUPERAGENT_SOURCE")
	}
}

func TestSuperagentConfigGitHub(t *testing.T) {
	cfg, err := superagentConfigFromEnv(envMap(map[string]string{
		"KEEL_SUPERAGENT_SOURCE":               superagent.SourceGitHub,
		"KEEL_SUPERAGENT_GITHUB_REPO":          "acme/repo",
		"KEEL_SUPERAGENT_ORCHESTRATOR_BOT":     "orchestrator",
		"KEEL_SUPERAGENT_WORKER_BOT":           "worker",
		"KEEL_SUPERAGENT_ALLOWED_ACTORS":       "alice, bob",
		"KEEL_SUPERAGENT_INTERVAL":             "5s",
		"KEEL_SUPERAGENT_SLOTS":                "3",
		"KEEL_SUPERAGENT_HUMAN_SLOTS":          "2",
		"KEEL_SUPERAGENT_ORCHESTRATOR_SESSION": uuid.New().String(),
	}))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !cfg.Enabled || cfg.Source != superagent.SourceGitHub || cfg.Repo != "acme/repo" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if !cfg.AllowedActors["alice"] || !cfg.AllowedActors["bob"] {
		t.Fatalf("actors = %+v", cfg.AllowedActors)
	}
	if cfg.Interval != 5*time.Second || cfg.Slots != 3 || cfg.HumanSlots != 2 {
		t.Fatalf("limits = %+v", cfg)
	}
	if cfg.OrchestratorSession == uuid.Nil {
		t.Fatal("orchestrator session was not parsed")
	}
}

func TestSuperagentConfigRequiresSourceFields(t *testing.T) {
	cases := []map[string]string{
		{"KEEL_SUPERAGENT_SOURCE": "github"},
		{"KEEL_SUPERAGENT_SOURCE": "github", "KEEL_SUPERAGENT_GITHUB_REPO": "acme/repo"},
		{"KEEL_SUPERAGENT_SOURCE": "github", "KEEL_SUPERAGENT_ORCHESTRATOR_BOT": "o", "KEEL_SUPERAGENT_WORKER_BOT": "w"},
		{"KEEL_SUPERAGENT_SOURCE": "feishu", "KEEL_SUPERAGENT_ORCHESTRATOR_BOT": "o", "KEEL_SUPERAGENT_WORKER_BOT": "w"},
		{"KEEL_SUPERAGENT_SOURCE": "github", "KEEL_SUPERAGENT_GITHUB_REPO": "acme/repo", "KEEL_SUPERAGENT_ORCHESTRATOR_BOT": "o", "KEEL_SUPERAGENT_WORKER_BOT": "w", "KEEL_SUPERAGENT_INTERVAL": "0s"},
	}
	for _, values := range cases {
		if _, err := superagentConfigFromEnv(envMap(values)); err == nil {
			t.Fatalf("config %v should fail", values)
		}
	}
}
