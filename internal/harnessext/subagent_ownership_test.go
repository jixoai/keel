package harnessext

import (
	"encoding/json"
	"testing"
)

// TestSpawnedBotIDFollowsChildSelection pins the ownership precedence to the
// execution binding: a non-empty selection is the child's complete selection
// fact and wins over the top-level botId, exactly like childSelection(). A
// team selection keeps the parent's captain ownership.
func TestSpawnedBotIDFollowsChildSelection(t *testing.T) {
	runner := &SubagentRunner{}
	cases := []struct {
		name      string
		botID     string
		selection string
		want      string
	}{
		{name: "top-level only", botID: "member-1", want: "member-1"},
		{name: "selection wins", botID: "member-1", selection: `{"botId":"member-2"}`, want: "member-2"},
		{name: "selection model only", botID: "member-1", selection: `{"modelId":"m"}`, want: ""},
		{name: "team selection", botID: "member-1", selection: `{"teamBotId":"team-1","botId":"member-2"}`, want: ""},
		{name: "null selection", botID: "member-1", selection: `null`, want: "member-1"},
		{name: "empty selection", botID: "member-1", selection: ` `, want: "member-1"},
		{name: "malformed selection", botID: "member-1", selection: `{`, want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			req := SpawnRequest{BotID: test.botID}
			if test.selection != "" {
				req.Selection = json.RawMessage(test.selection)
			}
			if got := runner.spawnedBotID(req); got != test.want {
				t.Fatalf("spawnedBotID = %q, want %q", got, test.want)
			}
		})
	}
}
