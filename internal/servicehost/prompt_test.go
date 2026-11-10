package servicehost

// The plugin prompt seam: the sections a plugin contributes (agentext
// PromptSections, published through RuntimeOptions.Prompt) must reach the
// system prompt the runner assembles, an enable/disable must be effective
// without a restart, and publishing one half of the dynamic surface (tools)
// must never drop the other half (prompt sections) — or vice versa.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/loadout"
)

// pluginStandardText is the content marker the tests assert on: the shape of
// the keel.creation contribution, without depending on its exact full text.
const pluginStandardText = "Creation mode builds plugins by writing real plugin packages."

func promptTestTenant() host.TenantConfig {
	return host.TenantConfig{
		Agent: host.AgentConfig{
			SystemPrompt: "You are the test agent.",
			Sections:     []host.PromptSection{{Name: "tenant_rules", Content: "Tenant rule text."}},
		},
	}
}

// TestRefreshProductPromptPublishesPluginSections is the model-side contract of
// the plugin prompt seam: with no plugin contributing, the prompt is exactly
// the pre-existing assembly (a refresh with an empty set is byte-identical — the
// compatibility line); a published section appears in Runtime.Assembly()
// .SystemPrompt(), which is the string the runner puts into
// agentcore.AgentContext.SystemPrompt; retracting it restores the prompt
// byte-for-byte; and each sink's republish keeps what the other published.
func TestRefreshProductPromptPublishesPluginSections(t *testing.T) {
	rt := bootForTest(t, promptTestTenant(), loadout.Options{})
	baseline := rt.Assembly().SystemPrompt()
	if !strings.Contains(baseline, "You are the test agent.") || !strings.Contains(baseline, "Tenant rule text.") {
		t.Fatalf("baseline prompt = %q", baseline)
	}
	if strings.Contains(baseline, pluginStandardText) {
		t.Fatalf("baseline prompt already carries the plugin section: %q", baseline)
	}

	// No plugins contribute: a refresh with the empty set changes nothing.
	if err := rt.RefreshProductPrompt(nil); err != nil {
		t.Fatal(err)
	}
	if got := rt.Assembly().SystemPrompt(); got != baseline {
		t.Fatalf("an empty refresh changed the prompt:\n got: %q\nwant: %q", got, baseline)
	}

	// Enable: the section reaches the prompt by name and by content.
	if err := rt.RefreshProductPrompt([]host.Section{{Name: "plugin_standard", Content: pluginStandardText}}); err != nil {
		t.Fatal(err)
	}
	prompt := rt.Assembly().SystemPrompt()
	if !strings.Contains(prompt, "<plugin_standard>") || !strings.Contains(prompt, pluginStandardText) {
		t.Fatalf("the plugin section did not reach the assembly prompt: %q", prompt)
	}
	// The prompt service (diagnostics/tests view) and the run assembly are the
	// same prompt: one host, one assembly, no second copy to drift.
	if rt.Host().SystemPrompt() != prompt {
		t.Fatalf("host prompt and assembly prompt diverged:\n host: %q\n asm: %q", rt.Host().SystemPrompt(), prompt)
	}
	// The configured sections survived the republish.
	if !strings.Contains(prompt, "You are the test agent.") || !strings.Contains(prompt, "Tenant rule text.") {
		t.Fatalf("a plugin refresh dropped a configured section: %q", prompt)
	}

	// The other sink (tools) republishing must not drop the prompt sections.
	if err := rt.RefreshProductTools([]agentcore.AgentTool{productStubTool{name: "plugin_tool"}}); err != nil {
		t.Fatal(err)
	}
	prompt = rt.Assembly().SystemPrompt()
	if !strings.Contains(prompt, pluginStandardText) {
		t.Fatalf("a tool refresh dropped the plugin prompt section: %q", prompt)
	}
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); !ok {
		t.Fatal("the plugin tool is not in the host registry")
	}

	// Disable: retracting the section restores the prompt byte-for-byte.
	if err := rt.RefreshProductPrompt(nil); err != nil {
		t.Fatal(err)
	}
	if got := rt.Assembly().SystemPrompt(); got != baseline {
		t.Fatalf("retracting the plugin section did not restore the prompt:\n got: %q\nwant: %q", got, baseline)
	}
	// The prompt refresh kept the tool surface the other sink published.
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); !ok {
		t.Fatal("a prompt refresh dropped the plugin tool")
	}

	// Unloading the tools keeps the prompt at its published state.
	if err := rt.RefreshProductTools(nil); err != nil {
		t.Fatal(err)
	}
	if got := rt.Assembly().SystemPrompt(); got != baseline {
		t.Fatalf("a tool refresh changed the prompt:\n got: %q\nwant: %q", got, baseline)
	}
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); ok {
		t.Fatal("an unloaded plugin tool survived the refresh")
	}
}

// TestRefreshProductPromptConcurrent drives the publication seams and the
// assembly readers together: a plugin toggling while the model's prompt is
// assembled must not panic, tear a section, or lose one (-race is the judge of
// the first; the invariant checks of the rest).
func TestRefreshProductPromptConcurrent(t *testing.T) {
	rt := bootForTest(t, promptTestTenant(), loadout.Options{})
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				prompt := rt.Assembly().SystemPrompt()
				if !strings.Contains(prompt, "You are the test agent.") {
					t.Errorf("torn prompt: %q", prompt)
					return
				}
				// The section is either absent or complete: a refresh publishes
				// the whole section set, never a half-written one.
				if strings.Contains(prompt, "<plugin_standard>") && !strings.Contains(prompt, pluginStandardText) {
					t.Errorf("torn plugin section: %q", prompt)
					return
				}
				_ = rt.Assembly().Tools().List()
			}
		}()
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Writers toggle the plugin on and off while republishing the tool
			// surface: the two sinks race and must compose, not clobber.
			section := host.Section{Name: "plugin_standard", Content: pluginStandardText}
			for n := 0; n < 64; n++ {
				if (n+i)%2 == 0 {
					if err := rt.RefreshProductPrompt([]host.Section{section}); err != nil {
						t.Errorf("refresh prompt: %v", err)
						return
					}
				} else if err := rt.RefreshProductPrompt(nil); err != nil {
					t.Errorf("refresh prompt: %v", err)
					return
				}
				if err := rt.RefreshProductTools([]agentcore.AgentTool{productStubTool{name: "plugin_tool"}}); err != nil {
					t.Errorf("refresh tools: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A second pack of publishers republishing the same set: last writer
			// wins with the complete surface.
			for n := 0; n < 32; n++ {
				if err := rt.RefreshProductPrompt([]host.Section{{Name: "plugin_standard", Content: pluginStandardText}}); err != nil {
					t.Errorf("refresh prompt: %v", err)
					return
				}
				if err := rt.RefreshProductTools([]agentcore.AgentTool{productStubTool{name: "plugin_tool"}}); err != nil {
					t.Errorf("refresh tools: %v", err)
					return
				}
			}
		}()
	}
	close(stop)
	wg.Wait()

	// The final published state is deterministic and complete: no lost section,
	// no lost tool.
	if err := rt.RefreshProductPrompt([]host.Section{{Name: "plugin_standard", Content: pluginStandardText}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.RefreshProductTools([]agentcore.AgentTool{productStubTool{name: "plugin_tool"}}); err != nil {
		t.Fatal(err)
	}
	prompt := rt.Assembly().SystemPrompt()
	if !strings.Contains(prompt, pluginStandardText) {
		t.Fatalf("the plugin section was lost by concurrent publication: %q", prompt)
	}
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); !ok {
		t.Fatal("the plugin tool was lost by concurrent publication")
	}
}

// TestRefreshProductPromptIsIdempotent pins that republishing the same section
// set converges on the same prompt: an unchanged plugin surface never rewrites
// the model's prompt between runs.
func TestRefreshProductPromptIsIdempotent(t *testing.T) {
	rt := bootForTest(t, promptTestTenant(), loadout.Options{})
	section := host.Section{Name: "plugin_standard", Content: pluginStandardText}
	if err := rt.RefreshProductPrompt([]host.Section{section}); err != nil {
		t.Fatal(err)
	}
	first := rt.Assembly().SystemPrompt()
	if err := rt.RefreshProductPrompt([]host.Section{section}); err != nil {
		t.Fatal(err)
	}
	if second := rt.Assembly().SystemPrompt(); second != first {
		t.Fatalf("republishing the same sections changed the prompt:\n got: %q\nwant: %q", second, first)
	}
}

// TestReloadKeepsPluginPromptSurface pins the reload interaction: a model
// settings write reassembles the service graph, and the fresh generation's host
// must receive the published plugin surface again — a reload is invisible to
// the plugin runtime, so nothing else would republish it, and the sections
// would silently vanish from the prompt the model sees.
func TestReloadKeepsPluginPromptSurface(t *testing.T) {
	rt := bootForTest(t, promptTestTenant(), loadout.Options{})
	if err := rt.RefreshProductPrompt([]host.Section{{Name: "plugin_standard", Content: pluginStandardText}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.RefreshProductTools([]agentcore.AgentTool{productStubTool{name: "plugin_tool"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := rt.ApplyModels(context.Background(), nil); err != nil {
		t.Fatalf("reload: %v", err)
	}

	prompt := rt.Assembly().SystemPrompt()
	if !strings.Contains(prompt, pluginStandardText) {
		t.Fatalf("the reload dropped the plugin prompt section: %q", prompt)
	}
	if !strings.Contains(prompt, "Tenant rule text.") {
		t.Fatalf("the reload dropped a configured section: %q", prompt)
	}
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); !ok {
		t.Fatal("the reload dropped the plugin tool")
	}
}
