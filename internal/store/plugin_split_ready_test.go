package store

import "testing"

// TestReadyCoversPluginOwnedSchema pins the boot contract added by the plugin
// split: a reachable database that is missing the plugin platform or superagent
// domain tables is not ready. The table probe is the only place a deployment
// learns that before a plugin reports Active.
func TestReadyCoversPluginOwnedSchema(t *testing.T) {
	want := []string{
		"plugin_state", "plugin_config", "plugin_dependency",
		"superagent_work_items", "superagent_assignments", "superagent_worker_reports",
		"superagent_attentions", "superagent_links", "superagent_subscriptions",
		"superagent_source_cursors", "superagent_source_events", "superagent_outbox",
		"superagent_session_index", "superagent_board_links",
	}
	listed := make(map[string]bool, len(requiredTables))
	for _, table := range requiredTables {
		listed[table] = true
	}
	for _, table := range want {
		if !listed[table] {
			t.Errorf("requiredTables is missing %s", table)
		}
	}
}
