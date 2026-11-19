package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

var tasksTool = &actionTool{
	name: Tasks,
	description: "The task graph (memory) aggregate tool. Actions: create, update, attach, detach, close, list, show, note. " +
		"Task-log lands in a later wave.",
	schema:  schema(`, "taskId":{"type":"string"}, "title":{"type":"string"}, "status":{"type":"string"}, "parentTaskId":{"type":"string"}, "note":{"type":"string"}, "text":{"type":"string"}`),
	actions: []string{ActionCreate, ActionUpdate, ActionAttach, ActionDetach, ActionClose, ActionList, ActionShow, ActionNote},
	run: func(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
		return NotImplemented(Tasks, action)
	},
}

func newTasksTool() *actionTool {
	copied := *tasksTool
	copied.run = tasksTool.run
	return &copied
}
