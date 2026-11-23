// Package sessiontitle is the one derivation of a session's display title.
//
// A session has no title column and no title event: the title is a projection of
// the session's FIRST user.message text, read from the event ledger. That keeps
// the rule cheap (one lateral read in the list query), deterministic (the same
// transcript always derives the same title), and honest (no state is invented
// for a display fact).
//
// The rule, in order:
//
//   - only the payload's "text" field is read; selection, attachments and any
//     other sibling fact never become a title;
//   - whitespace runs (spaces, tabs, newlines) collapse to one space, so a
//     multi-line prompt stays one line;
//   - the flattened text is trimmed;
//   - text longer than Limit runes is cut to Limit-1 runes plus "…", so a title
//     never exceeds Limit runes.
//
// The title updates only when the first user message changes. Appending later
// messages never moves it: there is no rename state, no LLM call and no second
// write path. A session with no user message derives "" and the client renders
// its own "new session" face.
package sessiontitle

import "strings"

// Limit is the maximum title length in runes, including the ellipsis of a cut
// title.
const Limit = 60

// Derive renders the display title from a session's first user.message text.
// An empty or whitespace-only text has no title.
func Derive(firstUserMessage string) string {
	flat := strings.Join(strings.Fields(firstUserMessage), " ")
	runes := []rune(flat)
	if len(runes) <= Limit {
		return flat
	}
	return string(runes[:Limit-1]) + "…"
}
