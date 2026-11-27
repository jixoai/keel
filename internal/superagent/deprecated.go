package superagent

// The legacy superagent board is frozen (change keel-board-v1, 11.6).
//
// /superagent (web and TUI) and its read-only data sources remain in this
// repository only until Phase 2 15.7 deletes them. They are not an entry point
// for new work: the board's task queue and thread must carry the Ticket,
// answer and completion journey. No compatibility switch, redirect or dual
// source is added here; the deletion is a Phase 2 task, not a feature.
const (
	// DeprecatedBoardRoute is the frozen legacy web/TUI route.
	DeprecatedBoardRoute = "/superagent"
	// LegacyBoardRetirement is the Phase 2 task that removes it.
	LegacyBoardRetirement = "15.7"
)
