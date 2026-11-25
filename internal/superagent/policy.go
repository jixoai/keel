package superagent

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AllowlistPolicy is the built-in first policy. It is intentionally boring:
// authorization comes from a configured actor allowlist, never from event
// text. A source body cannot add an actor, change a repository, raise a
// mandate's strength or stop a ticket unless the actor is already allowed and
// the explicit stop command is used.
type AllowlistPolicy struct {
	// Actors lists allowed actor ids and/or logins. Empty accepts nobody.
	Actors map[string]bool
	// AcceptKinds narrows which event kinds create work. Nil accepts the
	// built-in ticket-intent kinds.
	AcceptKinds map[EventKind]bool
	// WorkerBot is the default worker; an empty value falls back to the
	// orchestrator's configured default.
	WorkerBot string
	// PolicyVersion is recorded on every accepted Ticket.
	PolicyVersion string
	// StopCommand defaults to "/keel stop".
	StopCommand string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Evaluate implements Policy.
func (p *AllowlistPolicy) Evaluate(_ context.Context, tenantID uuid.UUID, event Event) (Verdict, error) {
	if tenantID == uuid.Nil {
		return Verdict{Action: ActionIgnore, Reason: "tenant is required"}, nil
	}
	version := p.PolicyVersion
	if version == "" {
		version = "allowlist-v1"
	}
	allowed := p.allowed(event.Actor)
	if !allowed {
		return Verdict{Action: ActionIgnore, Reason: "actor is not allowed to start work", PolicyVersion: version}, nil
	}
	command := strings.TrimSpace(p.StopCommand)
	if command == "" {
		command = "/keel stop"
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(event.Body)), strings.ToLower(command)) {
		return Verdict{Action: ActionStop, Reason: "explicit stop command from an allowed actor", PolicyVersion: version}, nil
	}
	if event.Kind.IsFollowUp() {
		return Verdict{Action: ActionAttach, Reason: "follow-up on a subscribed thread", PolicyVersion: version}, nil
	}
	if p.AcceptKinds != nil {
		if !p.AcceptKinds[event.Kind] {
			return Verdict{Action: ActionIgnore, Reason: "event kind does not start work", PolicyVersion: version}, nil
		}
	} else if !defaultTicketIntent(event.Kind) {
		return Verdict{Action: ActionIgnore, Reason: "event kind does not start work", PolicyVersion: version}, nil
	}
	return Verdict{
		Action:        ActionAccept,
		Reason:        "allowed actor and ticket intent",
		BotID:         p.WorkerBot,
		Strength:      MandateComment,
		PolicyVersion: version,
	}, nil
}

func defaultTicketIntent(kind EventKind) bool {
	switch kind {
	case EventIssueOpened, EventPullRequestOpen, EventManual:
		return true
	default:
		return false
	}
}

func (p *AllowlistPolicy) allowed(actor Actor) bool {
	if actor.IsBot {
		// A bot account is a source too, but the first policy only trusts the
		// configured humans. A deployment that wants bot-triggered work states it
		// explicitly in its own Policy.
		return false
	}
	if len(p.Actors) == 0 {
		return false
	}
	if actor.ID != "" && p.Actors[actor.ID] {
		return true
	}
	if actor.Login != "" && p.Actors[actor.Login] {
		return true
	}
	return false
}
