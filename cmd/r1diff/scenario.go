package main

// v2KindProbe is one negative write probe: the user edge must refuse a v2
// contract kind, and the refusal is recorded as an observable delta when the
// frozen baseline does not know the kind yet.
type v2KindProbe struct {
	Kind    string
	Payload map[string]any
}

// v2KindProbes covers one kind per v2 family (task/fs/config/placement/
// supervision/guardrail/resume/notify). They are written to the probe session
// through the same /events endpoint as the v1 owner-scoped probe, so a refusal
// cannot shift the conversation's seq numbering.
var v2KindProbes = []v2KindProbe{
	{Kind: "task.created", Payload: map[string]any{"taskId": "r1-probe-task", "title": "probe"}},
	{Kind: "fs.write", Payload: map[string]any{"path": "probe.txt", "hash": "sha256:probe", "mode": "100644"}},
	{Kind: "config.rotated", Payload: map[string]any{"configHash": "sha256:probe", "anchorEventSeq": 1}},
	{Kind: "placement.joined", Payload: map[string]any{"groupId": "r1-probe-group", "workerId": "r1-probe-worker"}},
	{Kind: "supervision.escalated", Payload: map[string]any{"from": "supervisor", "to": "parent", "reason": "probe"}},
	{Kind: "guardrail.tripped", Payload: map[string]any{"guardrail": "repeat-tool", "count": 3}},
	{Kind: "resume.attempt", Payload: map[string]any{"attempt": 1, "reason": "probe"}},
	{Kind: "notification.ack", Payload: map[string]any{"notificationId": "r1-probe-notification", "consumer": "client"}},
}
