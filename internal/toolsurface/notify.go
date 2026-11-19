package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

var notifyTool = &actionTool{
	name: Notify,
	description: "Push a notification and manage consumers. Actions: push, list, ack. " +
		"The notification service lands in a later wave.",
	schema:  schema(`, "text":{"type":"string"}, "consumer":{"type":"string"}, "notificationId":{"type":"string"}`),
	actions: []string{ActionPush, ActionList, ActionAck},
	run: func(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
		return NotImplemented(Notify, action)
	},
}

func newNotifyTool() *actionTool {
	copied := *notifyTool
	copied.run = notifyTool.run
	return &copied
}
